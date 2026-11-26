package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/metadata"
)

var vmGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}

// clusterRequest asks the metadata controller to look at every VM: the
// settings changed, or the operator started.
var clusterRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "k4all-metadata"}}

const (
	// metadataResync reads the Secrets a VM names again (the controller
	// does not watch Secrets).
	metadataResync = 10 * time.Minute
	// metadataRetry is sooner, while a Secret the VM names is missing.
	metadataRetry = time.Minute
	// psaEnforce is PodSecurity's enforced level of a namespace.
	psaEnforce = "pod-security.kubernetes.io/enforce"
)

// What the metadata controller needs (MetadataReconciler).
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=releasemanifests,verbs=get;list;watch

// MetadataReconciler renders, for each VM the metadata service serves, the
// Secret k4all-md-<vm> in the VM's namespace: what the VM's metadata server
// serves (the server has no API access), from the VM's spec (spec §11.2,
// D9). The Secret is owned by the VM and goes with it.
//
// What it needs, cluster-wide: VirtualMachines get/list/watch;
// Namespaces get/list/watch (PodSecurity labels); Secrets get, create,
// update, delete and no list or watch: a VM's Secrets are read one by one
// when it is reconciled, never cached; Events create/patch. It writes only
// Secrets named k4all-md-<vm> that it made (labels, type), never another.
type MetadataReconciler struct {
	// Client writes the Secrets.
	Client client.Client
	// Reader reads VMs, Namespaces and the ClusterConfig: the cache.
	Reader client.Reader
	// APIReader reads Secrets, uncached.
	APIReader client.Reader
	Recorder  record.EventRecorder
	Log       logr.Logger
	// Settings are the metadata settings; nil reads the ClusterConfig.
	Settings func(ctx context.Context) (hooks.MetadataSettings, error)
	// APIPollInterval is how often the controller asks whether KubeVirt
	// serves VirtualMachines yet (one minute when 0).
	APIPollInterval time.Duration

	ctrl     controller.Controller
	cache    cache.Cache
	mapper   meta.RESTMapper
	watching atomic.Bool
	// settings as the last cluster reconcile read them, for the VM watch's
	// filter; nil before it.
	settings atomic.Pointer[hooks.MetadataSettings]

	mu sync.Mutex
	// said is what each VM was told (Warning reason -> message), so an
	// Event is put once while it stays the same.
	said map[types.UID]map[string]string
}

// SetupWithManager watches the ClusterConfig; VMs and Namespaces are
// watched once KubeVirt serves VirtualMachines (the operator may install
// KubeVirt after it starts, or never).
func (r *MetadataReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.cache, r.mapper = mgr.GetCache(), mgr.GetRESTMapper()
	start := make(chan event.GenericEvent, 1)
	start <- event.GenericEvent{Object: &k4allv1alpha1.ClusterConfig{}}
	toCluster := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{clusterRequest}
	})
	c, err := ctrl.NewControllerManagedBy(mgr).
		Named("metadata").
		WatchesRawSource(source.Channel(start, toCluster)).
		WatchesRawSource(source.Kind(mgr.GetCache(), client.Object(&k4allv1alpha1.ClusterConfig{}), toCluster,
			predicate.GenerationChangedPredicate{})).
		Build(r)
	if err != nil {
		return err
	}
	r.ctrl = c
	return nil
}

