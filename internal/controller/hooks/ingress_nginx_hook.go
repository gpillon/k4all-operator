package hooks

import (
	"context"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IngressNginxHook computes Helm values for the NGINX Ingress Controller
// based on ClusterConfig (dedicated IP, isDefault, HA type, host networking).
// Mirrors the logic from setup-nginx-ingress.sh.
type IngressNginxHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewIngressNginxHook(c client.Client, log logr.Logger) *IngressNginxHook {
	return &IngressNginxHook{client: c, log: log}
}

func (h *IngressNginxHook) PreInstall(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec, values map[string]interface{}) (map[string]interface{}, error) {
	nginxCfg := config.Ingress.NginxSettings()
	haType := config.Cluster.HA.Type

	controller, _ := values["controller"].(map[string]interface{})
	if controller == nil {
		controller = make(map[string]interface{})
	}

	service, _ := controller["service"].(map[string]interface{})
	if service == nil {
		service = make(map[string]interface{})
	}

	labels, _ := service["labels"].(map[string]interface{})
	if labels == nil {
		labels = make(map[string]interface{})
	}
	labels["k4all-ingress"] = "nginx"
	service["labels"] = labels

	dedicatedIP := nginxCfg.DedicatedIP
	isDefault := nginxCfg.IsDefault

	if dedicatedIP != "" {
		service["type"] = serviceTypeLoadBalancer
		controller["hostNetwork"] = false
		service["loadBalancerIP"] = dedicatedIP
	} else {
		clusterIP, err := nodeInternalIP(ctx, h.client)
		if err != nil {
			h.log.V(1).Info("could not detect cluster IP, skipping externalIPs", "error", err)
		} else {
			service["externalIPs"] = []string{clusterIP}
			service["loadBalancerIP"] = clusterIP
		}
		service["type"] = serviceTypeLoadBalancer
	}

	if haType == haTypeKubeVIP && dedicatedIP == "" {
		service["loadBalancerClass"] = kubeVIPLoadBalancer
	}

	ingressClassResource, _ := controller["ingressClassResource"].(map[string]interface{})
	if ingressClassResource == nil {
		ingressClassResource = make(map[string]interface{})
	}
	ingressClassResource["default"] = isDefault
	controller["ingressClassResource"] = ingressClassResource

	// When there's no dedicated IP and no kubevip, use hostPort for direct node access
	if dedicatedIP == "" && haType != haTypeKubeVIP && isDefault {
		h.log.Info("using hostPort mode for NGINX (no dedicated IP, no kubevip)")
		hostPort := make(map[string]interface{})
		hostPort["enabled"] = true
		controller["hostPort"] = hostPort
		service["type"] = "ClusterIP"
	} else {
		hostPort := make(map[string]interface{})
		hostPort["enabled"] = false
		controller["hostPort"] = hostPort
	}

	controller["service"] = service
	values["controller"] = controller

	return values, nil
}
