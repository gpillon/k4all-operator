package migrations

import (
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// nginxState is a ReleaseManifest whose ingress-nginx entry has management
// (or none, when "") and whose status reports state (or none, when "").
func nginxState(management k4allv1alpha1.ComponentManagement, state k4allv1alpha1.ComponentState, listed bool) *k4allv1alpha1.ReleaseManifest {
	rm := manifest(map[string]k4allv1alpha1.ComponentManagement{"traefik": k4allv1alpha1.ComponentManagementManaged})
	if listed {
		rm.Spec.Components["ingress-nginx"] = k4allv1alpha1.ComponentSpec{Type: "helm", Version: "4.14.3", Management: management}
	}
	if state != "" {
		rm.Status.Components = map[string]k4allv1alpha1.ComponentStatus{"ingress-nginx": {State: state}}
	}
	return rm
}

// The migration waits while ingress-nginx is still the operator's to run or
// to remove; it cleans only once the uninstall is done, and never the
// namespace of an ingress-nginx the user runs.
func TestIngressNginxNamespace_DueAndClean(t *testing.T) {
	for name, tc := range map[string]struct {
		rm         *k4allv1alpha1.ReleaseManifest
		due, clean bool
	}{
		"Managed":                          {nginxState(k4allv1alpha1.ComponentManagementManaged, k4allv1alpha1.ComponentStateInstalled, true), false, false},
		"Removed, not uninstalled yet":     {nginxState(k4allv1alpha1.ComponentManagementRemoved, k4allv1alpha1.ComponentStateInstalled, true), false, false},
		"Removed, uninstalling":            {nginxState(k4allv1alpha1.ComponentManagementRemoved, k4allv1alpha1.ComponentStateUninstalling, true), false, false},
		"Removed and uninstalled (v2.4.0)": {nginxState(k4allv1alpha1.ComponentManagementRemoved, k4allv1alpha1.ComponentStateRemoved, true), true, true},
		"no longer listed (v2.6.0)":        {nginxState("", k4allv1alpha1.ComponentStateRemoved, false), true, true},
		"never installed":                  {nginxState("", "", false), true, true},
		"Unmanaged: the user's":            {nginxState(k4allv1alpha1.ComponentManagementUnmanaged, k4allv1alpha1.ComponentStateUnmanaged, true), true, false},
		"no longer listed, still running":  {nginxState("", k4allv1alpha1.ComponentStateInstalled, false), true, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ingressNginxNamespace.Due(tc.rm); got != tc.due {
				t.Errorf("due = %t, want %t", got, tc.due)
			}
			if got := cleanIngressNginxNamespace(tc.rm); got != tc.clean {
				t.Errorf("clean = %t, want %t", got, tc.clean)
			}
		})
	}
}
