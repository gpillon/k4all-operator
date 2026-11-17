package v1alpha1

import "slices"

// DedicatedIPs returns the LoadBalancer IPs the ingress controllers ask for,
// each once: Traefik's, NGINX's, then Cilium's. A ClusterConfig without
// ingress.traefik gives Traefik the nginx settings, so its IP is NGINX's.
func (in *IngressConfig) DedicatedIPs() []string {
	var traefik string
	if in.Traefik != nil {
		traefik = in.Traefik.DedicatedIP
	}
	var ips []string
	for _, ip := range []string{traefik, in.Nginx.DedicatedIP, in.Cilium.DedicatedIP} {
		if ip != "" && !slices.Contains(ips, ip) {
			ips = append(ips, ip)
		}
	}
	return ips
}
