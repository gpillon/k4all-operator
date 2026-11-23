package hooks

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CDIHook lets the reconciler follow CDI's upgradePath: CDI, like KubeVirt,
// upgrades one minor version at a time.
type CDIHook struct {
	BaseHook
	client client.Client
}

func NewCDIHook(c client.Client) *CDIHook {
	return &CDIHook{client: c}
}

// ValuesRevision implements ValuesRevisioner: every cluster applies CDI
// again once after the operator upgrade (virtApplyRevision).
func (h *CDIHook) ValuesRevision() string { return virtApplyRevision }

// RunningVersion reads the CDI CR, where cdi-operator reports the version it
// runs and the one it is rolling out.
func (h *CDIHook) RunningVersion(ctx context.Context) (string, bool, error) {
	return operatorCRVersion(ctx, h.client, "cdi.kubevirt.io/v1beta1", "CDI",
		types.NamespacedName{Name: "cdi"}, "observedVersion", "targetVersion")
}
