package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

// k4all#77: a ReleaseManifest that pins a component lower than the version
// installed (a hand edit, `sync-manifest` from an older image) does not take
// it down, unless the ReleaseManifest names that downgrade.

// downgradeProbe is the ConfigMap the probe component applies here; no
// other test applies it.
const downgradeProbe = `apiVersion: v1
kind: ConfigMap
metadata:
  name: downgrade-probe
  namespace: default
data:
  probe: "true"
`

// The probe runs probeInstalled; the ReleaseManifest pins the older
// probePinned.
const probeInstalled, probePinned = "v1.16.0", "v1.14.0"

// gateOff is a feature gate the test ClusterConfigs leave off.
const gateOff = "rook"

func probeDowngrade(t *testing.T) (client.Client, *ReleaseReconciler) {
	t.Helper()
	c, r := installedOnEnvtest(t, k4allv1alpha1.ComponentSpec{
		Type:            k4allv1alpha1.ComponentTypeManifest,
		Version:         probePinned,
		InlineManifests: []string{downgradeProbe},
	}, probeInstalled)
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "downgrade-probe", Namespace: "default"}})
	})
	return c, r
}

func reconcileK4all(t *testing.T, r *ReleaseReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "k4all"}}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcile_RefusesToDowngradeAnInstalledComponent(t *testing.T) {
	c, r := probeDowngrade(t)

	reconcileK4all(t, r)

	st := probeStatus(t, c)
	if st.State != k4allv1alpha1.ComponentStateFailed || st.InstalledVersion != probeInstalled {
		t.Errorf("probe = %s at %q, want Failed, still at v1.16.0", st.State, st.InstalledVersion)
	}
	if !strings.Contains(st.Message, "not downgrading") || !strings.Contains(st.Message, AllowDowngradeAnnotation+": probe="+probePinned) {
		t.Errorf("message = %q, want the refusal and how to allow it", st.Message)
	}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "downgrade-probe"}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the older version was applied (get ConfigMap: %v)", err)
	}
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	refused := meta.FindStatusCondition(rm.Status.Conditions, "DowngradeRefused")
	if refused == nil || !strings.Contains(refused.Message, "probe") {
		t.Errorf("DowngradeRefused = %+v, want it naming probe", refused)
	}
}

func TestReconcile_DowngradesWhenTheReleaseManifestAllowsIt(t *testing.T) {
	c, r := probeDowngrade(t)
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	rm.Annotations = map[string]string{AllowDowngradeAnnotation: "other=v9.0.0, probe=1.14.0"}
	if err := c.Update(context.Background(), rm); err != nil {
		t.Fatal(err)
	}

	reconcileK4all(t, r)

	if st := probeStatus(t, c); st.State != k4allv1alpha1.ComponentStateInstalled || st.InstalledVersion != probePinned {
		t.Errorf("probe = %s at %q (%s), want Installed at the pinned version", st.State, st.InstalledVersion, st.Message)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	if meta.IsStatusConditionTrue(rm.Status.Conditions, "DowngradeRefused") {
		t.Errorf("DowngradeRefused still True: %+v", rm.Status.Conditions)
	}
}

// preparerProbe is the hook of the "probe" component: an UninstallPreparer
// whose operator still uninstalls what it created while pending is set.
type preparerProbe struct {
	hooks.BaseHook
	pending string
}

func (p *preparerProbe) PrepareUninstall(context.Context, string) (string, error) {
	return p.pending, nil
}

// A component back in the release while its operator uninstalls it (KubeVirt
// after virt was off) waits for that, then installs afresh: the version it
// had is gone, so a lower pin is not a downgrade.
func TestReconcile_AComponentBeingUninstalledIsNotADowngrade(t *testing.T) {
	c, r := onEnvtest(t, k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: probePinned, InlineManifests: []string{downgradeProbe},
	}, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateUninstalling, InstalledVersion: probeInstalled, LastTransitionTime: metav1.Now(),
	})
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "downgrade-probe", Namespace: "default"}})
	})
	preparer := &preparerProbe{pending: "KubeVirt kubevirt/kubevirt is being deleted"}
	r.hookReg = hooks.NewRegistry(c, logr.Discard())
	r.hookReg.Register("probe", preparer)
	manifest := func() *k4allv1alpha1.ReleaseManifest {
		rm := &k4allv1alpha1.ReleaseManifest{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
			t.Fatal(err)
		}
		return rm
	}

	reconcileK4all(t, r)

	if st := probeStatus(t, c); st.State != k4allv1alpha1.ComponentStateUninstalling || strings.Contains(st.Message, "not downgrading") {
		t.Errorf("probe = %s (%s), want still Uninstalling", st.State, st.Message)
	}
	if cond := meta.FindStatusCondition(manifest().Status.Conditions, "DowngradeRefused"); cond != nil {
		t.Errorf("DowngradeRefused = %+v while the component is uninstalled, want none", cond)
	}

	preparer.pending = ""
	reconcileK4all(t, r)

	if st := probeStatus(t, c); st.State != k4allv1alpha1.ComponentStateInstalled || st.InstalledVersion != probePinned {
		t.Errorf("probe = %s at %q (%s), want installed afresh at the pinned version", st.State, st.InstalledVersion, st.Message)
	}
	if cond := meta.FindStatusCondition(manifest().Status.Conditions, "DowngradeRefused"); cond != nil {
		t.Errorf("DowngradeRefused = %+v, want none", cond)
	}
}

// A component left at its version by a refused downgrade still runs: when
// its feature goes off, it is uninstalled like any installed component.
func TestReconcile_ARefusedDowngradeIsUninstalledWhenItsFeatureGoesOff(t *testing.T) {
	c, r := probeDowngrade(t)
	ctx := context.Background()
	running := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "downgrade-probe", Namespace: "default"}}
	if err := c.Create(ctx, running); err != nil { // what v1.16.0 installed
		t.Fatal(err)
	}
	reconcileK4all(t, r)
	if st := probeStatus(t, c); !strings.HasPrefix(st.Message, "not downgrading") {
		t.Fatalf("probe = %s (%s), want the downgrade refused first", st.State, st.Message)
	}

	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(ctx, types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	spec := rm.Spec.Components["probe"]
	spec.FeatureGate = gateOff
	rm.Spec.Components["probe"] = spec
	if err := c.Update(ctx, rm); err != nil {
		t.Fatal(err)
	}
	reconcileK4all(t, r)

	if err := c.Get(ctx, client.ObjectKeyFromObject(running), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("the running version's object: get = %v, want it uninstalled", err)
	}
}
