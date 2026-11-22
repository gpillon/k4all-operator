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

// operatorCRVersion reads the version a KubeVirt-style operator reports in
// its CR's status: observedField is the version it runs, targetField the one
// it is rolling out. It is ready once the two match and the CR is Available,
// neither Progressing nor Degraded. No CR, or no CRD, means not installed; a
// CR with no observed version is an error, as nothing tells what runs.
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
	if observed == "" {
		return "", false, fmt.Errorf("%s %s reports no version yet: is its operator running?", kind, key)
	}
	target, _, _ := unstructured.NestedString(cr.Object, "status", targetField)
	ready := observed == target &&
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
