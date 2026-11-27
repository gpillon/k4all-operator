package engine

import (
	"encoding/json"
	"fmt"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// The spec.patches of a manifest component go into its objects before their
// server-side apply: the apply writes each object once, patched
// (gpillon/k4all-operator#44). Applied after the apply instead, a patch that
// changes a field upstream's manifest also sets flipped it at every apply,
// upstream's value then the patch's: two changes of the pod template, two
// rollouts (virt-operator's node affinity, metrics-server's args, the
// multus-cni and ovs-cni images). The patched fields are k4all-operator's
// as before, now through its apply.
//
// A patch whose target is in none of the component's manifests is still
// applied to the object in the cluster after the apply (PatchApplier). The
// patches are applied in memory with the API server's own libraries:
//   - strategic: strategicpatch, with the scheme's type of the object's
//     kind; a kind the scheme lacks (a CR, which the API server takes no
//     strategic merge patch for) gets the patch as a JSON merge patch;
//   - merge (and an unset type, as PatchApplier): a JSON merge patch;
//   - json: a JSON patch.

// patchFolder folds a component's patches into the objects of its
// manifests, and remembers which patches found their object.
type patchFolder struct {
	patches []k4allv1alpha1.ComponentPatch
	folded  []bool
	scheme  *runtime.Scheme
}

func newPatchFolder(patches []k4allv1alpha1.ComponentPatch, scheme *runtime.Scheme) *patchFolder {
	return &patchFolder{patches: patches, folded: make([]bool, len(patches)), scheme: scheme}
}

// fold applies to obj, an object of the component's manifests about to be
// applied, every patch whose target it is, in the order of the patches.
// manifestNamespace is obj's namespace as its manifest gives it, before the
// component's namespace fills it in. It is called once per object: a JSON
// patch that adds to a list adds again at each call.
func (f *patchFolder) fold(obj *unstructured.Unstructured, manifestNamespace string) error {
	for i, p := range f.patches {
		if !targets(p.Target, obj, manifestNamespace) {
			continue
		}
		if err := patchObject(obj, p, f.scheme); err != nil {
			return fmt.Errorf("patch %d (%s %s): %w", i, p.Target.Kind, p.Target.Name, err)
		}
		f.folded[i] = true
	}
	return nil
}

// unfolded are the patches whose target was in no manifest, in their order:
// they are applied after the apply, to the object in the cluster.
func (f *patchFolder) unfolded() []k4allv1alpha1.ComponentPatch {
	var rest []k4allv1alpha1.ComponentPatch
	for i, p := range f.patches {
		if !f.folded[i] {
			rest = append(rest, p)
		}
	}
	return rest
}

// targets tells whether t names obj: its kind and name, its apiVersion when
// t gives one, and its namespace, as applied or as its manifest gives it.
// The CDI CR is cluster-scoped and has no namespace in cdi-cr.yaml; its
// target has none either, though the apply fills in the component's.
func targets(t k4allv1alpha1.PatchTarget, obj *unstructured.Unstructured, manifestNamespace string) bool {
	return t.Kind == obj.GetKind() && t.Name == obj.GetName() &&
		(t.APIVersion == "" || t.APIVersion == obj.GetAPIVersion()) &&
		(t.Namespace == obj.GetNamespace() || t.Namespace == manifestNamespace)
}

// patchObject applies p to obj in memory, as the API server would apply it
// to the object.
func patchObject(obj *unstructured.Unstructured, p k4allv1alpha1.ComponentPatch, scheme *runtime.Scheme) error {
	patch := []byte(p.Patch)
	if !json.Valid(patch) {
		return fmt.Errorf("invalid JSON in patch for %s/%s", p.Target.Kind, p.Target.Name)
	}
	original, err := obj.MarshalJSON()
	if err != nil {
		return err
	}
	var patched []byte
	switch p.Type {
	case k4allv1alpha1.PatchTypeStrategic:
		patched, err = strategicMergePatch(original, patch, obj.GroupVersionKind(), scheme)
	case k4allv1alpha1.PatchTypeJSON:
		var ops jsonpatch.Patch
		if ops, err = jsonpatch.DecodePatch(patch); err == nil {
			patched, err = ops.Apply(original)
		}
	default:
		patched, err = jsonpatch.MergePatch(original, patch)
	}
	if err != nil {
		return fmt.Errorf("%s patch: %w", p.Type, err)
	}
	return obj.UnmarshalJSON(patched)
}

// strategicMergePatch is the strategic merge patch of original with the
// type the scheme has for gvk, which carries the merge keys and strategies;
// a JSON merge patch when the scheme has no type for it.
func strategicMergePatch(original, patch []byte, gvk schema.GroupVersionKind, scheme *runtime.Scheme) ([]byte, error) {
	typed, err := scheme.New(gvk)
	if runtime.IsNotRegisteredError(err) {
		return jsonpatch.MergePatch(original, patch)
	}
	if err != nil {
		return nil, err
	}
	if _, ok := typed.(runtime.Unstructured); ok {
		return jsonpatch.MergePatch(original, patch)
	}
	return strategicpatch.StrategicMergePatch(original, patch, typed)
}
