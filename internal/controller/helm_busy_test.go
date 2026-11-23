package controller

import (
	"strings"
	"testing"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	helmtime "helm.sh/helm/v3/pkg/time"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
)

// storeHelmRelease stores one revision of a release in the cluster, as Helm
// would.
func storeHelmRelease(t *testing.T, cfg *rest.Config, namespace, name string, rel *release.Release) {
	t.Helper()
	flags := genericclioptions.NewConfigFlags(false)
	flags.WrapConfigFn = func(*rest.Config) *rest.Config { return cfg }
	flags.Namespace = &namespace
	helmCfg := new(action.Configuration)
	if err := helmCfg.Init(flags, namespace, "secrets", func(string, ...interface{}) {}); err != nil {
		t.Fatal(err)
	}
	if err := helmCfg.Releases.Create(rel); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = helmCfg.Releases.Delete(name, rel.Version) })
}

// A Helm release whose install started moments ago (a previous leader may
// still finish it) is waited on, soon, instead of failing the component until
// the 5-minute resync.
func TestReconcile_AHelmReleaseBusyElsewhereIsWaitedOn(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeHelm, Namespace: "default", Version: probeRelease,
		Repo: "http://127.0.0.1:1", Chart: "probe",
	}
	c, r := onEnvtest(t, spec, k4allv1alpha1.ComponentStatus{})
	storeHelmRelease(t, r.RestConfig, "default", "probe", &release.Release{
		Name: "probe", Namespace: "default", Version: 1,
		Info:  &release.Info{Status: release.StatusPendingInstall, LastDeployed: helmtime.Time{Time: time.Now()}},
		Chart: &chart.Chart{Metadata: &chart.Metadata{Name: "probe", Version: probeRelease}},
	})

	res := reconcileProbe(t, r)

	st := probeStatus(t, c)
	if st.State != k4allv1alpha1.ComponentStatePending {
		t.Fatalf("%s (%s), want Pending while another helm operation may hold the release", st.State, st.Message)
	}
	if !strings.Contains(st.Message, "in progress") {
		t.Errorf("message %q should say the release is busy", st.Message)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 30*time.Second {
		t.Errorf("waiting on a busy release should come back soon, got RequeueAfter %v", res.RequeueAfter)
	}
}
