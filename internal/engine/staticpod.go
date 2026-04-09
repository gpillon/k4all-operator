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

// StaticPodManager writes/removes static pod manifests on target nodes
// via short-lived privileged jobs.
type StaticPodManager struct {
	client     client.Client
	log        logr.Logger
	toolsImage string
}

func NewStaticPodManager(c client.Client, log logr.Logger, toolsImage string) *StaticPodManager {
	return &StaticPodManager{client: c, log: log, toolsImage: toolsImage}
}

func (s *StaticPodManager) Reconcile(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec) error {
	if spec.Image == "" {
		return fmt.Errorf("component %s: no image specified for static-pod", name)
	}

	s.log.Info("reconciling static pod", "component", name, "image", spec.Image)

	manifest := s.generateStaticPodManifest(name, spec)
	return s.writeManifestOnNodes(ctx, name, manifest)
}

func (s *StaticPodManager) Remove(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec) error {
	s.log.Info("removing static pod", "component", name)
	return s.deleteManifestFromNodes(ctx, name)
}

func (s *StaticPodManager) generateStaticPodManifest(name string, spec k4allv1alpha1.ComponentSpec) string {
	ns := spec.Namespace
	if ns == "" {
		ns = "kube-system"
	}

	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/name: %s
    app.kubernetes.io/managed-by: k4all-operator
spec:
  hostNetwork: true
  containers:
  - name: %s
    image: %s
    imagePullPolicy: IfNotPresent
`, name, ns, name, name, spec.Image)
}

func (s *StaticPodManager) writeManifestOnNodes(ctx context.Context, name, manifest string) error {
	escapedManifest := strings.ReplaceAll(manifest, "'", "'\\''")
	script := fmt.Sprintf(`#!/bin/sh
set -e
cat > /manifests/%s.yaml << 'STATICPOD'
%s
STATICPOD
echo "Static pod manifest written to /etc/kubernetes/manifests/%s.yaml"
`, name, escapedManifest, name)

	return s.runNodeJob(ctx, fmt.Sprintf("staticpod-write-%s", name), script)
}

func (s *StaticPodManager) deleteManifestFromNodes(ctx context.Context, name string) error {
	script := fmt.Sprintf(`#!/bin/sh
rm -f /manifests/%s.yaml
echo "Removed static pod manifest %s"
`, name, name)

	return s.runNodeJob(ctx, fmt.Sprintf("staticpod-rm-%s", name), script)
}

func (s *StaticPodManager) runNodeJob(ctx context.Context, jobName, script string) error {
	namespace := "k4all-operator-system"
	jobName = sanitizeJobName(jobName)

	existingJob := &batchv1.Job{}
	if err := s.client.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, existingJob); err == nil {
		propagation := metav1.DeletePropagationForeground
		_ = s.client.Delete(ctx, existingJob, &client.DeleteOptions{
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
				"app.kubernetes.io/managed-by": "k4all-operator",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(1)),
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
							Name:    "worker",
							Image:   s.toolsImage,
							Command: []string{"/bin/sh", "-c", script},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "manifests", MountPath: "/manifests"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{Name: "manifests", VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/etc/kubernetes/manifests",
								Type: &dirOrCreate,
							},
						}},
					},
				},
			},
		},
	}

	if err := s.client.Create(ctx, job); err != nil {
		return fmt.Errorf("create static pod job: %w", err)
	}

	return s.waitForJob(ctx, jobName, namespace, 2*time.Minute)
}

func (s *StaticPodManager) waitForJob(ctx context.Context, name, namespace string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("static pod job %s timed out", name)
		case <-ticker.C:
			job := &batchv1.Job{}
			if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, job); err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return err
			}
			for _, cond := range job.Status.Conditions {
				if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
					return nil
				}
				if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
					return fmt.Errorf("static pod job %s failed: %s", name, cond.Message)
				}
			}
		}
	}
}
