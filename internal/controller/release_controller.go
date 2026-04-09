/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/engine"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ReleaseReconciler watches ReleaseManifest and ClusterConfig CRs
// and reconciles the cluster to match the desired state.
type ReleaseReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RestConfig *rest.Config
	ToolsImage string
	Log        logr.Logger

	engine  *engine.Engine
	hookReg *hooks.Registry
}

// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=releasemanifests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=releasemanifests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=releasemanifests/finalizers,verbs=update
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterconfigs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=nodeconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=nodeconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;serviceaccounts;services;pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings;roles;rolebindings,verbs=get;list;watch;create;update;patch;delete;bind;escalate
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations;validatingwebhookconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;ingressclasses;networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses;csidrivers,verbs=get;list;watch;create;update;patch;delete

func (r *ReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("releasemanifest", req.Name)

	rm := &k4allv1alpha1.ReleaseManifest{}
	if err := r.Get(ctx, req.NamespacedName, rm); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	config, err := r.getClusterConfig(ctx)
	if err != nil {
		log.Error(err, "failed to get ClusterConfig")
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "ClusterConfigNotFound", err.Error())
		_ = r.Status().Update(ctx, rm)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if r.engine == nil {
		r.engine = engine.New(r.Client, r.RestConfig, r.Log.WithName("engine"), r.ToolsImage)
		r.hookReg = hooks.NewRegistry(r.Client, r.Log)
	}

	active := engine.FilterActiveComponents(rm.Spec.Components, config.Spec)
	removed := engine.FilterRemovedComponents(rm.Spec.Components)

	graph := engine.NewDependencyGraph(active)
	order, err := graph.TopologicalSort()
	if err != nil {
		log.Error(err, "dependency resolution failed")
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "DependencyCycle", err.Error())
		_ = r.Status().Update(ctx, rm)
		return ctrl.Result{}, err
	}

	if rm.Status.Components == nil {
		rm.Status.Components = make(map[string]k4allv1alpha1.ComponentStatus)
	}

	// Mark Unmanaged components
	for name, spec := range rm.Spec.Components {
		if spec.Management == k4allv1alpha1.ComponentManagementUnmanaged {
			rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
				State:              k4allv1alpha1.ComponentStateUnmanaged,
				Message:            "lifecycle not managed by operator",
				LastTransitionTime: metav1.Now(),
			}
		}
	}

	// Handle components disabled by feature gates or CNI switch (but still Managed).
	// If they were previously installed, uninstall them first.
	for name, spec := range rm.Spec.Components {
		mgmt := spec.Management
		if mgmt == k4allv1alpha1.ComponentManagementUnmanaged || mgmt == k4allv1alpha1.ComponentManagementRemoved {
			continue
		}
		if _, isActive := active[name]; isActive {
			continue
		}

		prevStatus := rm.Status.Components[name]
		wasInstalled := prevStatus.State == k4allv1alpha1.ComponentStateInstalled ||
			r.engine.IsInstalled(ctx, name, spec, &prevStatus)

		if wasInstalled {
			log.Info("uninstalling component disabled by feature gate or CNI switch", "name", name)
			if err := r.engine.UninstallComponent(ctx, name, spec); err != nil {
				log.Error(err, "uninstall failed", "name", name)
			}
			if hook := r.hookReg.Get(name); hook != nil {
				if err := hook.Cleanup(ctx, name, config.Spec); err != nil {
					log.Error(err, "cleanup hook failed", "name", name)
				}
			}
		}

		rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
			State:              k4allv1alpha1.ComponentStateSkipped,
			Message:            "disabled by ClusterConfig feature gate",
			LastTransitionTime: metav1.Now(),
		}
	}

	// Reconcile active components in dependency order
	allOK := true
	for _, name := range order {
		spec := active[name]
		currentStatus := rm.Status.Components[name]

		if r.engine.IsInstalled(ctx, name, spec, &currentStatus) {
			continue
		}

		extraValues := r.computeExtraValues(name, config.Spec)
		log.Info("reconciling component", "name", name, "type", spec.Type, "version", spec.Version)

		// Run PreInstall hook to let hooks modify values or prepare the cluster
		if hook := r.hookReg.Get(name); hook != nil {
			var hookErr error
			extraValues, hookErr = hook.PreInstall(ctx, name, spec, config.Spec, extraValues)
			if hookErr != nil {
				log.Error(hookErr, "pre-install hook failed", "name", name)
				rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
					State:              k4allv1alpha1.ComponentStateFailed,
					Message:            fmt.Sprintf("pre-install hook failed: %v", hookErr),
					LastTransitionTime: metav1.Now(),
				}
				allOK = false
				continue
			}
		}

		st, err := r.engine.ReconcileComponent(ctx, name, spec, extraValues)
		if err != nil {
			log.Error(err, "component reconciliation failed", "name", name)
			allOK = false
			rm.Status.Components[name] = *st
			continue
		}
		rm.Status.Components[name] = *st

		// Run PostInstall hook for post-install configuration
		if hook := r.hookReg.Get(name); hook != nil {
			if hookErr := hook.PostInstall(ctx, spec, config.Spec); hookErr != nil {
				log.Error(hookErr, "post-install hook failed", "name", name)
				st.Message = fmt.Sprintf("installed but hook failed: %v", hookErr)
				rm.Status.Components[name] = *st
			}
		}
	}

	// Handle explicitly Removed components
	for name, spec := range removed {
		prevStatus, hasPrev := rm.Status.Components[name]
		if hasPrev && (prevStatus.State == k4allv1alpha1.ComponentStateInstalled || prevStatus.State == k4allv1alpha1.ComponentStateFailed) {
			log.Info("removing component (management=Removed)", "name", name)
			if err := r.engine.UninstallComponent(ctx, name, spec); err != nil {
				log.Error(err, "uninstall failed for removed component", "name", name)
			}
			if hook := r.hookReg.Get(name); hook != nil {
				if err := hook.Cleanup(ctx, name, config.Spec); err != nil {
					log.Error(err, "cleanup hook failed for removed component", "name", name)
				}
			}
		}
		rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
			State:              k4allv1alpha1.ComponentStateRemoved,
			Message:            "removed by management policy",
			LastTransitionTime: metav1.Now(),
		}
	}

	installed := 0
	for _, st := range rm.Status.Components {
		if st.State == k4allv1alpha1.ComponentStateInstalled {
			installed++
		}
	}
	if allOK {
		engine.SetReadyCondition(rm, metav1.ConditionTrue, "AllComponentsReconciled",
			fmt.Sprintf("%d/%d components installed", installed, len(active)))
	} else {
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "ComponentsFailed",
			"one or more components failed to reconcile")
	}

	if err := r.Status().Update(ctx, rm); err != nil {
		log.Error(err, "failed to update ReleaseManifest status")
		return ctrl.Result{}, err
	}

	// Update ClusterConfig status
	r.updateClusterConfigStatus(ctx, config, rm)

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *ReleaseReconciler) updateClusterConfigStatus(ctx context.Context, config *k4allv1alpha1.ClusterConfig, rm *k4allv1alpha1.ReleaseManifest) {
	config.Status.ObservedGeneration = config.Generation
	config.Status.ActiveCNI = config.Spec.Networking.CNI.Type
	config.Status.LastReconcileTime = metav1.Now()

	config.Status.Features = k4allv1alpha1.FeaturesStatus{}
	if config.Spec.Features.Virt.Enabled {
		config.Status.Features.VirtReady = isComponentInstalled(rm, "kubevirt")
	}
	if config.Spec.Features.ArgoCD.Enabled {
		config.Status.Features.ArgoCDReady = isComponentInstalled(rm, "argocd")
	}
	if config.Spec.Features.OVSCNI.Enabled {
		config.Status.Features.OVSCNIReady = isComponentInstalled(rm, "ovs-cni")
	}

	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               "Reconciled",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: config.Generation,
		Reason:             "Synced",
		Message:            fmt.Sprintf("CNI=%s, reconciled at %s", config.Status.ActiveCNI, config.Status.LastReconcileTime.Format(time.RFC3339)),
	})

	if err := r.Status().Update(ctx, config); err != nil {
		r.Log.Error(err, "failed to update ClusterConfig status")
	}
}

