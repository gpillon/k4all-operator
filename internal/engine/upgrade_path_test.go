package engine

import (
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// certManager mirrors the cert-manager entry of k4all-release.yaml with an
// upgradePath one minor at a time, as cert-manager recommends upstream.
func certManager() k4allv1alpha1.ComponentSpec {
	return k4allv1alpha1.ComponentSpec{
		Type:     k4allv1alpha1.ComponentTypeHelm,
		Version:  "v1.17.4",
		HelmArgs: []string{"--set crds.enabled=true"},
		UpgradePath: []k4allv1alpha1.UpgradeStep{
			{Version: "v1.15.5"},
			{Version: "v1.16.5"},
		},
	}
}

func TestUpgradeStep_PicksTheNextVersionOnThePath(t *testing.T) {
	cases := []struct {
		installed string
		want      string
		step      bool
	}{
		{installed: "v1.14.5", want: "v1.15.5", step: true},
		{installed: "v1.15.5", want: "v1.16.5", step: true},
		{installed: "v1.16.5", want: "v1.17.4", step: false},
		{installed: "", want: "v1.17.4", step: false},        // fresh install: straight to version
		{installed: "v1.17.4", want: "v1.17.4", step: false}, // already there
		{installed: "v1.18.0", want: "v1.17.4", step: false}, // downgrade: never through the path
	}
	for _, tc := range cases {
		got, step := UpgradeStep(certManager(), tc.installed)
		if got.Version != tc.want || step != tc.step {
			t.Errorf("installed %q: got version %q (step %v), want %q (step %v)",
				tc.installed, got.Version, step, tc.want, tc.step)
		}
	}
}

func TestUpgradeStep_StepArgumentsReplaceTheComponentOnes(t *testing.T) {
	spec := certManager()
	spec.Values = &apiextensionsv1.JSON{Raw: []byte(`{"crds":{"keep":true}}`)}
	spec.UpgradePath[0].HelmArgs = []string{"--set installCRDs=true"}
	spec.UpgradePath[0].Values = &apiextensionsv1.JSON{Raw: []byte(`{}`)}

	withArgs, _ := UpgradeStep(spec, "v1.14.5")
	if len(withArgs.HelmArgs) != 1 || withArgs.HelmArgs[0] != "--set installCRDs=true" {
		t.Errorf("step with helmArgs: got %v, want the step's [--set installCRDs=true]", withArgs.HelmArgs)
	}

	if string(withArgs.Values.Raw) != `{}` {
		t.Errorf("step with values: got %s, want the step's {}", withArgs.Values.Raw)
	}

	inherited, _ := UpgradeStep(spec, "v1.15.5")
	if string(inherited.Values.Raw) != `{"crds":{"keep":true}}` {
		t.Errorf("step without values: got %s, want the component's", inherited.Values.Raw)
	}
	if len(inherited.HelmArgs) != 1 || inherited.HelmArgs[0] != "--set crds.enabled=true" {
		t.Errorf("step without helmArgs: got %v, want the component's [--set crds.enabled=true]", inherited.HelmArgs)
	}
}
