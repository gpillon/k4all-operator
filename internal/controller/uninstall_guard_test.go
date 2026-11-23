package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

// guardProbe is the hook of the "probe" component: an UninstallGuard that
// refuses with reason while it is set, and fails with err.
type guardProbe struct {
	hooks.BaseHook
	reason string
	err    error
}

func (g *guardProbe) UninstallBlocked(context.Context, string) (string, error) {
	return g.reason, g.err
}

const guardedConfigMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: guarded
  namespace: default
`

// A component whose hook refuses its uninstall stays installed when it goes
// away from the release, whichever way: Rook while a CephCluster holds data.
// Its status says why, and the uninstall happens at the first reconcile
// after the hook lets it.
func TestReconcile_UninstallWaitsForTheHookToAllowIt(t *testing.T) {
	for name, goAway := range map[string]func(*k4allv1alpha1.ComponentSpec){
		"its feature is off":    func(s *k4allv1alpha1.ComponentSpec) { s.FeatureGate = "rook" },
		"management is Removed": func(s *k4allv1alpha1.ComponentSpec) { s.Management = k4allv1alpha1.ComponentManagementRemoved },
	} {
		t.Run(name, func(t *testing.T) {
			spec := k4allv1alpha1.ComponentSpec{
				Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{guardedConfigMap},
			}
			goAway(&spec)
			c, r := installedOnEnvtest(t, spec, "v1")
			ctx := context.Background()
			cm := &corev1.ConfigMap{}
			cm.Name, cm.Namespace = "guarded", "default"
			if err := c.Create(ctx, cm); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Delete(context.Background(), cm) })
			guard := &guardProbe{reason: "CephCluster rook-ceph/rook-ceph holds data"}
			r.hookReg = hooks.NewRegistry(c, logr.Discard())
			r.hookReg.Register("probe", guard)

			reconcileProbe(t, r)

			if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); err != nil {
				t.Fatalf("the component's object: %v, want it kept", err)
			}
			st := probeStatus(t, c)
			if st.State != k4allv1alpha1.ComponentStateInstalled || st.InstalledVersion != "v1" ||
				!strings.Contains(st.Message, "uninstall refused: CephCluster rook-ceph/rook-ceph holds data") {
				t.Fatalf("status %s at %q (%s), want Installed at v1 with the refusal", st.State, st.InstalledVersion, st.Message)
			}
			rm := &k4allv1alpha1.ReleaseManifest{}
			if err := c.Get(ctx, types.NamespacedName{Name: "k4all"}, rm); err != nil {
				t.Fatal(err)
			}
			if meta.IsStatusConditionTrue(rm.Status.Conditions, "Ready") {
				t.Error("Ready is True with an uninstall refused")
			}

			guard.reason = ""
			reconcileProbe(t, r)

			if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
				t.Fatalf("the component's object: get = %v, want it uninstalled", err)
			}
			if st := probeStatus(t, c); st.State == k4allv1alpha1.ComponentStateInstalled {
				t.Fatalf("status %s (%s) once the hook allows the uninstall", st.State, st.Message)
			}
		})
	}
}

// A guard that cannot tell (the API server did not answer) refuses: an
// uninstall cannot be undone.
func TestReconcile_UninstallIsRefusedWhenTheHookCannotTell(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{guardedConfigMap},
		FeatureGate: "rook",
	}
	c, r := installedOnEnvtest(t, spec, "v1")
	r.hookReg = hooks.NewRegistry(c, logr.Discard())
	r.hookReg.Register("probe", &guardProbe{err: errors.New("list CephClusters: timeout")})

	reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.State != k4allv1alpha1.ComponentStateInstalled ||
		!strings.Contains(st.Message, "uninstall refused: cannot tell whether probe may be uninstalled: list CephClusters: timeout") {
		t.Fatalf("status %s (%s), want Installed with the refusal", st.State, st.Message)
	}
}

// A component whose uninstall was refused and that comes back to the release
// (its feature on again, management Managed) drops the refusal from its
// status: nothing is refused any more, and the refusal must not read as a
// reason to wait for the user (k4all-upgrade settles on it).
func TestReconcile_AComponentBackInTheReleaseDropsTheRefusal(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1", InlineManifests: []string{guardedConfigMap},
		Management: k4allv1alpha1.ComponentManagementRemoved,
	}
	c, r := installedOnEnvtest(t, spec, "v1")
	ctx := context.Background()
	cm := &corev1.ConfigMap{}
	cm.Name, cm.Namespace = "guarded", "default"
	if err := c.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), cm) })
	r.hookReg = hooks.NewRegistry(c, logr.Discard())
	r.hookReg.Register("probe", &guardProbe{reason: "CephCluster rook-ceph/rook-ceph holds data"})

	reconcileProbe(t, r)
	if st := probeStatus(t, c); !strings.HasPrefix(st.Message, "uninstall refused: ") {
		t.Fatalf("status %s (%s), want the refusal", st.State, st.Message)
	}

	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(ctx, types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	back := rm.Spec.Components["probe"]
	back.Management = k4allv1alpha1.ComponentManagementManaged
	rm.Spec.Components["probe"] = back
	if err := c.Update(ctx, rm); err != nil {
		t.Fatal(err)
	}
	reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.State != k4allv1alpha1.ComponentStateInstalled || strings.HasPrefix(st.Message, "uninstall refused: ") {
		t.Fatalf("status %s (%s), want Installed without the refusal", st.State, st.Message)
	}
}
