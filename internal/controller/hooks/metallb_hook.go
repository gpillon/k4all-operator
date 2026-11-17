package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MetalLBHook handles post-install configuration for MetalLB:
//   - Patches kube-proxy for strictARP
//   - Creates IPAddressPool + L2Advertisement from dedicated ingress IPs
//
// Mirrors the logic from setup-metallb.sh.
type MetalLBHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewMetalLBHook(c client.Client, log logr.Logger) *MetalLBHook {
	return &MetalLBHook{client: c, log: log}
}

func (h *MetalLBHook) PostInstall(ctx context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) error {
	dedicatedIPs := metalLBAddresses(config)
	if len(dedicatedIPs) == 0 {
		h.log.Info("no dedicated IPs for MetalLB, no k4all-ingress-pool")
		return h.deletePool(ctx)
	}

	if err := h.patchKubeProxyStrictARP(ctx); err != nil {
		h.log.Error(err, "failed to enable strictARP in kube-proxy (non-fatal)")
	}

	if err := h.ensureIPAddressPool(ctx, dedicatedIPs); err != nil {
		return fmt.Errorf("ensure IPAddressPool: %w", err)
	}

	if err := h.ensureL2Advertisement(ctx); err != nil {
		return fmt.Errorf("ensure L2Advertisement: %w", err)
	}

	h.log.Info("MetalLB post-install complete", "ips", dedicatedIPs)
	return nil
}

// metalLBAddresses are the dedicated ingress IPs MetalLB announces: all but
// the one the host's kube-vip announces (HA kube-vip).
func metalLBAddresses(config k4allv1alpha1.ClusterConfigSpec) []string {
	kubeVIP := kubeVIPIngressIP(config)
	var ips []string
	for _, ip := range config.Ingress.DedicatedIPs() {
		if ip != kubeVIP {
			ips = append(ips, ip)
		}
	}
	return ips
}

// deletePool removes k4all-ingress-pool and its L2Advertisement, left from
// when MetalLB had addresses to announce.
func (h *MetalLBHook) deletePool(ctx context.Context) error {
	for _, kind := range []string{"L2Advertisement", "IPAddressPool"} {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion("metallb.io/v1beta1")
		obj.SetKind(kind)
		obj.SetNamespace("metallb-system")
		obj.SetName("k4all-ingress-pool")
		if kind == "L2Advertisement" {
			obj.SetName("k4all-l2adv")
		}
		if err := h.client.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return fmt.Errorf("delete %s: %w", kind, err)
		}
	}
	return nil
}

func (h *MetalLBHook) patchKubeProxyStrictARP(ctx context.Context) error {
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")

	key := types.NamespacedName{Name: "kube-proxy", Namespace: "kube-system"}
	if err := h.client.Get(ctx, key, cm); err != nil {
		return fmt.Errorf("get kube-proxy ConfigMap: %w", err)
	}

	data, ok := cm.Object["data"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("kube-proxy ConfigMap has no data")
	}
	configStr, ok := data["config.conf"].(string)
	if !ok {
		return nil
	}

	if strings.Contains(configStr, "strictARP: false") {
		data["config.conf"] = strings.ReplaceAll(configStr, "strictARP: false", "strictARP: true")
		cm.Object["data"] = data
		return h.client.Update(ctx, cm)
	}
	return nil
}

func (h *MetalLBHook) ensureIPAddressPool(ctx context.Context, ips []string) error {
	addresses := make([]interface{}, 0, len(ips))
	for _, ip := range ips {
		addresses = append(addresses, ip+"/32")
	}

	pool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "metallb.io/v1beta1",
			"kind":       "IPAddressPool",
			"metadata": map[string]interface{}{
				"name":      "k4all-ingress-pool",
				"namespace": "metallb-system",
			},
			"spec": map[string]interface{}{
				"addresses": addresses,
			},
		},
	}

	return h.applyWithRetry(ctx, pool, "IPAddressPool")
}

func (h *MetalLBHook) ensureL2Advertisement(ctx context.Context) error {
	adv := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "metallb.io/v1beta1",
			"kind":       "L2Advertisement",
			"metadata": map[string]interface{}{
				"name":      "k4all-l2adv",
				"namespace": "metallb-system",
			},
			"spec": map[string]interface{}{
				"ipAddressPools": []interface{}{
					"k4all-ingress-pool",
				},
			},
		},
	}

	return h.applyWithRetry(ctx, adv, "L2Advertisement")
}

func (h *MetalLBHook) applyWithRetry(ctx context.Context, obj *unstructured.Unstructured, kind string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		patchData, err := json.Marshal(obj.Object)
		if err != nil {
			return err
		}

		existing := obj.DeepCopy()
		err = h.client.Get(waitCtx, types.NamespacedName{
			Name:      obj.GetName(),
			Namespace: obj.GetNamespace(),
		}, existing)

		if errors.IsNotFound(err) {
			if createErr := h.client.Create(waitCtx, obj); createErr != nil {
				h.log.V(1).Info("retrying create", "kind", kind, "error", createErr)
				select {
				case <-waitCtx.Done():
					return fmt.Errorf("timed out creating %s: %w", kind, createErr)
				case <-ticker.C:
					continue
				}
			}
			return nil
		} else if err != nil {
			return fmt.Errorf("get %s: %w", kind, err)
		}

		return h.client.Patch(waitCtx, existing, client.RawPatch(types.MergePatchType, patchData))
	}
}
