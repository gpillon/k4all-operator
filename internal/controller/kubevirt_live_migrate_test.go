package controller

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// liveMigratePatch is the KubeVirt patch of the k4all v2.7.0 release (D3, a
// release invariant): VMs follow a KubeVirt update by live migration.
const liveMigratePatch = `{"spec":{"workloadUpdateStrategy":{"workloadUpdateMethods":["LiveMigrate"]}}}`

func workloadUpdateMethods(t *testing.T, cr *unstructured.Unstructured) []string {
	t.Helper()
	methods, _, err := unstructured.NestedStringSlice(cr.Object, "spec", "workloadUpdateStrategy", "workloadUpdateMethods")
	if err != nil {
		t.Fatal(err)
	}
	return methods
}

// The apply of upstream's kubevirt-cr.yaml (workloadUpdateStrategy: {}, as
// k4all-operator) and the release's patch (workloadUpdateMethods, folded
// into it since #44, after it before) do not fight: every apply ends with
// [LiveMigrate], a value set by hand gives way at the next apply, and
// Hangar's own KubeVirt fields (evictionStrategy, D3) stay.
func TestReconcile_KubeVirtKeepsTheLiveMigrateInvariant(t *testing.T) {
	srv := upstreamReleases(t)
	kubevirt := func(version string) k4allv1alpha1.ComponentSpec {
		return k4allv1alpha1.ComponentSpec{
			Type:      k4allv1alpha1.ComponentTypeManifest,
			Version:   version,
			Sources:   []string{srv + "/kubevirt/{{ version }}/kubevirt-cr.yaml"},
			Namespace: "kubevirt",
			Patches: []k4allv1alpha1.ComponentPatch{{
				Target: k4allv1alpha1.PatchTarget{
					APIVersion: "kubevirt.io/v1", Kind: "KubeVirt", Name: "kubevirt", Namespace: "kubevirt",
				},
				Type:  k4allv1alpha1.PatchTypeMerge,
				Patch: liveMigratePatch,
			}},
		}
	}
	c, r := onEnvtest(t, kubevirt("v1.7.4"), k4allv1alpha1.ComponentStatus{})
	ensureNamespace(t, c, "kubevirt")
	kubeVirtCR.deleteOnCleanup(t, c)
	want := []string{"LiveMigrate"}

	reconcileProbeTo(t, c, r, "v1.7.4", k4allv1alpha1.ComponentStateInstalled)
	if got := workloadUpdateMethods(t, kubeVirtCR.get(t, c)); !slices.Equal(got, want) {
		t.Fatalf("fresh install: workloadUpdateMethods %v, want %v", got, want)
	}

	kubeVirtCR.patchByHand(t, c, `{"spec":{"configuration":{"evictionStrategy":"LiveMigrateIfPossible"},
		"workloadUpdateStrategy":{"workloadUpdateMethods":["Evict"]}}}`)

	for _, version := range []string{"v1.8.4", "v1.9.1"} {
		setProbe(t, c, kubevirt(version))
		reconcileProbeTo(t, c, r, version, k4allv1alpha1.ComponentStateInstalled)
		cr := kubeVirtCR.get(t, c)
		if got := workloadUpdateMethods(t, cr); !slices.Equal(got, want) {
			t.Errorf("after the %s apply: workloadUpdateMethods %v, want %v", version, got, want)
		}
		if got, _, _ := unstructured.NestedString(cr.Object, "spec", "configuration", "evictionStrategy"); got != "LiveMigrateIfPossible" {
			t.Errorf("after the %s apply: evictionStrategy %q, want Hangar's LiveMigrateIfPossible", version, got)
		}
	}
}
