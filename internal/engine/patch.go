package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PatchApplier applies post-install patches to Kubernetes resources.
type PatchApplier struct {
	client client.Client
	log    logr.Logger
}

func NewPatchApplier(c client.Client, log logr.Logger) *PatchApplier {
	return &PatchApplier{client: c, log: log}
}

// ApplyAll applies all patches in order, waiting for each target resource to appear.
func (p *PatchApplier) ApplyAll(ctx context.Context, patches []k4allv1alpha1.ComponentPatch) error {
	for i, patch := range patches {
		if err := p.applyOne(ctx, patch); err != nil {
			return fmt.Errorf("patch %d (%s %s): %w", i, patch.Target.Kind, patch.Target.Name, err)
		}
	}
	return nil
}

func (p *PatchApplier) applyOne(ctx context.Context, patch k4allv1alpha1.ComponentPatch) error {
	target := patch.Target

	obj := &unstructured.Unstructured{}
	obj.SetKind(target.Kind)
	if target.APIVersion != "" {
		obj.SetAPIVersion(target.APIVersion)
	}

	key := types.NamespacedName{
		Name:      target.Name,
		Namespace: target.Namespace,
	}

	// Wait for the target resource to exist (up to 2 minutes)
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := p.waitForResource(waitCtx, key, obj); err != nil {
		return fmt.Errorf("waiting for %s/%s: %w", target.Kind, target.Name, err)
	}

	patchData := []byte(patch.Patch)

	var patchType types.PatchType
	switch patch.Type {
	case k4allv1alpha1.PatchTypeMerge:
		patchType = types.MergePatchType
	case k4allv1alpha1.PatchTypeStrategic:
		patchType = types.StrategicMergePatchType
	case k4allv1alpha1.PatchTypeJSON:
		patchType = types.JSONPatchType
	default:
		patchType = types.MergePatchType
	}

	// Validate the patch is valid JSON
	if !json.Valid(patchData) {
		return fmt.Errorf("invalid JSON in patch for %s/%s", target.Kind, target.Name)
	}

	p.log.Info("applying patch", "kind", target.Kind, "name", target.Name,
		"namespace", target.Namespace, "type", patch.Type)

	return p.client.Patch(ctx, obj, client.RawPatch(patchType, patchData))
}

func (p *PatchApplier) waitForResource(ctx context.Context, key types.NamespacedName, obj *unstructured.Unstructured) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		err := p.client.Get(ctx, key, obj)
		if err == nil {
			return nil
		}
		if !errors.IsNotFound(err) {
			return err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s %s to appear", obj.GetKind(), key)
		case <-ticker.C:
		}
	}
}
