package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/engine"
)

// The KubeVirt and CDI CRs from upstream's releases (testdata/upstream, files
// as published) on envtest, with the CRDs of test/crds.

// upstreamReleases serves testdata/upstream as GitHub serves the release
// assets the k4all release points at: <project>/<version>/<file>.
func upstreamReleases(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir("testdata/upstream")))
	t.Cleanup(srv.Close)
	return srv.URL
}

// gatedCR is a CR whose feature gates the cluster's admins add to.
type gatedCR struct {
	gvk   schema.GroupVersionKind
	key   types.NamespacedName
	gates []string // path to the list
}

var (
	kubeVirtCR = gatedCR{
		gvk:   schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "KubeVirt"},
		key:   types.NamespacedName{Namespace: "kubevirt", Name: "kubevirt"},
		gates: []string{"spec", "configuration", "developerConfiguration", "featureGates"},
	}
	cdiCR = gatedCR{
		gvk:   schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "CDI"},
		key:   types.NamespacedName{Name: "cdi"},
		gates: []string{"spec", "config", "featureGates"},
	}
)

func (g gatedCR) get(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(g.gvk)
	if err := c.Get(context.Background(), g.key, obj); err != nil {
		t.Fatalf("get %s: %v", g.gvk.Kind, err)
	}
	return obj
}

func (g gatedCR) featureGates(t *testing.T, c client.Client) []string {
	t.Helper()
	gates, _, err := unstructured.NestedStringSlice(g.get(t, c).Object, g.gates...)
	if err != nil {
		t.Fatal(err)
	}
	return gates
}

// deleteOnCleanup removes the CR when the test ends: envtest is shared.
func (g gatedCR) deleteOnCleanup(t *testing.T, c client.Client) {
	t.Cleanup(func() {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(g.gvk)
		obj.SetNamespace(g.key.Namespace)
		obj.SetName(g.key.Name)
		_ = c.Delete(context.Background(), obj)
	})
}

// patchByHand merges patch into the CR as `kubectl patch --type merge` does.
func (g gatedCR) patchByHand(t *testing.T, c client.Client, patch string) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(g.gvk)
	obj.SetNamespace(g.key.Namespace)
	obj.SetName(g.key.Name)
	if err := c.Patch(context.Background(), obj, client.RawPatch(types.MergePatchType, []byte(patch)),
		client.FieldOwner("kubectl-patch")); err != nil {
		t.Fatalf("patch %s by hand: %v", g.gvk.Kind, err)
	}
}

func ensureNamespace(t *testing.T, c client.Client, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

// setProbe replaces the "probe" component in the ReleaseManifest, as the
// ReleaseManifest of a new k4all release does.
func setProbe(t *testing.T, c client.Client, spec k4allv1alpha1.ComponentSpec) {
	t.Helper()
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	rm.Spec.Components["probe"] = spec
	if err := c.Update(context.Background(), rm); err != nil {
		t.Fatal(err)
	}
}

// reconcileProbeTo runs one Reconcile and expects "probe" at version, in
// state (Installed, or Upgrading after an intermediate upgradePath step).
func reconcileProbeTo(t *testing.T, c client.Client, r *ReleaseReconciler, version string, state k4allv1alpha1.ComponentState) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "k4all"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if st := probeStatus(t, c); st.State != state || st.InstalledVersion != version {
		t.Fatalf("probe is %s at %q, want %s at %s (%s)", st.State, st.InstalledVersion, state, version, st.Message)
	}
}

// KubeVirt as kubevirt-cr.yaml makes it, the gates and a host device set by
// hand, then an upgrade through upgradePath: every apply keeps both. Before
// this, each apply forced upstream's empty list over the admin's.
func TestReconcile_KubeVirtKeepsTheGatesSetOnTheCluster(t *testing.T) {
	srv := upstreamReleases(t)
	kubevirt := func(version string, path ...k4allv1alpha1.UpgradeStep) k4allv1alpha1.ComponentSpec {
		return k4allv1alpha1.ComponentSpec{
			Type:        k4allv1alpha1.ComponentTypeManifest,
			Version:     version,
			UpgradePath: path,
			Sources:     []string{srv + "/kubevirt/{{ version }}/kubevirt-cr.yaml"},
			Namespace:   "kubevirt",
		}
	}
	c, r := onEnvtest(t, kubevirt("v1.7.4"), k4allv1alpha1.ComponentStatus{})
	ensureNamespace(t, c, "kubevirt")
	kubeVirtCR.deleteOnCleanup(t, c)

	reconcileProbeTo(t, c, r, "v1.7.4", k4allv1alpha1.ComponentStateInstalled)
	if gates := kubeVirtCR.featureGates(t, c); len(gates) != 0 {
		t.Fatalf("fresh install: feature gates %v, want upstream's empty list", gates)
	}

	byHand := []string{"HostDevices", "Snapshot", "DeclarativeHotplugVolumes"}
	kubeVirtCR.patchByHand(t, c, `{"spec":{"configuration":{
		"developerConfiguration":{"featureGates":["HostDevices","Snapshot","DeclarativeHotplugVolumes"]},
		"permittedHostDevices":{"pciHostDevices":[{"pciVendorSelector":"8086:46a6","resourceName":"example.com/iris-xe-vf"}]}}}}`)

	setProbe(t, c, kubevirt("v1.9.1", k4allv1alpha1.UpgradeStep{Version: "v1.8.4"}))
	for _, step := range []struct {
		version string
		state   k4allv1alpha1.ComponentState
	}{
		{"v1.8.4", k4allv1alpha1.ComponentStateUpgrading},
		{"v1.9.1", k4allv1alpha1.ComponentStateInstalled},
	} {
		reconcileProbeTo(t, c, r, step.version, step.state)
		if gates := kubeVirtCR.featureGates(t, c); !slices.Equal(gates, byHand) {
			t.Errorf("after the %s apply: feature gates %v, want %v", step.version, gates, byHand)
		}
		devices, _, _ := unstructured.NestedSlice(kubeVirtCR.get(t, c).Object,
			"spec", "configuration", "permittedHostDevices", "pciHostDevices")
		if len(devices) != 1 {
			t.Errorf("after the %s apply: pciHostDevices %v, want the one set by hand", step.version, devices)
		}
	}
}

