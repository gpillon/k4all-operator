package hooks

import (
	"context"
	"encoding/json"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CiliumHook configures Cilium-specific Helm values that depend on ClusterConfig
// (pod CIDR, kube-proxy replacement, device selection, L2 announcements).
type CiliumHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewCiliumHook(c client.Client, log logr.Logger) *CiliumHook {
	return &CiliumHook{client: c, log: log}
}

func (h *CiliumHook) PreInstall(_ context.Context, _ string, _ k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec, values map[string]interface{}) (map[string]interface{}, error) {
	extra := BuildCiliumValues(config)
	for k, v := range extra {
		values[k] = v
	}
	return values, nil
}

// Cleanup removes cilium-specific node taints after uninstallation.
func (h *CiliumHook) Cleanup(ctx context.Context, name string, config k4allv1alpha1.ClusterConfigSpec) error {
	h.log.Info("cleaning up cilium artifacts")

	nodes := &corev1.NodeList{}
	if err := h.client.List(ctx, nodes); err != nil {
		return err
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		var filtered []corev1.Taint
		changed := false
		for _, t := range node.Spec.Taints {
			if t.Key == "node.cilium.io/agent-not-ready" {
				changed = true
				continue
			}
			filtered = append(filtered, t)
		}
		if changed {
			h.log.Info("removing cilium taint from node", "node", node.Name)
			patch := map[string]interface{}{
				"spec": map[string]interface{}{
					"taints": filtered,
				},
			}
			patchBytes, _ := json.Marshal(patch)
			if err := h.client.Patch(ctx, node, client.RawPatch(types.MergePatchType, patchBytes)); err != nil {
				h.log.Error(err, "failed to remove cilium taint", "node", node.Name)
			}
		}
	}

	return nil
}

// BuildCiliumValues computes Helm values for Cilium based on ClusterConfig.
func BuildCiliumValues(config k4allv1alpha1.ClusterConfigSpec) map[string]interface{} {
	values := map[string]interface{}{
		"kubeProxyReplacement": true,
		"l2announcements": map[string]interface{}{
			"enabled": true,
		},
		"externalIPs": map[string]interface{}{
			"enabled": true,
		},
	}

	if config.Networking.Interface.Dev != "" {
		values["devices"] = []string{config.Networking.Interface.Dev + "+"}
	}

	return values
}