func (r *MetadataReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	settings, err := r.readSettings(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if req == clusterRequest {
		return r.reconcileAll(ctx, settings)
	}
	vm := newVM()
	switch err := r.Reader.Get(ctx, req.NamespacedName, vm); {
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, nil // its Secret goes with it (owner reference)
	case err != nil:
		return ctrl.Result{}, err
	}
	return r.reconcileVM(ctx, settings, vm)
}

func (r *MetadataReconciler) readSettings(ctx context.Context) (hooks.MetadataSettings, error) {
	var s hooks.MetadataSettings
	if r.Settings != nil {
		var err error
		s, err = r.Settings(ctx)
		if err != nil {
			return s, err
		}
	} else {
		list := &k4allv1alpha1.ClusterConfigList{}
		if err := r.Reader.List(ctx, list); err != nil {
			return s, fmt.Errorf("list ClusterConfigs: %w", err)
		}
		// The release says whether k4all-metadata is kept for the virt
		// group (metadataSettings). Its uninstall is seen at the next
		// reconcile of each VM, within metadataResync: the virt group
		// goes only once its VMs, and their Secrets with them, are gone.
		releases := &k4allv1alpha1.ReleaseManifestList{}
		if err := r.Reader.List(ctx, releases); err != nil {
			return s, fmt.Errorf("list ReleaseManifests: %w", err)
		}
		var rm *k4allv1alpha1.ReleaseManifest
		if len(releases.Items) > 0 {
			rm = &releases.Items[0]
		}
		if len(list.Items) > 0 {
			s = metadataSettings(list.Items[0].Spec, rm)
		}
	}
	r.settings.Store(&s)
	return s, nil
}

// reconcileAll starts the VM watch once KubeVirt serves VMs, then renders
// (or removes) the Secret of every VM.
func (r *MetadataReconciler) reconcileAll(ctx context.Context, s hooks.MetadataSettings) (ctrl.Result, error) {
	if !r.watching.Load() {
		started, err := r.startWatches()
		if err != nil || !started {
			poll := r.APIPollInterval
			if poll <= 0 {
				poll = time.Minute
			}
			return ctrl.Result{RequeueAfter: poll}, err
		}
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmGVK.GroupVersion().WithKind("VirtualMachineList"))
	if err := r.Reader.List(ctx, list); err != nil {
		return ctrl.Result{}, fmt.Errorf("list VirtualMachines: %w", err)
	}
	var errs []error
	for i := range list.Items {
		if _, err := r.reconcileVM(ctx, s, &list.Items[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return ctrl.Result{}, errors.Join(errs...)
}

// startWatches watches VMs and Namespaces, once KubeVirt serves VMs.
func (r *MetadataReconciler) startWatches() (bool, error) {
	if r.ctrl == nil {
		return false, nil
	}
	if _, err := r.mapper.RESTMapping(vmGVK.GroupKind(), vmGVK.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	if err := r.ctrl.Watch(source.Kind(r.cache, newVM(), &handler.TypedEnqueueRequestForObject[*unstructured.Unstructured]{},
		predicate.TypedFuncs[*unstructured.Unstructured]{
			CreateFunc: func(e event.TypedCreateEvent[*unstructured.Unstructured]) bool { return r.mayServe(e.Object) },
			UpdateFunc: func(e event.TypedUpdateEvent[*unstructured.Unstructured]) bool {
				return r.mayServe(e.ObjectOld) || r.mayServe(e.ObjectNew)
			},
			// The Secret goes with its VM: nothing to do.
			DeleteFunc:  func(event.TypedDeleteEvent[*unstructured.Unstructured]) bool { return false },
			GenericFunc: func(event.TypedGenericEvent[*unstructured.Unstructured]) bool { return false },
		})); err != nil {
		return false, err
	}
	if err := r.ctrl.Watch(source.Kind(r.cache, &corev1.Namespace{},
		handler.TypedEnqueueRequestsFromMapFunc(r.vmsOf),
		predicate.TypedFuncs[*corev1.Namespace]{
			CreateFunc: func(event.TypedCreateEvent[*corev1.Namespace]) bool { return false },
			UpdateFunc: func(e event.TypedUpdateEvent[*corev1.Namespace]) bool {
				return e.ObjectOld.Labels[psaEnforce] != e.ObjectNew.Labels[psaEnforce]
			},
			DeleteFunc:  func(event.TypedDeleteEvent[*corev1.Namespace]) bool { return false },
			GenericFunc: func(event.TypedGenericEvent[*corev1.Namespace]) bool { return false },
		})); err != nil {
		return false, err
	}
	r.watching.Store(true)
	r.Log.Info("watching VirtualMachines for the metadata service")
	return true, nil
}

// vmsOf are the VMs of a namespace whose PodSecurity level changed.
func (r *MetadataReconciler) vmsOf(ctx context.Context, ns *corev1.Namespace) []reconcile.Request {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmGVK.GroupVersion().WithKind("VirtualMachineList"))
	if err := r.Reader.List(ctx, list, client.InNamespace(ns.Name)); err != nil {
		r.Log.Error(err, "list the VMs of a namespace", "namespace", ns.Name)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, vm := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&vm)})
	}
	return reqs
}

// mayServe filters VM events: a VM the settings serve, or any before the
// settings are known.
func (r *MetadataReconciler) mayServe(vm *unstructured.Unstructured) bool {
	s := r.settings.Load()
	return s == nil || (s.Enabled && inMetadataScope(*s, vm))
}

func newVM() *unstructured.Unstructured {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	return vm
}

// optInLabel is the VM template's metadata label.
func optInLabel(vm *unstructured.Unstructured) string {
	v, _, _ := unstructured.NestedString(vm.Object, "spec", "template", "metadata", "labels", metadata.LabelOptIn)
	return v
}

// inMetadataScope says whether the settings' scope takes the VM, by its
// template's label (the pod's, which the admission policy selects on).
func inMetadataScope(s hooks.MetadataSettings, vm *unstructured.Unstructured) bool {
	if s.ServesAll() {
		return optInLabel(vm) != metadata.OptInDisabled
	}
	return optInLabel(vm) == metadata.OptInEnabled
}

func (r *MetadataReconciler) reconcileVM(ctx context.Context, s hooks.MetadataSettings, vm *unstructured.Unstructured) (ctrl.Result, error) {
	log := r.Log.WithValues("vm", client.ObjectKeyFromObject(vm))
	if !s.Enabled || !inMetadataScope(s, vm) {
		r.warn(vm, nil)
		return ctrl.Result{}, r.dropSecret(ctx, vm)
	}
	ns := &corev1.Namespace{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: vm.GetNamespace()}, ns); err != nil {
		return ctrl.Result{}, fmt.Errorf("get namespace %s: %w", vm.GetNamespace(), err)
	}
	if level := ns.Labels[psaEnforce]; level == "baseline" || level == "restricted" {
		// D8: the policy does not inject here; one who asked is told.
		warnings := map[string]string{}
		if optInLabel(vm) == metadata.OptInEnabled {
			warnings["MetadataNotServed"] = fmt.Sprintf(
				"namespace %s enforces PodSecurity %q, which forbids the NET_ADMIN capability the metadata service "+
					"needs in the VM's pod: 169.254.169.254 is not served to this VM; give it a cloud-init disk "+
					"(cloudInitNoCloud or cloudInitConfigDrive) instead", vm.GetNamespace(), level)
		}
		r.warn(vm, warnings)
		return ctrl.Result{}, r.dropSecret(ctx, vm)
	}
	warnings := map[string]string{}
	if asset := userAssetTag(vm); asset != "" {
		warnings["MetadataChassisAsset"] = fmt.Sprintf(
			"the VM's chassis asset tag is %q: cloud-init looks for the OpenStack metadata service only with %q, "+
				"so this VM's cloud-init does not use 169.254.169.254 (EC2 clients such as cirros do); set it to %q, "+
				"or remove it and k4all sets it", asset, metadata.ChassisAsset, metadata.ChassisAsset)
	}

	data, problems, err := renderVMMetadata(ctx, r.APIReader, vm)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := metadataSecret(vm, data, problems)
	current, err := getSecret(ctx, r.APIReader, desired.Namespace, desired.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case current == nil:
		if err := r.Client.Create(ctx, desired, client.FieldOwner(metadataFieldOwner)); err != nil {
			return ctrl.Result{}, fmt.Errorf("create Secret %s/%s: %w", desired.Namespace, desired.Name, err)
		}
		log.Info("metadata Secret created", "secret", desired.Name)
	case !isK4allMetadataSecret(current):
		warnings["MetadataSecretTaken"] = fmt.Sprintf(
			"Secret %s exists and is not k4all's: the metadata service serves this VM no user-data, keys or "+
				"instance-id until it is renamed or deleted", desired.Name)
		r.warn(vm, warnings)
		return ctrl.Result{RequeueAfter: metadataResync}, nil
	case !sameMetadataSecret(current, desired):
		current.Data, current.OwnerReferences = desired.Data, desired.OwnerReferences
		for k, v := range desired.Labels {
			current.Labels[k] = v
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		delete(current.Annotations, metadataMessageAnnotation)
		for k, v := range desired.Annotations {
			current.Annotations[k] = v
		}
		if err := r.Client.Update(ctx, current, client.FieldOwner(metadataFieldOwner)); err != nil {
			return ctrl.Result{}, fmt.Errorf("update Secret %s/%s: %w", desired.Namespace, desired.Name, err)
		}
		log.Info("metadata Secret updated", "secret", desired.Name)
	}
	if len(problems) > 0 {
		warnings["MetadataIncomplete"] = "the metadata service serves this VM without what is missing: " +
			desired.Annotations[metadataMessageAnnotation]
		r.warn(vm, warnings)
		return ctrl.Result{RequeueAfter: metadataRetry}, nil
	}
	r.warn(vm, warnings)
	return ctrl.Result{RequeueAfter: metadataResync}, nil
}

// userAssetTag is the chassis asset tag of the VM's spec when it is set and
// is not the one cloud-init needs; "" otherwise. Only for a VM with a
// masquerade interface: the others are not served at all.
func userAssetTag(vm *unstructured.Unstructured) string {
	asset, _, _ := unstructured.NestedString(vm.Object, "spec", "template", "spec", "domain", "chassis", "asset")
	if asset == "" || asset == metadata.ChassisAsset {
		return ""
	}
	ifaces, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "interfaces")
	for _, i := range ifaces {
		if iface, ok := i.(map[string]any); ok && iface["masquerade"] != nil {
			return asset
		}
	}
	return ""
}

