package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// cluster is a fake API server holding one ReleaseManifest and one
// ClusterConfig, reconciled as many times as a test needs.
type cluster struct {
	t *testing.T
	c client.Client
	r *ReleaseReconciler
}

func newCluster(t *testing.T, components map[string]k4allv1alpha1.ComponentSpec, status map[string]k4allv1alpha1.ComponentStatus) *cluster {
	t.Helper()
	s := gateScheme(t)
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec:       k4allv1alpha1.ReleaseManifestSpec{Components: components},
		Status:     k4allv1alpha1.ReleaseManifestStatus{Components: status},
	}
	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(rm, cc).
		WithStatusSubresource(&k4allv1alpha1.ReleaseManifest{}, &k4allv1alpha1.ClusterConfig{}).
		Build()
	return &cluster{t: t, c: c, r: &ReleaseReconciler{Client: c, Scheme: s, Log: logr.Discard()}}
}

func (cl *cluster) reconcile() ctrl.Result {
	cl.t.Helper()
	res, err := cl.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "k4all"}})
	if err != nil {
		cl.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (cl *cluster) manifest() *k4allv1alpha1.ReleaseManifest {
	cl.t.Helper()
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := cl.c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
		cl.t.Fatal(err)
	}
	return rm
}

func (cl *cluster) updateSpec(name string, mutate func(*k4allv1alpha1.ComponentSpec)) {
	cl.t.Helper()
	rm := cl.manifest()
	spec := rm.Spec.Components[name]
	mutate(&spec)
	rm.Spec.Components[name] = spec
	if err := cl.c.Update(context.Background(), rm); err != nil {
		cl.t.Fatal(err)
	}
}

func (cl *cluster) state(name string) k4allv1alpha1.ComponentState {
	cl.t.Helper()
	return cl.manifest().Status.Components[name].State
}

// "probe" uses an unsupported type: applying it fails at once without network
// access, so a Failed state means the operator tried to apply it again.
func TestReconcile_SpecChangeAtSameVersionIsReapplied(t *testing.T) {
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {Type: "unsupported", Version: "1"}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "1"}},
	)

	cl.reconcile()
	if st := cl.state("probe"); st != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("after adopting an installed component: state = %q, want Installed (no reinstall on operator upgrade)", st)
	}
	if cl.manifest().Status.Components["probe"].AppliedHash == "" {
		t.Fatalf("adopted component has no appliedHash recorded")
	}

	cl.reconcile()
	if st := cl.state("probe"); st != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("unchanged spec: state = %q, want Installed", st)
	}

	cl.updateSpec("probe", func(s *k4allv1alpha1.ComponentSpec) {
		s.InlineManifests = []string{"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: probe\n"}
	})
	cl.reconcile()
	if st := cl.state("probe"); st != k4allv1alpha1.ComponentStateFailed {
		t.Fatalf("spec changed at the same version: state = %q, want Failed (i.e. applied again)", st)
	}
}

// oldAppliedHash is the hash v0.5.2 and earlier recorded: no hook revision.
func oldAppliedHash(t *testing.T, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) string {
	t.Helper()
	hookConfig := config
	hookConfig.ComponentOverrides = nil
	inputs := struct {
		Spec     k4allv1alpha1.ComponentSpec      `json:"spec"`
		Override *k4allv1alpha1.ComponentOverride `json:"override,omitempty"`
		Config   *k4allv1alpha1.ClusterConfigSpec `json:"config,omitempty"`
	}{Spec: spec, Config: &hookConfig}
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// A hook with a values revision (cilium) changes the hash an older operator
// recorded, so the component is applied again after the operator upgrade;
// hooks without one (topolvm) keep their hash and are left alone.
func TestAppliedHash_HookRevisionReappliesOnlyThatComponent(t *testing.T) {
	r := &ReleaseReconciler{Log: logr.Discard()}
	r.hookReg = hooks.NewRegistry(nil, logr.Discard())
	var config k4allv1alpha1.ClusterConfigSpec
	config.Networking.CNI.Type = "cilium"
	config.Features.Virt.Enabled = true
	spec := k4allv1alpha1.ComponentSpec{Type: "helm", Version: "1.19.8"}

	if r.appliedHash("cilium", spec, config, nil) == oldAppliedHash(t, spec, config) {
		t.Errorf("cilium: the hash ignores the hook revision, running clusters would keep the old Cilium values")
	}
	if r.appliedHash("topolvm", spec, config, nil) != oldAppliedHash(t, spec, config) {
		t.Errorf("topolvm: the hash changed without a hook revision, every cluster would re-apply it")
	}
}
