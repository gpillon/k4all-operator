package engine

import (
	"context"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
)

// A release whose last revision stays pending-install or pending-upgrade was
// left by an operator process that stopped in the middle of it: on a fresh
// install k4all's CRI-O bridge handover deletes the operator pod, and a pod
// can also be killed by an OOM or a node reboot. Helm then refuses every
// later install or upgrade with "another operation (install/upgrade/rollback)
// is in progress", and the component stays Failed for good (seen on the
// k4all v2.6.6 VM test: cert-manager).

func certManagerChart(version string) *chart.Chart {
	return &chart.Chart{
		Metadata: &chart.Metadata{APIVersion: chart.APIVersionV2, Name: "cert-manager", Version: version},
	}
}

var certManagerSpec = k4allv1alpha1.ComponentSpec{
	Type: k4allv1alpha1.ComponentTypeHelm, Namespace: "cert-manager", Version: "v1.21.3",
}

func lastRevision(t *testing.T, h *HelmInstaller) *release.Release {
	t.Helper()
	cfg, err := h.configFor("cert-manager")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := cfg.Releases.Last("cert-manager")
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func TestHelmInstallOrUpgrade_GoesOnFromAnInstallLeftPending(t *testing.T) {
	h := helmWith(t, certManagerRelease(1, "v1.21.3", release.StatusPendingInstall))
	cfg, _ := h.configFor("cert-manager")

	err := h.installOrUpgrade(context.Background(), cfg, "cert-manager", certManagerSpec,
		certManagerChart("v1.21.3"), nil, false)

	if err != nil {
		t.Fatalf("installOrUpgrade: %v, want the release installed: the pending-install was left by a stopped operator", err)
	}
	if last := lastRevision(t, h); last.Version != 2 || last.Info.Status != release.StatusDeployed {
		t.Fatalf("last revision %d is %s, want revision 2 deployed", last.Version, last.Info.Status)
	}
}

func TestHelmInstallOrUpgrade_GoesOnFromAnUpgradeLeftPending(t *testing.T) {
	h := helmWith(t,
		certManagerRelease(1, "v1.20.5", release.StatusDeployed),
		certManagerRelease(2, "v1.21.3", release.StatusPendingUpgrade))
	cfg, _ := h.configFor("cert-manager")

	err := h.installOrUpgrade(context.Background(), cfg, "cert-manager", certManagerSpec,
		certManagerChart("v1.21.3"), nil, false)

	if err != nil {
		t.Fatalf("installOrUpgrade: %v, want the upgrade done", err)
	}
	last := lastRevision(t, h)
	if last.Version != 3 || last.Info.Status != release.StatusDeployed || last.Chart.Metadata.Version != "v1.21.3" {
		t.Fatalf("last revision %d is %s with chart %s, want revision 3 deployed with v1.21.3",
			last.Version, last.Info.Status, last.Chart.Metadata.Version)
	}
}

// A deployed release is upgraded as before, and nothing is rewritten.
func TestHelmInstallOrUpgrade_ADeployedReleaseIsUpgradedAsBefore(t *testing.T) {
	h := helmWith(t, certManagerRelease(1, "v1.21.3", release.StatusDeployed))
	cfg, _ := h.configFor("cert-manager")

	if err := h.installOrUpgrade(context.Background(), cfg, "cert-manager", certManagerSpec,
		certManagerChart("v1.21.3"), nil, false); err != nil {
		t.Fatal(err)
	}
	first, err := cfg.Releases.Get("cert-manager", 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Info.Status != release.StatusSuperseded {
		t.Errorf("revision 1 is %s, want superseded by the upgrade", first.Info.Status)
	}
}
