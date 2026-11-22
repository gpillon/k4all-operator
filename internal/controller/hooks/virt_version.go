package hooks

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RunningVersion reads the KubeVirt CR: virt-operator reports there the
// version it runs and the one it is rolling out.
func (h *VirtHook) RunningVersion(ctx context.Context) (string, bool, error) {
	return operatorCRVersion(ctx, h.client, "kubevirt.io/v1", "KubeVirt",
		types.NamespacedName{Namespace: "kubevirt", Name: "kubevirt"},
		"observedKubeVirtVersion", "targetKubeVirtVersion")
}

// operatorCRVersion reads the version a KubeVirt-style operator reports in
// its CR's status: observedField is the version it runs, targetField the one
// it is rolling out. It is ready once the two match and the CR is Available,
// neither Progressing nor Degraded. No CR, or no CRD, means not installed.
func operatorCRVersion(
	ctx context.Context, c client.Client, apiVersion, kind string, key types.NamespacedName, observedField, targetField string,
) (string, bool, error) {
	cr := &unstructured.Unstructured{}
	cr.SetAPIVersion(apiVersion)
	cr.SetKind(kind)
	if err := c.Get(ctx, key, cr); err != nil {
		if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get %s %s: %w", kind, key, err)
	}
	observed, _, _ := unstructured.NestedString(cr.Object, "status", observedField)
	target, _, _ := unstructured.NestedString(cr.Object, "status", targetField)
	ready := observed != "" && observed == target &&
		conditionIs(cr, "Available", "True") && !conditionIs(cr, "Progressing", "True") && !conditionIs(cr, "Degraded", "True")
	return observed, ready, nil
}

// conditionIs tells whether the CR's status has the condition typ at status.
func conditionIs(cr *unstructured.Unstructured, typ, status string) bool {
	conditions, _, _ := unstructured.NestedSlice(cr.Object, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if ok && cond["type"] == typ {
			return cond["status"] == status
		}
	}
	return false
}

// CDIHook lets the reconciler follow CDI's upgradePath: CDI, like KubeVirt,
// upgrades one minor version at a time.
type CDIHook struct {
	BaseHook
	client client.Client
}

func NewCDIHook(c client.Client) *CDIHook {
	return &CDIHook{client: c}
}

// RunningVersion reads the CDI CR, where cdi-operator reports the version it
// runs and the one it is rolling out.
func (h *CDIHook) RunningVersion(ctx context.Context) (string, bool, error) {
	return operatorCRVersion(ctx, h.client, "cdi.kubevirt.io/v1beta1", "CDI",
		types.NamespacedName{Name: "cdi"}, "observedVersion", "targetVersion")
}
