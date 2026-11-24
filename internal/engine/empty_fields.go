package engine

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// emptyFieldKinds are the upstream CRs applied without their empty fields:
// kubevirt-cr.yaml sets certificateRotateStrategy: {}, customizeComponents:
// {}, workloadUpdateStrategy: {} and featureGates: []. Applied as they are,
// they show as {} leaves in k4all-operator's managedFields: the apply owns
// fields it sets nothing in, and a console reading managedFields takes them
// for k4all's (spec R5, §16.3). Left out, the CR is the same to its operator:
// KubeVirt and CDI read their CR into Go structs, where an empty struct or
// list and an absent one are the same value.
var emptyFieldKinds = map[[2]string]bool{
	{"kubevirt.io", "KubeVirt"}: true,
	{"cdi.kubevirt.io", "CDI"}:  true,
}

// stripEmptyFields removes from the spec of obj, the object about to be
// applied, when it is a KubeVirt or CDI CR, every empty map and list, and the
// maps left empty by that. The apply then leaves them out: the API server
// deletes such a field k4all-operator owned from an earlier apply, unless
// another manager owns something there (a workloadUpdateStrategy set by
// hand keeps what it was set to).
func stripEmptyFields(obj *unstructured.Unstructured) {
	gvk := obj.GroupVersionKind()
	if !emptyFieldKinds[[2]string{gvk.Group, gvk.Kind}] {
		return
	}
	if spec, ok := obj.Object["spec"].(map[string]interface{}); ok {
		pruneEmpty(spec)
	}
}

// pruneEmpty removes from m every empty list and every map that is empty once
// pruned, and reports whether m is left empty. Lists are not entered.
func pruneEmpty(m map[string]interface{}) bool {
	for k, v := range m {
		switch v := v.(type) {
		case map[string]interface{}:
			if pruneEmpty(v) {
				delete(m, k)
			}
		case []interface{}:
			if len(v) == 0 {
				delete(m, k)
			}
		}
	}
	return len(m) == 0
}
