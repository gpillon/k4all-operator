package controller

import (
	"context"
	"strings"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
