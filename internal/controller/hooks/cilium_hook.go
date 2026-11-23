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
// (kube-proxy replacement, spec.networking.cni.cilium options).
type CiliumHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewCiliumHook(c client.Client, log logr.Logger) *CiliumHook {
	return &CiliumHook{client: c, log: log}
}

// ciliumValuesRevision changes whenever BuildCiliumValues renders other
// values for the same ClusterConfig. 2: socketLB.hostNamespaceOnly with
// KubeVirt, cni.exclusive off with Multus, devices ovs-bridge with
// forceDeviceDetection with the OVS bridge.
const ciliumValuesRevision = "2"

// ValuesRevision implements ValuesRevisioner.
func (h *CiliumHook) ValuesRevision() string { return ciliumValuesRevision }

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

// ovsBridgeDevice is the bridge created on every node when features.ovsBridge is enabled.
const ovsBridgeDevice = "ovs-bridge"

// BuildCiliumValues computes Helm values for Cilium based on ClusterConfig.
func BuildCiliumValues(config k4allv1alpha1.ClusterConfigSpec) map[string]interface{} {
	cilium := config.Networking.CNI.Cilium
	values := map[string]interface{}{
		"kubeProxyReplacement": true,
		"externalIPs": map[string]interface{}{
			"enabled": true,
		},
	}

	if cilium.L2Announcements {
		values["l2announcements"] = map[string]interface{}{"enabled": true}
	}

	// The OVS bridge: Cilium (1.20) skips devices of kind openvswitch in its
	// auto-detection, so once k4all moved the node IP to ovs-bridge an agent
	// that starts finds no device and stops ("unable to determine direct
	// routing device"), at every reboot. ovs-bridge is named, and
	// forceDeviceDetection keeps the auto-detection for every other device:
	// on a new node the agent starts on the NIC before the bridge exists,
	// and the NIC, once a port of the bridge, is skipped.
	// Setting devices without the OVS bridge turns off the auto-detection:
	// additionalDevices are then the whole list, as before.
	if config.Features.OVSBridge.Enabled {
		values["devices"] = append([]string{ovsBridgeDevice}, cilium.AdditionalDevices...)
		values["forceDeviceDetection"] = true
	} else if len(cilium.AdditionalDevices) > 0 {
		values["devices"] = append([]string{}, cilium.AdditionalDevices...)
	}

	// KubeVirt: a VM on the pod network sends its packets through
	// virt-launcher's NAT, not from a socket in the pod. With the socket load
	// balancer on in pods, Cilium skips the per-packet translation there, so
	// a VM reaches no ClusterIP, the cluster DNS included. This is Cilium's
	// documented setting for KubeVirt (and Kata).
	if config.Features.Virt.Enabled {
		values["socketLB"] = map[string]interface{}{"hostNamespaceOnly": true}
	}

	// Multus (feature ovsCni) delegates to Cilium through its own CNI
	// configuration: with cni.exclusive Cilium renames it at every start, so
	// pods and VMs never get their secondary networks.
	if config.Features.OVSCNI.Enabled {
		values["cni"] = map[string]interface{}{"exclusive": false}
	}

	// The agents read these values at their next start; the operator does
	// not roll them (rollOutCiliumPods): a node still registered with a
	// pod-network InternalIP, or still without the devices above, would
	// crash-loop. k4all-upgrade restarts the agent of each node once the
	// node runs the release that pins this operator.

	if cilium.GatewayAPI {
		values["gatewayAPI"] = map[string]interface{}{"enabled": true}
	}

	if cilium.Hubble {
		values["hubble"] = map[string]interface{}{
			"relay": map[string]interface{}{"enabled": true},
			"ui":    map[string]interface{}{"enabled": true},
		}
	}

	return values
}