func isComponentInstalled(rm *k4allv1alpha1.ReleaseManifest, name string) bool {
	if rm.Status.Components == nil {
		return false
	}
	st, ok := rm.Status.Components[name]
	return ok && st.State == k4allv1alpha1.ComponentStateInstalled
}

func (r *ReleaseReconciler) getClusterConfig(ctx context.Context) (*k4allv1alpha1.ClusterConfig, error) {
	list := &k4allv1alpha1.ClusterConfigList{}
	if err := r.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list ClusterConfigs: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("no ClusterConfig found")
	}
	return &list.Items[0], nil
}

func (r *ReleaseReconciler) computeExtraValues(name string, config k4allv1alpha1.ClusterConfigSpec) map[string]interface{} {
	extra := make(map[string]interface{})

	if override, ok := config.ComponentOverrides[name]; ok && override.Values != nil {
		overrideValues := make(map[string]interface{})
		if err := override.Values.UnmarshalJSON(override.Values.Raw); err == nil {
			for k, v := range overrideValues {
				extra[k] = v
			}
		}
	}

	return extra
}

func (r *ReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&k4allv1alpha1.ReleaseManifest{}).
		Watches(&k4allv1alpha1.ClusterConfig{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				list := &k4allv1alpha1.ReleaseManifestList{}
				if err := r.List(ctx, list); err != nil {
					return nil
				}
				var requests []reconcile.Request
				for _, rm := range list.Items {
					requests = append(requests, reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&rm),
					})
				}
				return requests
			},
		)).
		Named("release").
		Complete(r)
}
