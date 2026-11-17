package migrations

import k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"

// nginxToTraefik: from k4all v2.6.0 Traefik is the ingress controller. Its
// settings move from ingress.nginx to ingress.traefik, unless the cluster
// already has its own, and ingress.nginx goes away.
var nginxToTraefik = Migration{
	Name: "nginx-to-traefik",
	Due: func(rm *k4allv1alpha1.ReleaseManifest) bool {
		traefik, ok := rm.Spec.Components["traefik"]
		if !ok || traefik.Management == k4allv1alpha1.ComponentManagementRemoved {
			return false
		}
		nginx, ok := rm.Spec.Components["ingress-nginx"]
		return !ok || nginx.Management == k4allv1alpha1.ComponentManagementRemoved
	},
	Apply: func(spec *k4allv1alpha1.ClusterConfigSpec) {
		ingress := &spec.Ingress
		if ingress.Traefik == nil && ingress.Nginx != nil {
			ingress.Traefik = &k4allv1alpha1.TraefikIngressConfig{
				DedicatedIP: ingress.Nginx.DedicatedIP,
				IsDefault:   ingress.Nginx.IsDefault,
			}
		}
		ingress.Nginx = nil
	},
}
