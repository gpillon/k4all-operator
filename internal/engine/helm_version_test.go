package engine

import (
	"context"
	"io"
	"testing"

	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// helmWith is a HelmInstaller over Helm's in-memory release storage, holding
// the given revisions of the cert-manager release.
func helmWith(t *testing.T, revisions ...*release.Release) *HelmInstaller {
	t.Helper()
	store := storage.Init(driver.NewMemory())
	for _, rel := range revisions {
		if err := store.Create(rel); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHelmInstaller(nil, logr.Discard())
	h.configFor = func(string) (*action.Configuration, error) {
		return &action.Configuration{Releases: store, KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard}, Log: func(string, ...interface{}) {}}, nil
	}
	return h
}

func certManagerRelease(revision int, chartVersion string, status release.Status) *release.Release {
	return &release.Release{
		Name: "cert-manager", Namespace: "cert-manager", Version: revision,
		Info:  &release.Info{Status: status},
		Chart: &chart.Chart{Metadata: &chart.Metadata{Name: "cert-manager", Version: chartVersion}},
	}
}

func TestHelmRunningVersion_ADeployedReleaseRunsItsChart(t *testing.T) {
	h := helmWith(t, certManagerRelease(1, "v1.14.5", release.StatusDeployed))

	version, ready, err := h.RunningVersion(context.Background(), "cert-manager", "cert-manager")

	if err != nil || version != "v1.14.5" || !ready {
		t.Fatalf("got (%q, %t, %v), want (\"v1.14.5\", true, nil)", version, ready, err)
	}
}

func TestHelmRunningVersion_NoReleaseIsNotInstalled(t *testing.T) {
	h := helmWith(t)

	version, ready, err := h.RunningVersion(context.Background(), "cert-manager", "cert-manager")

	if err != nil || version != "" || ready {
		t.Fatalf("got (%q, %t, %v), want (\"\", false, nil): not installed", version, ready, err)
	}
}

// A failed upgrade leaves the revision before it deployed: that chart is
// what runs, and helm upgrade can go on from it.
func TestHelmRunningVersion_AFailedUpgradeLeavesThePreviousChartRunning(t *testing.T) {
	h := helmWith(t,
		certManagerRelease(1, "v1.14.5", release.StatusDeployed),
		certManagerRelease(2, "v1.15.5", release.StatusFailed))

	version, ready, err := h.RunningVersion(context.Background(), "cert-manager", "cert-manager")

	if err != nil || version != "v1.14.5" || !ready {
		t.Fatalf("got (%q, %t, %v), want (\"v1.14.5\", true, nil)", version, ready, err)
	}
}

// A failed first install deployed nothing: it is still a fresh install, which
// helm upgrade retries over the failed revision.
func TestHelmRunningVersion_AFailedInstallIsNotInstalled(t *testing.T) {
	h := helmWith(t, certManagerRelease(1, "v1.21.3", release.StatusFailed))

	version, ready, err := h.RunningVersion(context.Background(), "cert-manager", "cert-manager")

	if err != nil || version != "" || ready {
		t.Fatalf("got (%q, %t, %v), want (\"\", false, nil): nothing was deployed", version, ready, err)
	}
}
