package hooks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// UninstallBlocked refuses the uninstall of KubeVirt while VMs exist: see
// vmsBlock.
func (h *VirtHook) UninstallBlocked(ctx context.Context, name string) (string, error) {
	return vmsBlock(ctx, h.client, name)
}

// UninstallBlocked refuses the uninstall of kubevirt-manager while VMs
// exist: see vmsBlock and KubevirtManagerHook.
func (h *KubevirtManagerHook) UninstallBlocked(ctx context.Context, name string) (string, error) {
	return vmsBlock(ctx, h.client, name)
}

// workloadKind is a kind whose objects keep a component installed while
// they exist.
type workloadKind struct {
	list      schema.GroupVersionKind
	one, many string // how the refusal names one object, and several
}

// vmKinds are KubeVirt's VMs: a VirtualMachine, and the
// VirtualMachineInstance that runs it (or runs alone).
var vmKinds = []workloadKind{
	{schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineList"},
		"VirtualMachine", "VirtualMachines"},
	{schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstanceList"},
		"VirtualMachineInstance", "VirtualMachineInstances"},
}

// vmsBlock is why the component name may not be uninstalled: a
// VirtualMachine or a VirtualMachineInstance exists, in any namespace. An
// uninstall deletes the objects of the component's manifests, the operator
// first: for KubeVirt, virt-operator, then the kubevirt namespace with
// virt-controller and virt-handler, from under the running VMs; for
// kubevirt-manager, the PriorityClasses its VMs name (KubevirtManagerHook).
// Empty once no VM is left, or the cluster does not serve the kinds.
func vmsBlock(ctx context.Context, c client.Client, name string) (string, error) {
	found := make([]workloadsFound, 0, len(vmKinds))
	for _, kind := range vmKinds {
		names, err := listWorkloads(ctx, c, kind.list, nil)
		if err != nil {
			return "", err
		}
		found = append(found, workloadsFound{kind, names})
	}
	return refusal(name, found), nil
}

// UninstallBlocked refuses the uninstall of CDI while its volumes exist:
// see cdiVolumesBlock.
func (h *CDIHook) UninstallBlocked(ctx context.Context, name string) (string, error) {
	return cdiVolumesBlock(ctx, h.client, name)
}

var (
	dataVolumes = workloadKind{schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolumeList"},
		"DataVolume", "DataVolumes"}
	cdiPVCs = workloadKind{schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaimList"},
		"PVC made by CDI", "PVCs made by CDI"}
)

// madeByCDI tells a PVC that CDI made. CDI v1.66 gives every PVC it makes
// for a DataVolume the annotation cdi.kubevirt.io/createdForDataVolume, and
// the label app: containerized-data-importer, which the DataVolume's own
// labels may replace; its scratch PVCs have the label only.
func madeByCDI(pvc *unstructured.Unstructured) bool {
	_, forDataVolume := pvc.GetAnnotations()["cdi.kubevirt.io/createdForDataVolume"]
	return forDataVolume || pvc.GetLabels()["app"] == "containerized-data-importer"
}

// cdiVolumesBlock is why the component name may not be uninstalled: a
// DataVolume, or a PVC that CDI made, exists in any namespace. An uninstall
// deletes cdi-operator, then the cdi namespace with CDI's controller and
// API server: an import, upload or clone stops half-way, and the
// DataVolumes are left with nothing to reconcile them. A PVC that outlived
// its DataVolume is still a disk CDI made: CDI stays until the user decides
// what to do with it. Empty once none is left, or the cluster does not
// serve the kinds.
func cdiVolumesBlock(ctx context.Context, c client.Client, name string) (string, error) {
	dvs, err := listWorkloads(ctx, c, dataVolumes.list, nil)
	if err != nil {
		return "", err
	}
	pvcs, err := listWorkloads(ctx, c, cdiPVCs.list, madeByCDI)
	if err != nil {
		return "", err
	}
	return refusal(name, []workloadsFound{{dataVolumes, dvs}, {cdiPVCs, pvcs}}), nil
}

// workloadsFound are the objects of a kind that exist, as namespace/name.
type workloadsFound struct {
	kind  workloadKind
	names []string
}

// listWorkloads lists, in every namespace, the objects of the list kind
// that keep (nil: all), as namespace/name. A kind the cluster does not
// serve has none. It reads the API server, not the manager's cache.
func listWorkloads(
	ctx context.Context, c client.Client, list schema.GroupVersionKind, keep func(*unstructured.Unstructured) bool,
) ([]string, error) {
	objs := &unstructured.UnstructuredList{}
	objs.SetGroupVersionKind(list)
	if err := c.List(ctx, objs); err != nil {
		if kindNotServed(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", strings.TrimSuffix(list.Kind, "List"), err)
	}
	var names []string
	for i := range objs.Items {
		if keep == nil || keep(&objs.Items[i]) {
			names = append(names, objs.Items[i].GetNamespace()+"/"+objs.Items[i].GetName())
		}
	}
	return names, nil
}

// kindNotServed: the cluster does not serve the kind, now (no match) or any
// more (the client's discovery is older than the CRD's removal).
func kindNotServed(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}

// refusalExamples is how many objects of a kind the refusal names.
const refusalExamples = 3

// refusal is why the component name may not be uninstalled while the
// objects found exist, naming how many of each kind and the first few:
// "2 VirtualMachines (default/a, default/b) exist: delete them or set
// kubevirt Unmanaged first". Empty when none exists.
func refusal(name string, found []workloadsFound) string {
	parts := make([]string, 0, len(found))
	total := 0
	for _, f := range found {
		if len(f.names) == 0 {
			continue
		}
		total += len(f.names)
		sort.Strings(f.names)
		examples := f.names
		if len(examples) > refusalExamples {
			examples = append(examples[:refusalExamples:refusalExamples], "…")
		}
		noun := f.kind.many
		if len(f.names) == 1 {
			noun = f.kind.one
		}
		parts = append(parts, fmt.Sprintf("%d %s (%s)", len(f.names), noun, strings.Join(examples, ", ")))
	}
	if total == 0 {
		return ""
	}
	if total == 1 {
		return fmt.Sprintf("%s exists: delete it or set %s Unmanaged first", parts[0], name)
	}
	return fmt.Sprintf("%s exist: delete them or set %s Unmanaged first", strings.Join(parts, " and "), name)
}
