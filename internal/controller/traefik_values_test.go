package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const nodeIP = "192.168.56.100"

// traefikValues returns the Helm values the operator computes for the traefik
// component on a single-node cluster whose node has InternalIP nodeIP.
// Expected values follow IngressNginxHook, which Traefik replaces in v2.6.0.
func traefikValues(t *testing.T, config k4allv1alpha1.ClusterConfigSpec) map[string]interface{} {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "k4all"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: nodeIP},
		}},
	}
	r := &ReleaseReconciler{
		hookReg: hooks.NewRegistry(fake.NewClientBuilder().WithObjects(node).Build(), logr.Discard()),
	}
	got, err := r.componentValues(context.Background(), "traefik", k4allv1alpha1.ComponentSpec{}, config)
	if err != nil {
		t.Fatal(err)
	}
	return normalize(t, got)
}

// An existing cluster only has ingress.nginx: Traefik takes its place on the
// node's own ports 80/443, as nginx did without a dedicated IP or kube-vip.
func TestComponentValues_TraefikTakesOverNginxHostPorts(t *testing.T) {
	config := k4allv1alpha1.ClusterConfigSpec{
		Ingress: k4allv1alpha1.IngressConfig{Nginx: &k4allv1alpha1.NginxIngressConfig{IsDefault: true}},
	}

	got := traefikValues(t, config)

	expectValue(t, got, float64(80), "ports", "web", "hostPort")
	expectValue(t, got, float64(443), "ports", "websecure", "hostPort")
	expectValue(t, got, "ClusterIP", "service", "spec", "type")
	expectValue(t, got, []interface{}{nodeIP}, "service", "spec", "externalIPs")
	expectValue(t, got, "traefik", "service", "labels", "k4all-ingress")
	expectValue(t, got, true, "ingressClass", "isDefaultClass")
	// With host ports a surge pod cannot start next to the old one.
	expectValue(t, got, float64(1), "updateStrategy", "rollingUpdate", "maxUnavailable")
	expectValue(t, got, float64(0), "updateStrategy", "rollingUpdate", "maxSurge")
}

func TestComponentValues_TraefikDedicatedIPUsesALoadBalancer(t *testing.T) {
	config := k4allv1alpha1.ClusterConfigSpec{
		Ingress: k4allv1alpha1.IngressConfig{Nginx: &k4allv1alpha1.NginxIngressConfig{
			DedicatedIP: "192.168.56.200", IsDefault: true,
		}},
	}

	got := traefikValues(t, config)

	expectValue(t, got, "LoadBalancer", "service", "spec", "type")
	expectValue(t, got, "192.168.56.200", "service", "spec", "loadBalancerIP")
	for _, path := range [][]string{
		{"ports", "web", "hostPort"},
		{"ports", "websecure", "hostPort"},
		{"service", "spec", "externalIPs"},
	} {
		if v, ok := valueAt(got, path...); ok {
			t.Errorf("%v = %v, want unset with a dedicated IP", path, v)
		}
	}
}

// Without host ports the service carries the node IP, as nginx's did.
func TestComponentValues_TraefikWithoutHostPorts(t *testing.T) {
	cases := map[string]k4allv1alpha1.ClusterConfigSpec{
		"kube-vip": {
			Cluster: k4allv1alpha1.ControlPlaneConfig{HA: k4allv1alpha1.HAConfig{Type: "kubevip"}},
			Ingress: k4allv1alpha1.IngressConfig{Nginx: &k4allv1alpha1.NginxIngressConfig{IsDefault: true}},
		},
		"not the default class": {
			Ingress: k4allv1alpha1.IngressConfig{Nginx: &k4allv1alpha1.NginxIngressConfig{IsDefault: false}},
		},
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			got := traefikValues(t, config)

			expectValue(t, got, "LoadBalancer", "service", "spec", "type")
			expectValue(t, got, nodeIP, "service", "spec", "loadBalancerIP")
			expectValue(t, got, []interface{}{nodeIP}, "service", "spec", "externalIPs")
			if v, ok := valueAt(got, "ports"); ok {
				t.Errorf("ports = %v, want no host ports", v)
			}
		})
	}
}

func TestComponentValues_TraefikKubeVIPClass(t *testing.T) {
	config := k4allv1alpha1.ClusterConfigSpec{
		Cluster: k4allv1alpha1.ControlPlaneConfig{HA: k4allv1alpha1.HAConfig{Type: "kubevip"}},
	}

	got := traefikValues(t, config)

	expectValue(t, got, "kube-vip.io/kube-vip-class", "service", "spec", "loadBalancerClass")
}

// Once ingress.traefik is present it wins, even when it is all defaults:
// nginx settings are only the fallback for clusters installed before v2.6.0.
func TestComponentValues_TraefikConfigWinsOverNginx(t *testing.T) {
	config := k4allv1alpha1.ClusterConfigSpec{
		Ingress: k4allv1alpha1.IngressConfig{
			Nginx:   &k4allv1alpha1.NginxIngressConfig{DedicatedIP: "192.168.56.200", IsDefault: true},
			Traefik: &k4allv1alpha1.TraefikIngressConfig{},
		},
	}

	got := traefikValues(t, config)

	expectValue(t, got, false, "ingressClass", "isDefaultClass")
	expectValue(t, got, nodeIP, "service", "spec", "loadBalancerIP")
}
