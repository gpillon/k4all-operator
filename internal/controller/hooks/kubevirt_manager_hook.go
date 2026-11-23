package hooks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// kubevirtManagerNamespace is where kubevirt-manager runs.
const kubevirtManagerNamespace = "kubevirt-manager"

// KubevirtManagerHook removes kubevirt-manager, which Hangar replaces from
// k4all v2.7.0 (management: Removed). It deletes what kubevirt-manager
// v1.5.4 (k4all v2.2-v2.6) runs as, and never the VMs. It is a Remover,
// not an UninstallGuard: it keeps what VMs still use (PriorityClasses) and
// what holds the user's objects, so the removal need not wait for the VMs
// to go, and v2.7.0 removes kubevirt-manager on clusters with VMs too.
type KubevirtManagerHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
	// restConfig reaches the API server directly, for the namespace check.
	restConfig *rest.Config
	// namespace of kubevirt-manager; set apart only by tests.
	namespace string
	// How long and how often the namespace check waits for what the removal
	// is still deleting; 0 means the defaults (tests shorten them).
	settle, poll time.Duration
}

func NewKubevirtManagerHook(c client.Client, log logr.Logger) *KubevirtManagerHook {
	return &KubevirtManagerHook{client: c, log: log}
}

// kubevirtManagerRoles are the ClusterRoles of kubevirt-manager, each with a
// ClusterRoleBinding of the same name.
var kubevirtManagerRoles = []string{
	"kubevirt-manager", "kubevirt-manager-kccm", "kubevirt-manager-cas-workload", "kubevirt-manager-cas-management",
}

// Remove deletes kubevirt-manager's console (Deployment, Service,
// ServiceAccount) and its cluster-wide roles; an error there leaves the
// component Failed, and the operator tries again. Then, best effort and only
// logged, what may hold the user's things: the image catalog CRD when it is
// empty, each of its PriorityClasses that nothing names, and its namespace
// when nothing else is left in it.
func (h *KubevirtManagerHook) Remove(ctx context.Context, _ k4allv1alpha1.ComponentSpec, _ k4allv1alpha1.ClusterConfigSpec) error {
	ns := h.namespace
	if ns == "" {
		ns = kubevirtManagerNamespace
	}
	own := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kubevirt-manager"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kubevirt-manager"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "kubevirt-manager"}},
	}
	for _, name := range kubevirtManagerRoles {
		own = append(own,
			&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}},
			&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	var errs []error
	for _, obj := range own {
		if err := h.client.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %T %s: %w", obj, client.ObjectKeyFromObject(obj), err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	h.removeImageCatalog(ctx)
	h.removePriorityClasses(ctx)
	deleteNamespaceIfEmpty(ctx, h.client, h.restConfig, h.log, ns, h.settle, h.poll)
	return nil
}

// imageCatalogCRD is kubevirt-manager's image catalog; its Images are the user's.
const imageCatalogCRD = "images.kubevirt-manager.io"

var imageListGVK = schema.GroupVersionKind{Group: "kubevirt-manager.io", Version: "v1", Kind: "ImageList"}

// removeImageCatalog deletes the CRD of kubevirt-manager's image catalog,
// unless an Image is in it: deleting the CRD would delete them.
func (h *KubevirtManagerHook) removeImageCatalog(ctx context.Context) {
	images := &unstructured.UnstructuredList{}
	images.SetGroupVersionKind(imageListGVK)
	err := h.client.List(ctx, images, client.Limit(1))
	switch {
	case err == nil && len(images.Items) > 0:
		h.log.Info("CRD kept: the image catalog holds images", "crd", imageCatalogCRD,
			"image", images.Items[0].GetNamespace()+"/"+images.Items[0].GetName())
		return
	case err != nil && !notServed(err):
		h.log.Info("CRD kept: cannot tell whether the image catalog is empty", "crd", imageCatalogCRD, "error", err.Error())
		return
	}
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: imageCatalogCRD}}
	if err := h.client.Delete(ctx, crd); err != nil && !apierrors.IsNotFound(err) {
		h.log.Info("cannot delete the image catalog CRD", "crd", imageCatalogCRD, "error", err.Error())
	}
}

// kubevirtManagerPriorityClasses are the PriorityClasses kubevirt-manager
// ships; its VM form puts one of them in the VMs it creates.
var kubevirtManagerPriorityClasses = []string{"vm-standard", "vm-preemptible"}

// priorityClassUsers are where a workload names its PriorityClass: pods
// (every running VM has one), VMs, and the templates KubeVirt makes VMs or
// their instances from. A kind the cluster does not serve names none.
var priorityClassUsers = []struct {
	list schema.GroupVersionKind
	path []string
}{
	{schema.GroupVersionKind{Version: "v1", Kind: "PodList"}, []string{"spec", "priorityClassName"}},
	{schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineList"},
		[]string{"spec", "template", "spec", "priorityClassName"}},
	{schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstanceList"},
		[]string{"spec", "priorityClassName"}},
	{schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstanceReplicaSetList"},
		[]string{"spec", "template", "spec", "priorityClassName"}},
	{schema.GroupVersionKind{Group: "pool.kubevirt.io", Version: "v1beta1", Kind: "VirtualMachinePoolList"},
		[]string{"spec", "virtualMachineTemplate", "spec", "template", "spec", "priorityClassName"}},
	{schema.GroupVersionKind{Group: "pool.kubevirt.io", Version: "v1alpha1", Kind: "VirtualMachinePoolList"},
		[]string{"spec", "virtualMachineTemplate", "spec", "template", "spec", "priorityClassName"}},
}

// removePriorityClasses deletes kubevirt-manager's PriorityClasses that
// nothing names. A VM that names a missing one can no longer start, nor move
// to another host: its new pod is refused.
func (h *KubevirtManagerHook) removePriorityClasses(ctx context.Context) {
	used, err := h.priorityClassesInUse(ctx)
	if err != nil {
		h.log.Info("PriorityClasses kept: cannot tell whether VMs use them",
			"priorityClasses", kubevirtManagerPriorityClasses, "error", err.Error())
		return
	}
	for _, name := range kubevirtManagerPriorityClasses {
		if user, ok := used[name]; ok {
			h.log.Info("PriorityClass kept: in use", "priorityClass", name, "usedBy", user)
			continue
		}
		pc := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := h.client.Delete(ctx, pc); err != nil && !apierrors.IsNotFound(err) {
			h.log.Info("cannot delete the PriorityClass", "priorityClass", name, "error", err.Error())
		}
	}
}

// priorityClassesInUse maps each PriorityClass named by a workload to one of
// them ("<kind> <namespace>/<name>").
func (h *KubevirtManagerHook) priorityClassesInUse(ctx context.Context) (map[string]string, error) {
	used := map[string]string{}
	for _, u := range priorityClassUsers {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(u.list)
		if err := h.client.List(ctx, list); err != nil {
			if notServed(err) {
				continue
			}
			return nil, fmt.Errorf("list %s: %w", u.list.Kind, err)
		}
		for _, item := range list.Items {
			name, _, _ := unstructured.NestedString(item.Object, u.path...)
			if name != "" && used[name] == "" {
				used[name] = fmt.Sprintf("%s %s/%s", strings.TrimSuffix(u.list.Kind, "List"), item.GetNamespace(), item.GetName())
			}
		}
	}
	return used, nil
}

// notServed: the API server (or the client's scheme) does not know the kind.
func notServed(err error) bool {
	return meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err)
}
