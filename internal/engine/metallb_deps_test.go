package engine

import (
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// A cluster installed with k4all v2.6.0 or later has ingress.traefik and no
// nginx key. On Cilium, MetalLB only comes up for dedicated ingress IPs, and
// Traefik's is one of them.
func TestFilterActiveComponents_MetalLBForTraefikDedicatedIP(t *testing.T) {
	components := map[string]k4allv1alpha1.ComponentSpec{
		"metallb": {Type: k4allv1alpha1.ComponentTypeManifest, Version: "v0.16.2"},
	}
	config := k4allv1alpha1.ClusterConfigSpec{
		Networking: k4allv1alpha1.NetworkingConfig{CNI: k4allv1alpha1.CNIConfig{Type: "cilium"}},
		Ingress: k4allv1alpha1.IngressConfig{
			Traefik: &k4allv1alpha1.TraefikIngressConfig{DedicatedIP: "192.168.6.193", IsDefault: true},
		},
	}

	if _, ok := FilterActiveComponents(components, config)["metallb"]; !ok {
		t.Error("metallb not installed on Cilium with ingress.traefik.dedicatedIP set")
	}
}
