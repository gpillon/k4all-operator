package hooks

import (
	"context"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type NMStateHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewNMStateHook(c client.Client, log logr.Logger) *NMStateHook {
	return &NMStateHook{client: c, log: log}
}

var nmstateCRDs = []string{
	"nodenetworkconfigurationenactments.nmstate.io",
	"nodenetworkconfigurationpolicies.nmstate.io",
	"nodenetworkstates.nmstate.io",
	"nmstates.nmstate.io",
}

func (h *NMStateHook) Cleanup(ctx context.Context, name string, config k4allv1alpha1.ClusterConfigSpec) error {
	h.log.Info("cleaning up nmstate resources")

	// Delete the NMState CR first so the operator can teardown child resources.
	nmstateCR := &unstructured.Unstructured{}
	nmstateCR.SetAPIVersion("nmstate.io/v1")
	nmstateCR.SetKind("NMState")
	nmstateCR.SetName("nmstate")
	if err := h.client.Delete(ctx, nmstateCR); err != nil {
		if !errors.IsNotFound(err) {
			h.log.V(1).Info("nmstate CR delete (best-effort)", "error", err)
		}
	} else {
		h.log.Info("deleted NMState CR")
	}

	// Clean up leftover CRDs that the operator sometimes leaves behind.
	for _, crdName := range nmstateCRDs {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion("apiextensions.k8s.io/v1")
		obj.SetKind("CustomResourceDefinition")
		obj.SetName(crdName)
		if err := h.client.Delete(ctx, obj); err != nil {
			if !errors.IsNotFound(err) {
				h.log.V(1).Info("nmstate CRD delete (best-effort)", "crd", crdName, "error", err)
			}
		} else {
			h.log.Info("deleted nmstate CRD", "crd", crdName)
		}
	}

	return nil
}
