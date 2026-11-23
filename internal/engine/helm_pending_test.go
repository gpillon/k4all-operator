package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	helmtime "helm.sh/helm/v3/pkg/time"
)

// A release whose last revision stays pending-install or pending-upgrade was
// left by an operator process that stopped in the middle of it: on a fresh
// install k4all's CRI-O bridge handover deletes the operator pod, and a pod
// can also be OOM-killed or rebooted with its node. Helm then refuses every
// later install or upgrade with "another operation (install/upgrade/rollback)
// is in progress", and the component stays Failed for good (k4all v2.6.6 VM
// test: cert-manager). Reconcile recovers the release, then installs or
// upgrades: these tests run the same two steps on Helm's in-memory storage.

var testNow = time.Date(2026, 11, 23, 18, 10, 0, 0, time.UTC)

func certManagerChart(version string) *chart.Chart {
	return &chart.Chart{
		Metadata: &chart.Metadata{APIVersion: chart.APIVersionV2, Name: "cert-manager", Version: version},
	}
}

var certManagerSpec = k4allv1alpha1.ComponentSpec{
	Type: k4allv1alpha1.ComponentTypeHelm, Namespace: "cert-manager", Version: "v1.21.3",
}

// pendingSince is a cert-manager revision in status, last written at.
func pendingSince(revision int, chartVersion string, status release.Status, at time.Time) *release.Release {
	rel := certManagerRelease(revision, chartVersion, status)
	rel.Info.LastDeployed = helmtime.Time{Time: at}
	return rel
}

func testConfig(t *testing.T, h *HelmInstaller) *action.Configuration {
	t.Helper()
	cfg, err := h.configFor("cert-manager")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// reconcileChart is HelmInstaller.Reconcile after the chart (cert-manager
// v1.21.3) is pulled.
func reconcileChart(h *HelmInstaller, cfg *action.Configuration) error {
	if err := h.recoverPendingRelease(cfg, "cert-manager", testNow); err != nil {
		return err
	}
	return h.installOrUpgrade(context.Background(), cfg, "cert-manager", certManagerSpec,
		certManagerChart("v1.21.3"), nil, false)
}

func history(t *testing.T, cfg *action.Configuration) map[int]release.Status {
	t.Helper()
	revs, err := cfg.Releases.History("cert-manager")
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]release.Status{}
	for _, r := range revs {
		out[r.Version] = r.Info.Status
	}
	return out
}

// A first install left pending: only its record goes, then the install runs
// (and adopts what the stopped one created). Nothing is uninstalled.
func TestHelmReconcile_AFirstInstallLeftPendingIsInstalledAgain(t *testing.T) {
	h := helmWith(t, pendingSince(1, "v1.21.3", release.StatusPendingInstall, testNow.Add(-2*time.Minute)))
	cfg := testConfig(t, h)

	if err := reconcileChart(h, cfg); err != nil {
		t.Fatalf("reconcile: %v, want the release installed: the pending-install was left by a stopped operator", err)
	}
	if got := history(t, cfg); len(got) != 1 || got[1] != release.StatusDeployed {
		t.Fatalf("history %v, want only revision 1, deployed by the new install", got)
	}
}

// An upgrade left pending over a deployed revision: rolled back to it, then
// upgraded as usual.
func TestHelmReconcile_AnUpgradeLeftPendingIsRolledBackThenUpgraded(t *testing.T) {
	h := helmWith(t,
		pendingSince(1, "v1.20.5", release.StatusDeployed, testNow.Add(-time.Hour)),
		pendingSince(2, "v1.21.3", release.StatusPendingUpgrade, testNow.Add(-2*time.Minute)))
	cfg := testConfig(t, h)

	if err := reconcileChart(h, cfg); err != nil {
		t.Fatalf("reconcile: %v, want the upgrade done", err)
	}
	last, err := cfg.Releases.Last("cert-manager")
	if err != nil {
		t.Fatal(err)
	}
	if last.Info.Status != release.StatusDeployed || last.Chart.Metadata.Version != "v1.21.3" {
		t.Fatalf("last revision %d is %s with chart %s, want deployed v1.21.3",
			last.Version, last.Info.Status, last.Chart.Metadata.Version)
	}
	for rev, status := range history(t, cfg) {
		if status.IsPending() {
			t.Errorf("revision %d is still %s", rev, status)
		}
	}
	rollback, err := cfg.Releases.Get("cert-manager", 3)
	if err != nil || rollback.Chart.Metadata.Version != "v1.20.5" {
		t.Errorf("revision 3 should be the rollback to v1.20.5, got %v (%v)", rollback, err)
	}
}

// A pending revision that recent may belong to an operation still running
// (a leader hand-over): it is left alone and reported as busy.
func TestHelmReconcile_ARecentPendingRevisionIsLeftAlone(t *testing.T) {
	h := helmWith(t, pendingSince(1, "v1.21.3", release.StatusPendingInstall, testNow.Add(-20*time.Second)))
	cfg := testConfig(t, h)

	err := reconcileChart(h, cfg)

	if !errors.Is(err, ErrHelmReleaseBusy) {
		t.Fatalf("reconcile: %v, want ErrHelmReleaseBusy", err)
	}
	if got := history(t, cfg); len(got) != 1 || got[1] != release.StatusPendingInstall {
		t.Fatalf("history %v, want revision 1 still pending-install", got)
	}
}

// A deployed release is upgraded as before.
func TestHelmReconcile_ADeployedReleaseIsUpgradedAsBefore(t *testing.T) {
	h := helmWith(t, pendingSince(1, "v1.21.3", release.StatusDeployed, testNow.Add(-time.Hour)))
	cfg := testConfig(t, h)

	if err := reconcileChart(h, cfg); err != nil {
		t.Fatal(err)
	}
	if got := history(t, cfg); got[1] != release.StatusSuperseded || got[2] != release.StatusDeployed {
		t.Fatalf("history %v, want 1 superseded and 2 deployed", got)
	}
}