// metadataFieldOwner writes what k4all owns.
const metadataFieldOwner = "k4all-operator"

func sameMetadataSecret(current, desired *corev1.Secret) bool {
	if !reflect.DeepEqual(current.Data, desired.Data) ||
		!reflect.DeepEqual(current.OwnerReferences, desired.OwnerReferences) ||
		current.Annotations[metadataMessageAnnotation] != desired.Annotations[metadataMessageAnnotation] {
		return false
	}
	for k, v := range desired.Labels {
		if current.Labels[k] != v {
			return false
		}
	}
	return true
}

// dropSecret deletes the VM's Secret if k4all made it.
func (r *MetadataReconciler) dropSecret(ctx context.Context, vm *unstructured.Unstructured) error {
	s, err := getSecret(ctx, r.APIReader, vm.GetNamespace(), metadata.SecretName(vm.GetName()))
	if err != nil || s == nil || !isK4allMetadataSecret(s) {
		return err
	}
	err = r.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	r.Log.Info("metadata Secret deleted", "vm", client.ObjectKeyFromObject(vm))
	return nil
}

// warn puts on the VM a Warning Event for each of warnings (reason ->
// message) it was not told yet, or told otherwise; what is no longer in
// warnings is forgotten, to be said again if it comes back.
func (r *MetadataReconciler) warn(vm *unstructured.Unstructured, warnings map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.said == nil {
		r.said = map[types.UID]map[string]string{}
	}
	told := r.said[vm.GetUID()]
	for reason, message := range warnings {
		if told[reason] != message && r.Recorder != nil {
			r.Recorder.Event(vm, corev1.EventTypeWarning, reason, message)
		}
	}
	if len(warnings) == 0 {
		delete(r.said, vm.GetUID())
		return
	}
	r.said[vm.GetUID()] = warnings
}
