package hooks

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gpillon/k4all-operator/internal/engine"
)

// PrepareUninstall has virt-operator uninstall KubeVirt before k4all deletes
// virt-operator: see operatorCR.uninstallStep.
func (h *VirtHook) PrepareUninstall(ctx context.Context, _ string) (string, error) {
	return kubeVirtOperator.uninstallStep(ctx, h.client)
}

// PrepareUninstall has cdi-operator uninstall CDI before k4all deletes
// cdi-operator: see operatorCR.uninstallStep.
func (h *CDIHook) PrepareUninstall(ctx context.Context, _ string) (string, error) {
	return cdiOperator.uninstallStep(ctx, h.client)
}

// operatorCR is the CR whose deletion makes an operator uninstall what it
// created at run time, outside the component's manifests: CRDs, webhooks,
// APIServices, its workers.
type operatorCR struct {
	operator string
	gvk      schema.GroupVersionKind
	key      types.NamespacedName
	// created selects the APIServices and webhook configurations the
	// operator created; cleaner removes them once the CR is gone.
	created, cleaner string
}

var (
	// virt-operator (v1.9.1, kubevirt.go syncDeletion) removes everything it
	// created, CRDs first, then virt-handler, virt-api, virt-controller, the
	// webhooks and the APIServices, and only then the finalizer: the CR goes
	// last. Its objects carry app.kubernetes.io/managed-by.
	kubeVirtOperator = operatorCR{
		operator: "virt-operator",
		gvk:      schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "KubeVirt"},
		key:      kubeVirtKey,
		created:  "app.kubernetes.io/managed-by in (virt-operator,kubevirt-operator)",
		cleaner:  "virt-operator",
	}
	// cdi-operator (v1.66.1, controller-lifecycle-operator-sdk ReconcileDelete)
	// deletes its controller and worker pods, then the finalizer: what it
	// created is owned by the CDI CR, and the garbage collector deletes it
	// once the CR is gone. Its objects carry the label cdi.kubevirt.io.
	cdiOperator = operatorCR{
		operator: "cdi-operator",
		gvk:      schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "CDI"},
		key:      types.NamespacedName{Name: "cdi"},
		created:  "cdi.kubevirt.io",
		cleaner:  "the garbage collector",
	}
)

