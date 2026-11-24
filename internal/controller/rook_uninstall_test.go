package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// asRookCeph makes the "probe" component of installedOnEnvtest the
// rook-ceph component, so the reconciler uses the operator's own Rook hook.
func asRookCeph(t *testing.T, c client.Client, spec k4allv1alpha1.ComponentSpec) {
	t.Helper()
	ctx := context.Background()
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(ctx, types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	status := rm.Status.Components["probe"]
	rm.Spec.Components = map[string]k4allv1alpha1.ComponentSpec{"rook-ceph": spec}
	if err := c.Update(ctx, rm); err != nil {
		t.Fatal(err)
	}
	rm.Status.Components = map[string]k4allv1alpha1.ComponentStatus{"rook-ceph": status}
	if err := c.Status().Update(ctx, rm); err != nil {
		t.Fatal(err)
	}
}

func rookCephStatus(t *testing.T, c client.Client) (k4allv1alpha1.ComponentStatus, *metav1.Condition) {
	t.Helper()
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	return rm.Status.Components["rook-ceph"], meta.FindStatusCondition(rm.Status.Conditions, "Ready")
}

// rookOffSpec is rook-ceph with its feature off (the rook gate has no
// ClusterConfig switch yet), as an inline manifest so it applies offline.
var rookOffSpec = k4allv1alpha1.ComponentSpec{
	Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{guardedConfigMap},
	FeatureGate: "rook",
}

// withCephClusterCRD installs Rook's CephCluster CRD for the test and removes
// it when the test ends, waiting until it is gone: the other tests of the
// package run without Rook's CRDs.
func withCephClusterCRD(t *testing.T, c client.Client) {
	t.Helper()
	opts := envtest.CRDInstallOptions{
		Paths:              []string{filepath.Join("hooks", "testdata", "crds", "ceph.rook.io_cephclusters.yaml")},
		ErrorIfPathMissing: true,
	}
	cfg := envtestConfig(t)
	if _, err := envtest.InstallCRDs(cfg, opts); err != nil {
		t.Fatalf("install the CephCluster CRD: %v", err)
	}
	t.Cleanup(func() {
		if err := envtest.UninstallCRDs(cfg, opts); err != nil {
			t.Errorf("uninstall the CephCluster CRD: %v", err)
		}
		crd := &unstructured.Unstructured{}
		crd.SetAPIVersion("apiextensions.k8s.io/v1")
		crd.SetKind("CustomResourceDefinition")
		crd.SetName("cephclusters.ceph.rook.io")
		err := wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(crd), crd)), nil
			})
		if err != nil {
			t.Errorf("the CephCluster CRD is still there: %v", err)
		}
	})
}

// Rook turned off while a CephCluster holds data: rook-ceph stays installed,
// the ReleaseManifest says UninstallRefused and comes back within a minute,
// and once the admin has deleted the CephCluster the next reconcile
// uninstalls it.
func TestReconcile_RookStaysWhileACephClusterExists(t *testing.T) {
	c, r := installedOnEnvtest(t, rookOffSpec, "v1")
	asRookCeph(t, c, rookOffSpec)
	withCephClusterCRD(t, c)
	ctx := context.Background()
	// The object of rook-ceph's manifest, as an earlier apply left it.
	cm := &corev1.ConfigMap{}
	if err := yaml.Unmarshal([]byte(guardedConfigMap), cm); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cm) })
	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "ceph.rook.io/v1",
		"kind":       "CephCluster",
		"metadata":   map[string]interface{}{"name": "rook-ceph", "namespace": metav1.NamespaceDefault},
		"spec": map[string]interface{}{
			"cephVersion":     map[string]interface{}{"image": "quay.io/ceph/ceph:v20.2.4"},
			"dataDirHostPath": "/var/lib/rook",
		},
	}}
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cluster) })

	res := reconcileProbe(t, r)

	st, ready := rookCephStatus(t, c)
	if st.State != k4allv1alpha1.ComponentStateInstalled ||
		!strings.HasPrefix(st.Message, "uninstall refused: rook-ceph: CephCluster default/rook-ceph exists") {
		t.Fatalf("rook-ceph %s (%s), want Installed with the refusal", st.State, st.Message)
	}
	if ready == nil || ready.Status != "False" || ready.Reason != "UninstallRefused" {
		t.Errorf("Ready %+v, want False with reason UninstallRefused", ready)
	}
	if res.RequeueAfter > time.Minute {
		t.Errorf("RequeueAfter %v, want within a minute while the uninstall is refused", res.RequeueAfter)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("rook-ceph's object: %v, want it kept", err)
	}

	if err := c.Delete(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	reconcileProbe(t, r)

	if st, _ := rookCephStatus(t, c); st.State != k4allv1alpha1.ComponentStateSkipped {
		t.Fatalf("rook-ceph %s (%s) once the CephCluster is gone, want Skipped: uninstalled", st.State, st.Message)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("rook-ceph's object: get = %v, want it uninstalled", err)
	}
}

// Without Rook's CRDs no CephCluster can exist: rook-ceph, its feature
// turned off, is uninstalled.
func TestReconcile_RookWithoutItsCRDsIsUninstalled(t *testing.T) {
	c, r := installedOnEnvtest(t, rookOffSpec, "v1")
	asRookCeph(t, c, rookOffSpec)

	reconcileProbe(t, r)

	if st, _ := rookCephStatus(t, c); st.State != k4allv1alpha1.ComponentStateSkipped {
		t.Fatalf("rook-ceph %s (%s), want Skipped: uninstalled", st.State, st.Message)
	}
}
