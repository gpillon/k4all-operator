package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const probeConfigMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: upgrade-path-probe
  namespace: default
data:
  probe: "true"
`

// installedOnEnvtest creates a ClusterConfig and a ReleaseManifest whose
// "probe" component (inline manifest, so it applies without network access)
// is recorded as Installed at installedVersion.
func installedOnEnvtest(t *testing.T, spec k4allv1alpha1.ComponentSpec, installedVersion string) (client.Client, *ReleaseReconciler) {
	t.Helper()
	cfg := envtestConfig(t)
	s := gateScheme(t)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	cc := &k4allv1alpha1.ClusterConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all-cluster-config"},
		Spec: k4allv1alpha1.ClusterConfigSpec{
			Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "calico"}},
		},
	}
	rm := &k4allv1alpha1.ReleaseManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Spec:       k4allv1alpha1.ReleaseManifestSpec{Components: map[string]k4allv1alpha1.ComponentSpec{"probe": spec}},
	}
	for _, obj := range []client.Object{cc, rm} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
	}
	rm.Status.Components = map[string]k4allv1alpha1.ComponentStatus{
		"probe": {State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: installedVersion, LastTransitionTime: metav1.Now()},
	}
	if err := c.Status().Update(ctx, rm); err != nil {
		t.Fatal(err)
	}
	return c, &ReleaseReconciler{Client: c, Scheme: s, RestConfig: cfg, Log: logr.Discard()}
}

func probeStatus(t *testing.T, c client.Client) k4allv1alpha1.ComponentStatus {
	t.Helper()
	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "k4all"}, rm); err != nil {
		t.Fatal(err)
	}
	return rm.Status.Components["probe"]
}

func TestReconcile_UpgradePathOneStepPerReconcile(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type:            k4allv1alpha1.ComponentTypeManifest,
		Version:         "v1.16.0",
		InlineManifests: []string{probeConfigMap},
		UpgradePath:     []k4allv1alpha1.UpgradeStep{{Version: "v1.15.0"}},
	}
	c, r := installedOnEnvtest(t, spec, "v1.14.0")
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "k4all"}}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := probeStatus(t, c).InstalledVersion; got != "v1.15.0" {
		t.Fatalf("after the first reconcile: installedVersion = %q, want the intermediate v1.15.0", got)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 30*time.Second {
		t.Errorf("with steps left the reconcile should come back soon, got RequeueAfter %v", res.RequeueAfter)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	st := probeStatus(t, c)
	if st.InstalledVersion != "v1.16.0" || st.State != k4allv1alpha1.ComponentStateInstalled {
		t.Fatalf("after the second reconcile: %s at %q, want Installed at v1.16.0 (%s)", st.State, st.InstalledVersion, st.Message)
	}
}

// A failed step must not lose the version still running, or the next attempt
// would skip the rest of the path and jump straight to the target.
func TestReconcile_FailedUpgradeStepKeepsInstalledVersion(t *testing.T) {
	cl := newCluster(t,
		map[string]k4allv1alpha1.ComponentSpec{"probe": {
			Type:        "unsupported",
			Version:     "v1.16.0",
			UpgradePath: []k4allv1alpha1.UpgradeStep{{Version: "v1.15.0"}},
		}},
		map[string]k4allv1alpha1.ComponentStatus{"probe": {State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v1.14.0"}},
	)

	cl.reconcile()

	st := cl.manifest().Status.Components["probe"]
	if st.State != k4allv1alpha1.ComponentStateFailed {
		t.Fatalf("state = %q, want Failed", st.State)
	}
	if st.InstalledVersion != "v1.14.0" {
		t.Errorf("installedVersion = %q after a failed step, want v1.14.0 (still running)", st.InstalledVersion)
	}
	if !strings.Contains(st.Message, "v1.15.0") {
		t.Errorf("message %q should name the failed step v1.15.0", st.Message)
	}
}