// leftoverKinds are what an operator leaves behind that breaks the whole
// cluster: an APIService without its service fails discovery, so no
// namespace can be deleted; a webhook without its service refuses what it
// validates (failurePolicy Fail).
var leftoverKinds = []schema.GroupVersionKind{
	{Group: "apiregistration.k8s.io", Version: "v1", Kind: "APIServiceList"},
	{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfigurationList"},
	{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "MutatingWebhookConfigurationList"},
}

// uninstallStep takes the operator's uninstall one step, without waiting:
//   - the CR is there: it gets spec.uninstallStrategy
//     BlockUninstallIfWorkloadsExist (the operators' default deletes the
//     VMs and DataVolumes with their CRDs) and is deleted;
//   - the CR is being deleted: pending, with what is left;
//   - the CR is gone: pending while the operator's APIServices or webhook
//     configurations are left, done ("") once they are gone too, or when the
//     CR's kind is not served at all.
func (o operatorCR) uninstallStep(ctx context.Context, c client.Client) (string, error) {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(o.gvk)
	err := c.Get(ctx, o.key, cr)
	switch {
	case kindNotServed(err):
		left, err := o.leftovers(ctx, c)
		if err != nil || len(left) == 0 {
			return "", err
		}
		return fmt.Sprintf("waiting for %s to remove what %s %s created: %s", o.cleaner, o.gvk.Kind, o.name(), left), nil
	case err != nil:
		return "", fmt.Errorf("get %s %s: %w", o.gvk.Kind, o.name(), err)
	case cr.GetDeletionTimestamp() == nil:
		if err := o.blockUninstallIfWorkloadsExist(ctx, c, cr); err != nil {
			return "", err
		}
		uid, version := cr.GetUID(), cr.GetResourceVersion()
		err := c.Delete(ctx, cr, client.Preconditions{UID: &uid, ResourceVersion: &version})
		switch {
		case apierrors.IsConflict(err):
			return fmt.Sprintf("%s %s changed as k4all-operator deleted it: deleting it again", o.gvk.Kind, o.name()), nil
		case err != nil && !apierrors.IsNotFound(err):
			return "", fmt.Errorf("delete %s %s: %w", o.gvk.Kind, o.name(), err)
		}
		return fmt.Sprintf("waiting for %s to remove %s %s: deleted", o.operator, o.gvk.Kind, o.name()), nil
	default:
		pending := fmt.Sprintf("waiting for %s to remove %s %s", o.operator, o.gvk.Kind, o.name())
		if phase, _, _ := unstructured.NestedString(cr.Object, "status", "phase"); phase != "" {
			pending += " (phase " + phase + ")"
		}
		left, err := o.leftovers(ctx, c)
		if err != nil {
			return "", err
		}
		if left != "" {
			pending += ": " + left
		}
		return pending, nil
	}
}

func (o operatorCR) name() string {
	if o.key.Namespace == "" {
		return o.key.Name
	}
	return o.key.String()
}

// blockUninstallIfWorkloadsExist makes sure cr has spec.uninstallStrategy
// BlockUninstallIfWorkloadsExist before it is deleted. Unset (k4all's CRs
// had none) KubeVirt v1.9.1 deletes with its CRDs every VM (its delete
// webhook only refuses with the field set), and CDI v1.66.1 every
// DataVolume and PVC it owns (its CRDs are owned by the CR, and its delete
// webhook checks DataVolumes only with the field set). The field is read
// back: a CRD that does not serve it drops it, and the CR is then not
// deleted.
func (o operatorCR) blockUninstallIfWorkloadsExist(ctx context.Context, c client.Client, cr *unstructured.Unstructured) error {
	if strategy, _, _ := unstructured.NestedString(cr.Object, "spec", "uninstallStrategy"); strategy == engine.BlockUninstallIfWorkloadsExist {
		return nil
	}
	patch := fmt.Sprintf(`{"spec":{"uninstallStrategy":%q}}`, engine.BlockUninstallIfWorkloadsExist)
	if err := c.Patch(ctx, cr, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		return fmt.Errorf("set the uninstallStrategy of %s %s: %w", o.gvk.Kind, o.name(), err)
	}
	if err := c.Get(ctx, o.key, cr); err != nil {
		return fmt.Errorf("read %s %s back: %w", o.gvk.Kind, o.name(), err)
	}
	if strategy, _, _ := unstructured.NestedString(cr.Object, "spec", "uninstallStrategy"); strategy != engine.BlockUninstallIfWorkloadsExist {
		return fmt.Errorf("%s %s does not keep spec.uninstallStrategy %s (it has %q): not deleted, "+
			"its operator could delete the workloads with it", o.gvk.Kind, o.name(), engine.BlockUninstallIfWorkloadsExist, strategy)
	}
	return nil
}

// leftoverExamples is how many leftovers a message names.
const leftoverExamples = 4

// leftovers names the operator's APIServices and webhook configurations
// still there ("APIService v1.subresources.kubevirt.io, …"); empty when none
// is.
func (o operatorCR) leftovers(ctx context.Context, c client.Client) (string, error) {
	selector, err := labels.Parse(o.created)
	if err != nil {
		return "", err
	}
	var left []string
	for _, kind := range leftoverKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(kind)
		if err := c.List(ctx, list, client.MatchingLabelsSelector{Selector: selector}); err != nil {
			if kindNotServed(err) {
				continue
			}
			return "", fmt.Errorf("list %s: %w", strings.TrimSuffix(kind.Kind, "List"), err)
		}
		for _, item := range list.Items {
			left = append(left, strings.TrimSuffix(kind.Kind, "List")+" "+item.GetName())
		}
	}
	if len(left) > leftoverExamples {
		left = append(left[:leftoverExamples:leftoverExamples], fmt.Sprintf("… (%d in all)", len(left)))
	}
	return strings.Join(left, ", "), nil
}