// CDI forces a non-empty list: upstream's gates (HonorWaitForFirstConsumer)
// join the admin's at the end, a gate added by hand stays, and one upstream
// stopped listing stays as long as the CR has it.
func TestReconcile_CDIKeepsTheGatesSetOnTheCluster(t *testing.T) {
	srv := upstreamReleases(t)
	cdi := func(version string) k4allv1alpha1.ComponentSpec {
		return k4allv1alpha1.ComponentSpec{
			Type:      k4allv1alpha1.ComponentTypeManifest,
			Version:   version,
			Sources:   []string{srv + "/cdi/{{ version }}/cdi-cr.yaml"},
			Namespace: "cdi", // as in the k4all release; the CDI CR is cluster-scoped
		}
	}
	c, r := onEnvtest(t, cdi("v1.65.0"), k4allv1alpha1.ComponentStatus{})
	cdiCR.deleteOnCleanup(t, c)

	reconcileProbeTo(t, c, r, "v1.65.0", k4allv1alpha1.ComponentStateInstalled)
	if gates, want := cdiCR.featureGates(t, c), []string{"HonorWaitForFirstConsumer", "WebhookPvcRendering"}; !slices.Equal(gates, want) {
		t.Fatalf("fresh install: feature gates %v, want upstream's %v", gates, want)
	}

	// HonorWaitForFirstConsumer removed, DataVolumeClaimAdoption added.
	cdiCR.patchByHand(t, c, `{"spec":{"config":{"featureGates":["WebhookPvcRendering","DataVolumeClaimAdoption"]}}}`)

	setProbe(t, c, cdi("v1.66.1"))
	reconcileProbeTo(t, c, r, "v1.66.1", k4allv1alpha1.ComponentStateInstalled)
	want := []string{"WebhookPvcRendering", "DataVolumeClaimAdoption", "HonorWaitForFirstConsumer"}
	if gates := cdiCR.featureGates(t, c); !slices.Equal(gates, want) {
		t.Errorf("after the v1.66.1 apply: feature gates %v, want %v (the CR's, then upstream's)", gates, want)
	}
}

// A gate set between the operator's read of the CR and its apply is not lost:
// the apply is conditional on the CR it read, and on a conflict it reads
// again.
func TestManifestApply_KeepsAGateSetDuringTheApply(t *testing.T) {
	srv := upstreamReleases(t)
	base, err := client.NewWithWatch(envtestConfig(t), client.Options{Scheme: gateScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ensureNamespace(t, base, "kubevirt")
	kubeVirtCR.deleteOnCleanup(t, base)
	spec := k4allv1alpha1.ComponentSpec{
		Type:      k4allv1alpha1.ComponentTypeManifest,
		Sources:   []string{srv + "/kubevirt/v1.9.1/kubevirt-cr.yaml"},
		Namespace: "kubevirt",
	}
	ctx := context.Background()
	if _, err := engine.NewManifestApplier(base, logr.Discard()).Apply(ctx, "kubevirt", spec, nil); err != nil {
		t.Fatal(err)
	}
	kubeVirtCR.patchByHand(t, base, `{"spec":{"configuration":{"developerConfiguration":{"featureGates":["HostDevices"]}}}}`)

	// Right after the operator's first read of the CR, an admin adds Snapshot.
	raced := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if obj.GetObjectKind().GroupVersionKind().Kind == "KubeVirt" && !raced {
				raced = true
				kubeVirtCR.patchByHand(t, cl, `{"spec":{"configuration":{"developerConfiguration":{"featureGates":["HostDevices","Snapshot"]}}}}`)
			}
			return nil
		},
	})
	if _, err := engine.NewManifestApplier(c, logr.Discard()).Apply(ctx, "kubevirt", spec, nil); err != nil {
		t.Fatal(err)
	}
	if gates, want := kubeVirtCR.featureGates(t, base), []string{"HostDevices", "Snapshot"}; !slices.Equal(gates, want) {
		t.Errorf("feature gates %v, want %v", gates, want)
	}
}
