package engine

import (
	"context"
	goerrors "errors"
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
//
// Deprecated: the manifest it writes has only an image (no args, env or
// volumes), and only on the control-plane node its Job lands on. k4all's one
// static pod, kube-vip, is owned by the host from v2.4.0 (Unmanaged in the
// ReleaseManifest). It never touches a manifest it did not write.
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

	s.log.Info("reconciling static pod (type static-pod is deprecated)", "component", name, "image", spec.Image)

	manifest := s.generateStaticPodManifest(name, spec)
	return s.runNodeJob(ctx, fmt.Sprintf("staticpod-write-%s", name), s.writeScript(name, manifest))
}

// Remove deletes the component's manifest, if the operator wrote it.
func (s *StaticPodManager) Remove(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec) error {
	s.log.Info("removing static pod", "component", name)
	err := s.runNodeJob(ctx, fmt.Sprintf("staticpod-rm-%s", name), s.removeScript(name))
	if goerrors.Is(err, errForeignManifest) {
		s.log.Info("static pod manifest not written by the k4all-operator: left in place", "component", name)
		return nil
	}
	return err
}

// errForeignManifest: the node has a manifest for the component that the
// operator did not write (no managed-by label); it is left alone.
var errForeignManifest = goerrors.New("its static pod manifest was not written by the k4all-operator: left alone")

// staticPodResult reports a component whose manifest the operator did not
// write as Unmanaged with the reason. In the status only: management in the
// ReleaseManifest spec belongs to the user and the host. Not as Failed
// either, which would stop the host's sync.
func staticPodResult(st *k4allv1alpha1.ComponentStatus, name string, err error) error {
	if !goerrors.Is(err, errForeignManifest) {
		return err
	}
	st.State = k4allv1alpha1.ComponentStateUnmanaged
	st.Message = fmt.Sprintf("%s: %v; set management: Unmanaged in the ReleaseManifest", name, err)
	st.LastTransitionTime = metav1.Now()
	return nil
}

// foreignManifestMark is the termination message of a Job that found a
// manifest the operator did not write.
const foreignManifestMark = "foreign-manifest"

// ownManifestLabel marks the manifests the operator writes.
const ownManifestLabel = "app.kubernetes.io/managed-by: k4all-operator"

// guardScript opens the node scripts: they stop, leaving the file and the
// mark, when the component's manifest exists without the operator's label.
// MANIFESTS and TERMINATION_LOG are overridable for tests.
func (s *StaticPodManager) guardScript(name string) string {
	return fmt.Sprintf(`#!/bin/sh
set -e
M="${MANIFESTS:-/manifests}"
F="$M/%s.yaml"
if [ -f "$F" ] && ! grep -q '%s' "$F"; then
  echo "%s" > "${TERMINATION_LOG:-/dev/termination-log}"
  echo "$F was not written by the k4all-operator: left alone"
  exit 0
fi
`, name, ownManifestLabel, foreignManifestMark)
}

func (s *StaticPodManager) writeScript(name, manifest string) string {
	return s.guardScript(name) + fmt.Sprintf(`cat > "$F" << 'STATICPOD'
%s
STATICPOD
echo "Static pod manifest written to /etc/kubernetes/manifests/%s.yaml"
`, manifest, name)
}

func (s *StaticPodManager) removeScript(name string) string {
	return s.guardScript(name) + fmt.Sprintf(`rm -f "$F"
echo "Removed static pod manifest %s"
`, name)
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

	if err := s.waitForJob(ctx, jobName, namespace, 2*time.Minute); err != nil {
		return err
	}
	return s.jobMark(ctx, jobName, namespace)
}

// jobMark returns errForeignManifest when the Job's pod ended with
// foreignManifestMark as its termination message.
func (s *StaticPodManager) jobMark(ctx context.Context, jobName, namespace string) error {
	pods := &corev1.PodList{}
	if err := s.client.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": jobName}); err != nil {
		return fmt.Errorf("read the result of job %s: %w", jobName, err)
	}
	for _, p := range pods.Items {
		for _, c := range p.Status.ContainerStatuses {
			if t := c.State.Terminated; t != nil && strings.TrimSpace(t.Message) == foreignManifestMark {
				return errForeignManifest
			}
		}
	}
	return nil
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
