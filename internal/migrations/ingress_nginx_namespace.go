package migrations

import (
	"context"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

// ingressNginxNamespace: operator v0.5.0, pinned by k4all v2.4.0, kept the
// ingress-nginx namespace after it removed ingress-nginx, and its removal
// cleanup never runs again. Once ingress-nginx is uninstalled, this runs
// that cleanup once: the leftovers outside the release, then the namespace
// if nothing else is in it. It never runs while the operator still runs or
// removes ingress-nginx, so the admission webhook's secret is never deleted
// under a running controller.
var ingressNginxNamespace = Migration{
	Name: "ingress-nginx-namespace",
	Due: func(rm *k4allv1alpha1.ReleaseManifest) bool {
		spec, listed := rm.Spec.Components[hooksIngressNginx]
		switch {
		case !listed, spec.Management == k4allv1alpha1.ComponentManagementUnmanaged:
			return true
		case spec.Management == k4allv1alpha1.ComponentManagementRemoved:
			return uninstalled(rm)
		}
		return false // Managed: still the operator's controller
	},
	Act: func(ctx context.Context, env Env, rm *k4allv1alpha1.ReleaseManifest) {
		if !cleanIngressNginxNamespace(rm) {
			return
		}
		hooks.CleanIngressNginxNamespace(ctx, env.Client, env.RestConfig, env.Log)
	},
}

const hooksIngressNginx = "ingress-nginx"

// cleanIngressNginxNamespace says whether the namespace is the operator's to
// clean: ingress-nginx Removed or no longer listed, and uninstalled. Not an
// ingress-nginx the user runs (Unmanaged), nor one still running.
func cleanIngressNginxNamespace(rm *k4allv1alpha1.ReleaseManifest) bool {
	spec, listed := rm.Spec.Components[hooksIngressNginx]
	if listed && spec.Management != k4allv1alpha1.ComponentManagementRemoved {
		return false
	}
	return uninstalled(rm)
}

// uninstalled: the status reports ingress-nginx Removed, or nothing.
func uninstalled(rm *k4allv1alpha1.ReleaseManifest) bool {
	st, ok := rm.Status.Components[hooksIngressNginx]
	return !ok || st.State == k4allv1alpha1.ComponentStateRemoved
}
