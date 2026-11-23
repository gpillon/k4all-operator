package engine

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// BlockUninstallIfWorkloadsExist is the uninstallStrategy of the KubeVirt and
// CDI CRs that k4all wants: their operators then refuse the deletion of the
// CR while VMs (KubeVirt) or DataVolumes (CDI) exist. Unset, as in
// upstream's kubevirt-cr.yaml and cdi-cr.yaml, the deletion is allowed and
// takes them with it: virt-operator v1.9.1 deletes its CRDs first, and the
// garbage collector deletes CDI v1.66.1's, owned by the CDI CR.
const BlockUninstallIfWorkloadsExist = "BlockUninstallIfWorkloadsExist"

// uninstallStrategyKinds are the CRs applied with spec.uninstallStrategy
// BlockUninstallIfWorkloadsExist.
var uninstallStrategyKinds = map[[2]string]bool{
	{"kubevirt.io", "KubeVirt"}: true,
	{"cdi.kubevirt.io", "CDI"}:  true,
}

// blockUninstallIfWorkloadsExist sets spec.uninstallStrategy on obj, the
// object about to be applied, when it is a KubeVirt or CDI CR. The apply
// owns the field: a value set by hand is replaced.
func blockUninstallIfWorkloadsExist(obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()
	if !uninstallStrategyKinds[[2]string{gvk.Group, gvk.Kind}] {
		return nil
	}
	return unstructured.SetNestedField(obj.Object, BlockUninstallIfWorkloadsExist, "spec", "uninstallStrategy")
}
