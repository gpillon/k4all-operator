package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BinaryInstaller downloads and installs binaries on target nodes
// using short-lived privileged Jobs.
type BinaryInstaller struct {
	client     client.Client
	log        logr.Logger
	toolsImage string
}

func NewBinaryInstaller(c client.Client, log logr.Logger, toolsImage string) *BinaryInstaller {
	return &BinaryInstaller{client: c, log: log, toolsImage: toolsImage}
}

func (b *BinaryInstaller) Install(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec) error {
	if spec.Source == "" {
		return fmt.Errorf("component %s: no source URL for binary", name)
	}

	installPath := spec.InstallPath
	if installPath == "" {
		installPath = "/usr/local/bin"
	}

	b.log.Info("installing binary", "component", name, "source", spec.Source, "installPath", installPath)

	// Determine binary name from source URL or component name
	binaryName := name
	if strings.HasSuffix(spec.Source, ".tar.gz") || strings.HasSuffix(spec.Source, ".tgz") {
		return b.installTarball(ctx, name, spec, installPath)
	}
	return b.installSingleBinary(ctx, name, binaryName, spec, installPath)
}

func (b *BinaryInstaller) installSingleBinary(ctx context.Context, name, binaryName string, spec k4allv1alpha1.ComponentSpec, installPath string) error {
	script := fmt.Sprintf(`#!/bin/sh
set -e
echo "Downloading %s from %s"
mkdir -p /target
curl -sSL -o /target/%s "%s"
chmod +x /target/%s
echo "Installed %s to %s/%s"
`, name, spec.Source,
		binaryName, spec.Source,
		binaryName,
		name, installPath, binaryName)

	return b.runNodeJob(ctx, name, installPath, script)
}

func (b *BinaryInstaller) installTarball(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, installPath string) error {
	script := fmt.Sprintf(`#!/bin/sh
set -e
TMPDIR=$(mktemp -d)
echo "Downloading %s tarball from %s"
curl -sSL "%s" | tar xz -C "$TMPDIR"
mkdir -p /target
cp -f "$TMPDIR"/* /target/ 2>/dev/null || true
chmod +x /target/*
rm -rf "$TMPDIR"
echo "Installed %s to %s"
`, name, spec.Source,
		spec.Source,
		name, installPath)

	return b.runNodeJob(ctx, name, installPath, script)
}

func (b *BinaryInstaller) runNodeJob(ctx context.Context, name, hostPath, script string) error {
	namespace := "k4all-operator-system"
	jobName := fmt.Sprintf("k4all-binary-%s", sanitizeJobName(name))

	existingJob := &batchv1.Job{}
	if err := b.client.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, existingJob); err == nil {
		propagation := metav1.DeletePropagationForeground
		_ = b.client.Delete(ctx, existingJob, &client.DeleteOptions{
			PropagationPolicy: &propagation,
		})
		time.Sleep(2 * time.Second)
	}

	privileged := true
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":  "k4all-operator",
				"k4all.magesgate.com/component": name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(2)),
			TTLSecondsAfterFinished: ptr.To(int32(300)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					HostNetwork:   true,
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					NodeSelector: map[string]string{
						"node-role.kubernetes.io/control-plane": "",
					},
					Containers: []corev1.Container{
						{
							Name:    "installer",
							Image:   b.toolsImage,
							Command: []string{"/bin/sh", "-c", script},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "target-dir", MountPath: "/target"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{Name: "target-dir", VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{Path: hostPath, Type: &dirOrCreate},
						}},
					},
				},
			},
		},
	}

	if err := b.client.Create(ctx, job); err != nil {
		return fmt.Errorf("create binary install job: %w", err)
	}

	return b.waitForJob(ctx, jobName, namespace, 5*time.Minute)
}

func (b *BinaryInstaller) waitForJob(ctx context.Context, name, namespace string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("job %s/%s timed out after %v", namespace, name, timeout)
		case <-ticker.C:
			job := &batchv1.Job{}
			if err := b.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, job); err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return err
			}

			for _, cond := range job.Status.Conditions {
				if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
					b.log.Info("binary install job completed", "name", name)
					return nil
				}
				if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
					return fmt.Errorf("binary install job %s failed: %s", name, cond.Message)
				}
			}
		}
	}
}

func sanitizeJobName(name string) string {
	return strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(name))
}
