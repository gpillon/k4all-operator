package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VirtHook handles post-install logic for KubeVirt:
//   - Enables/disables software emulation based on ClusterConfig
type VirtHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewVirtHook(c client.Client, log logr.Logger) *VirtHook {
	return &VirtHook{client: c, log: log}
}

func (h *VirtHook) PostInstall(ctx context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) error {
	emulation := config.Features.Virt.Emulation
	if emulation == "" {
		emulation = "auto"
	}

	useEmulation := false
	switch emulation {
	case "true":
		useEmulation = true
	case "false":
		useEmulation = false
	case "auto":
		useEmulation = !hasKVMDevice()
	}

	h.log.Info("configuring KubeVirt emulation", "emulation", emulation, "useEmulation", useEmulation)
	return h.patchKubeVirt(ctx, useEmulation)
}

func (h *VirtHook) patchKubeVirt(ctx context.Context, useEmulation bool) error {
	kv := &unstructured.Unstructured{}
	kv.SetAPIVersion("kubevirt.io/v1")
	kv.SetKind("KubeVirt")

	key := types.NamespacedName{Name: "kubevirt", Namespace: "kubevirt"}

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		err := h.client.Get(waitCtx, key, kv)
		if err == nil {
			break
		}
		if !errors.IsNotFound(err) {
			return fmt.Errorf("get KubeVirt CR: %w", err)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for KubeVirt CR")
		case <-ticker.C:
		}
	}

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"configuration": map[string]interface{}{
				"developerConfiguration": map[string]interface{}{
					"useEmulation": useEmulation,
				},
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	return h.client.Patch(ctx, kv, client.RawPatch(types.MergePatchType, patchBytes))
}

func hasKVMDevice() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}
