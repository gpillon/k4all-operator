package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CalicoHook handles post-install logic for Calico:
//   - Waits for the Tigera Operator CRDs to become available
//   - Creates the Installation CR with the pod CIDR from ClusterConfig
//   - Creates the APIServer CR
//
// Mirrors the logic from setup-cni-calico.sh (applying calico-resources.yaml).
type CalicoHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewCalicoHook(c client.Client, log logr.Logger) *CalicoHook {
	return &CalicoHook{client: c, log: log}
}

func (h *CalicoHook) PostInstall(ctx context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) error {
	podCIDR := config.Cluster.PodNetwork
	if podCIDR == "" {
		podCIDR = "10.100.0.1/18"
	}

	if err := h.ensureInstallation(ctx, podCIDR); err != nil {
		return fmt.Errorf("ensure Calico Installation: %w", err)
	}

	if err := h.ensureAPIServer(ctx); err != nil {
		return fmt.Errorf("ensure Calico APIServer: %w", err)
	}

	h.log.Info("Calico post-install complete", "podCIDR", podCIDR)
	return nil
}

func (h *CalicoHook) ensureInstallation(ctx context.Context, podCIDR string) error {
	installation := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.tigera.io/v1",
			"kind":       "Installation",
			"metadata": map[string]interface{}{
				"name": "default",
			},
			"spec": map[string]interface{}{
				"flexVolumePath": "/opt/libexec/kubernetes/kubelet-plugins/volume/exec/",
				"calicoNetwork": map[string]interface{}{
					"ipPools": []interface{}{
						map[string]interface{}{
							"blockSize":     float64(22),
							"cidr":          podCIDR,
							"encapsulation": "VXLANCrossSubnet",
							"natOutgoing":   "Enabled",
							"nodeSelector":  "all()",
						},
					},
				},
			},
		},
	}

	return h.applyWithRetry(ctx, installation, "Installation")
}

func (h *CalicoHook) ensureAPIServer(ctx context.Context) error {
	apiServer := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.tigera.io/v1",
			"kind":       "APIServer",
			"metadata": map[string]interface{}{
				"name": "default",
			},
			"spec": map[string]interface{}{},
		},
	}

	return h.applyWithRetry(ctx, apiServer, "APIServer")
}

// Cleanup removes Calico CRs so the Tigera operator can tear down child resources.
func (h *CalicoHook) Cleanup(ctx context.Context, name string, config k4allv1alpha1.ClusterConfigSpec) error {
	h.log.Info("cleaning up calico custom resources")

	toDelete := []struct {
		apiVersion, kind, name string
	}{
		{"operator.tigera.io/v1", "APIServer", "default"},
		{"operator.tigera.io/v1", "Installation", "default"},
	}

	for _, res := range toDelete {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(res.apiVersion)
		obj.SetKind(res.kind)
		obj.SetName(res.name)
		if err := h.client.Delete(ctx, obj); err != nil {
			h.log.V(1).Info("calico cleanup delete", "kind", res.kind, "name", res.name, "error", err)
		} else {
			h.log.Info("deleted calico resource", "kind", res.kind, "name", res.name)
		}
	}

	return nil
}

func (h *CalicoHook) applyWithRetry(ctx context.Context, obj *unstructured.Unstructured, kind string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		patchData, err := json.Marshal(obj.Object)
		if err != nil {
			return err
		}

		existing := obj.DeepCopy()
		key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
		getErr := h.client.Get(waitCtx, key, existing)

		if getErr != nil {
			// CRD might not be registered yet; try creating and retry on failure
			if createErr := h.client.Create(waitCtx, obj.DeepCopy()); createErr != nil {
				h.log.V(1).Info("retrying create (CRD may not be ready)", "kind", kind, "error", createErr)
				select {
				case <-waitCtx.Done():
					return fmt.Errorf("timed out creating %s: last error: %w", kind, createErr)
				case <-ticker.C:
					continue
				}
			}
			h.log.Info("created resource", "kind", kind, "name", obj.GetName())
			return nil
		}

		// Resource exists, patch it
		return h.client.Patch(waitCtx, existing, client.RawPatch(types.MergePatchType, patchData))
	}
}
