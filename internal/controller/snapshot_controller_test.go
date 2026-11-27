package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/engine"
)

// snapshotControllerDeployment is the snapshot controller's Deployment, as
// the snapshot-controller component of the k4all release ships it.
const snapshotControllerDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: snapshot-controller
  namespace: kube-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: snapshot-controller
  template:
    metadata:
      labels:
        app.kubernetes.io/name: snapshot-controller
    spec:
      containers:
        - name: snapshot-controller
          image: registry.k8s.io/sig-storage/snapshot-controller:v8.7.0
`

// withVolumeSnapshotCRD installs external-snapshotter's VolumeSnapshot CRD
// for the test, and removes it when the test ends.
func withVolumeSnapshotCRD(t *testing.T, c client.Client) {
	t.Helper()
	withTestCRD(t, c, "snapshot.storage.k8s.io_volumesnapshots.yaml", "volumesnapshots.snapshot.storage.k8s.io")
}

// D13: virt and rook both off turn the snapshots gate off. While a
// VolumeSnapshot exists the snapshot controller stays (R8); once none is
// left the reconciler removes it through its hook, which deletes the
// controller and keeps external-snapshotter's CRDs.
func TestReconcile_SnapshotControllerStaysWhileSnapshotsExist(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v8.7.0", FeatureGate: engine.FeatureSnapshots,
		InlineManifests: []string{snapshotControllerDeployment},
	}
	c, r := componentOnEnvtest(t, "snapshot-controller", spec, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v8.7.0", LastTransitionTime: metav1.Now(),
	})
	withVolumeSnapshotCRD(t, c)
	ctx := context.Background()
	for _, obj := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "snapshot-controller"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "snapshot-controller"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "snapshot-controller"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: "snapshot-controller", Image: "registry.k8s.io/sig-storage/snapshot-controller:v8.7.0"}}},
				}}},
	} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
	}
	vs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]interface{}{"name": "rootdisk-snap", "namespace": metav1.NamespaceDefault},
		"spec": map[string]interface{}{
			"source": map[string]interface{}{"persistentVolumeClaimName": "rootdisk"},
		},
	}}
	if err := c.Create(ctx, vs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), vs) })

	reconcileProbe(t, r)

	st := componentStatus(t, c, "snapshot-controller")
	if st.State != k4allv1alpha1.ComponentStateInstalled ||
		!strings.HasPrefix(st.Message, "uninstall refused: snapshot-controller: VolumeSnapshot default/rootdisk-snap exist") {
		t.Fatalf("snapshot-controller %s (%s), want Installed with the refusal", st.State, st.Message)
	}
	deployment := types.NamespacedName{Namespace: "kube-system", Name: "snapshot-controller"}
	if err := c.Get(ctx, deployment, &appsv1.Deployment{}); err != nil {
		t.Fatalf("the snapshot controller: %v, want it kept", err)
	}

	if err := c.Delete(ctx, vs); err != nil {
		t.Fatal(err)
	}
	reconcileProbe(t, r)

	if st := componentStatus(t, c, "snapshot-controller"); st.State != k4allv1alpha1.ComponentStateSkipped {
		t.Fatalf("snapshot-controller %s (%s) once no snapshot is left, want Skipped: uninstalled", st.State, st.Message)
	}
	if err := c.Get(ctx, deployment, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Errorf("the snapshot controller: get = %v, want it removed", err)
	}
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	if err := c.Get(ctx, types.NamespacedName{Name: "volumesnapshots.snapshot.storage.k8s.io"}, crd); err != nil ||
		crd.GetDeletionTimestamp() != nil {
		t.Errorf("the VolumeSnapshot CRD: %v, want it kept", err)
	}
}
