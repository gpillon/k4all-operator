package hooks

import (
	"context"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TraefikHook computes Helm values (traefik/traefik chart) for the ingress
// controller that replaces ingress-nginx in k4all v2.6.0, from the same
// ClusterConfig settings IngressNginxHook uses.
type TraefikHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewTraefikHook(c client.Client, log logr.Logger) *TraefikHook {
	return &TraefikHook{client: c, log: log}
}

func (h *TraefikHook) PreInstall(ctx context.Context, _ string, _ k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec, values map[string]interface{}) (map[string]interface{}, error) {
	cfg := traefikConfig(config.Ingress)

	service := nested(values, "service")
	nested(service, "labels")["k4all-ingress"] = "traefik"
	serviceSpec := nested(service, "spec")
	nested(values, "ingressClass")["isDefaultClass"] = cfg.IsDefault

	if cfg.DedicatedIP != "" {
		serviceSpec["type"] = serviceTypeLoadBalancer
		serviceSpec["loadBalancerIP"] = cfg.DedicatedIP
		return values, nil
	}

	nodeIP, err := nodeInternalIP(ctx, h.client)
	if err != nil {
		h.log.V(1).Info("could not detect cluster IP, skipping externalIPs", "error", err)
	} else {
		serviceSpec["externalIPs"] = []string{nodeIP}
	}

	kubeVIP := config.Cluster.HA.Type == haTypeKubeVIP
	if kubeVIP || !cfg.IsDefault {
		serviceSpec["type"] = serviceTypeLoadBalancer
		if nodeIP != "" {
			serviceSpec["loadBalancerIP"] = nodeIP
		}
		if kubeVIP {
			serviceSpec["loadBalancerClass"] = kubeVIPLoadBalancer
		}
		return values, nil
	}

	// No dedicated IP and no kube-vip: listen on the node's own ports.
	nested(values, "ports", "web")["hostPort"] = 80
	nested(values, "ports", "websecure")["hostPort"] = 443
	serviceSpec["type"] = "ClusterIP"
	// The new pod cannot bind the host ports while the old one holds them.
	rollingUpdate := nested(values, "updateStrategy", "rollingUpdate")
	rollingUpdate["maxUnavailable"] = 1
	rollingUpdate["maxSurge"] = 0

	return values, nil
}

// traefikConfig returns ingress.traefik, or the nginx settings on clusters
// whose ClusterConfig predates it.
func traefikConfig(ingress k4allv1alpha1.IngressConfig) k4allv1alpha1.TraefikIngressConfig {
	if ingress.Traefik != nil {
		return *ingress.Traefik
	}
	return k4allv1alpha1.TraefikIngressConfig{
		DedicatedIP: ingress.Nginx.DedicatedIP,
		IsDefault:   ingress.Nginx.IsDefault,
	}
}

// nested returns the map at path inside values, creating missing levels.
func nested(values map[string]interface{}, path ...string) map[string]interface{} {
	cur := values
	for _, key := range path {
		next, ok := cur[key].(map[string]interface{})
		if !ok {
			next = make(map[string]interface{})
			cur[key] = next
		}
		cur = next
	}
	return cur
}
