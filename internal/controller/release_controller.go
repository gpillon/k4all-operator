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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/engine"
	"github.com/gpillon/k4all-operator/internal/migrations"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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

const ForceResyncAnnotation = "k4all.magesgate.com/force-resync"

// AllowDowngradeAnnotation on the ReleaseManifest names the downgrades a
// person allows, as "component=version" entries separated by commas.
// Without one the operator never applies a version lower than the one a
// component has installed (k4all ADR-0010): the entry allows exactly that
// version, so a stale annotation allows nothing else.
const AllowDowngradeAnnotation = "k4all.magesgate.com/allow-downgrade"

// UpgradeInProgressAnnotation is set by the host tool k4all-upgrade on its own
// Node while an OS/Kubernetes hop is running (value "<from>-><to>"). While any
// Node carries it, components are not reconciled: no installs, no upgrades, no
// operator self-update and no re-resolution of "latest" versions.
const UpgradeInProgressAnnotation = "k4all.magesgate.com/upgrade-in-progress"

// upgradeStepInterval is how soon the next upgradePath step is applied.
const upgradeStepInterval = 10 * time.Second

// upgradePollInterval is how often a paused reconcile checks whether the hop is over.
const upgradePollInterval = 30 * time.Second

// uninstallPollInterval is how soon a reconcile comes back while an
// operator uninstalls what it created (an Uninstalling component).
const uninstallPollInterval = 15 * time.Second

// anyUninstalling tells whether a component is Uninstalling, or queued to
// be once another is gone.
func anyUninstalling(rm *k4allv1alpha1.ReleaseManifest) bool {
	for _, st := range rm.Status.Components {
		if st.State == k4allv1alpha1.ComponentStateUninstalling || strings.HasPrefix(st.Message, uninstallQueuedPrefix) {
			return true
		}
	}
	return false
}

// uninstallRefusedPollInterval is how soon a reconcile comes back while a
// component's uninstall is refused, to start it soon after the user deleted
// the last VM or DataVolume: nothing watches those. Each reconcile then lists
// every VM, VMI, DataVolume and PVC of the cluster from the API server, so a
// minute: short next to deleting VMs by hand, light on the API server.
const uninstallRefusedPollInterval = time.Minute

// anyUninstallRefused tells whether a component's uninstall is refused.
func anyUninstallRefused(rm *k4allv1alpha1.ReleaseManifest) bool {
	for _, st := range rm.Status.Components {
		if strings.HasPrefix(st.Message, uninstallRefusedPrefix) {
			return true
		}
	}
	return false
}

// resyncInterval is how soon a reconcile comes back when nothing asks for sooner.
const resyncInterval = 5 * time.Minute

// hookFailedPrefix starts the message of an installed component whose
// PostInstall or Sync failed.
const hookFailedPrefix = "installed but hook failed: "

// hookUnfinished tells whether an installed component's message says that
// its hook failed: the hook's PostInstall runs again.
func hookUnfinished(message string) bool {
	return strings.HasPrefix(message, hookFailedPrefix)
}

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
		_ = r.patchStatus(ctx, rm)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	upgrading, err := r.nodesUpgrading(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if upgrading != "" {
		log.Info("component reconcile paused", "upgrading", upgrading)
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "UpgradeInProgress",
			"node upgrade in progress: "+upgrading)
		if err := r.patchStatus(ctx, rm); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: upgradePollInterval}, nil
	}

	if r.engine == nil {
		r.engine = engine.New(r.Client, r.RestConfig, r.Log.WithName("engine"), r.ToolsImage)
	}
	if r.hookReg == nil {
		r.hookReg = hooks.NewRegistry(r.Client, r.Log, hooks.WithRestConfig(r.RestConfig))
	}

	// The release may need the ClusterConfig in a new shape before its
	// components are applied from it.
	env := migrations.Env{Client: r.Client, RestConfig: r.RestConfig, Log: log.WithName("migrations")}
	migrated, err := migrations.RunWith(ctx, env, rm, config)
	var ahead *migrations.AheadError
	if goerrors.As(err, &ahead) {
		// An older operator after a newer one: no component until a person
		// looks. They may need what the unknown migrations did.
		log.Error(err, "component reconcile stopped")
		setMigrationsAhead(&rm.Status.Conditions, rm.Generation, err.Error())
		engine.SetReadyCondition(rm, metav1.ConditionFalse, migrationsAheadCondition, err.Error())
		if err := r.patchStatus(ctx, rm); err != nil {
			return ctrl.Result{}, err
		}
		setMigrationsAhead(&config.Status.Conditions, config.Generation, err.Error())
		r.patchClusterConfigStatus(ctx, config)
		return ctrl.Result{RequeueAfter: resyncInterval}, nil
	}
	meta.RemoveStatusCondition(&rm.Status.Conditions, migrationsAheadCondition)
	meta.RemoveStatusCondition(&config.Status.Conditions, migrationsAheadCondition)
	if err != nil {
		log.Error(err, "ClusterConfig migration failed")
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "MigrationFailed", err.Error())
		_ = r.patchStatus(ctx, rm)
		return ctrl.Result{}, err
	} else if migrated {
		log.Info("ClusterConfig migrated", "migrations", config.Annotations[migrations.AppliedAnnotation])
	}

	// Handle force-resync annotation.
	// Value = component name to resync, or "all" to resync everything.
	if resyncTarget, ok := rm.GetAnnotations()[ForceResyncAnnotation]; ok {
		return r.handleForceResync(ctx, log, rm, resyncTarget)
	}

	active := engine.FilterActiveComponents(rm.Spec.Components, config.Spec)
	removed := engine.FilterRemovedComponents(rm.Spec.Components)

	graph := engine.NewDependencyGraph(active)
	order, err := graph.TopologicalSort()
	if err != nil {
		log.Error(err, "dependency resolution failed")
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "DependencyCycle", err.Error())
		_ = r.patchStatus(ctx, rm)
		return ctrl.Result{}, err
	}

	if rm.Status.Components == nil {
		rm.Status.Components = make(map[string]k4allv1alpha1.ComponentStatus)
	}

	markUnmanagedComponents(rm)
	before := maps.Clone(rm.Status.Components)
	// Removals first: a replacement can then take over what the removed
	// component held (traefik: the node's ports and the "nginx" IngressClass).
	removedOK := r.reconcileRemovedComponents(ctx, log, rm, removed, config.Spec)
	inactiveOK := r.reconcileInactiveComponents(ctx, log, rm, active, config.Spec)
	activeOK, stepsLeft, hookRequeue := r.reconcileActiveComponents(ctx, log, rm, order, active, config.Spec)
	keepFailedSince(before, rm)
	setDowngradeRefused(rm, active)
	setReadySummary(rm, inactiveOK && activeOK && removedOK, len(active))

	if err := r.patchStatus(ctx, rm); err != nil {
		log.Error(err, "failed to update ReleaseManifest status")
		return ctrl.Result{}, err
	}

	// Update ClusterConfig status
	r.updateClusterConfigStatus(ctx, config, rm)

	requeue := resyncInterval
	if stepsLeft {
		requeue = upgradeStepInterval
	}
	if anyUninstalling(rm) {
		requeue = sooner(requeue, uninstallPollInterval)
	}
	if anyUninstallRefused(rm) {
		requeue = sooner(requeue, uninstallRefusedPollInterval)
	}
	requeue = sooner(requeue, failedRetry(rm, time.Now()))
	return ctrl.Result{RequeueAfter: sooner(requeue, hookRequeue)}, nil
}

// handleForceResync resets the matching Installed components to Pending so the
// next reconcile reinstalls them, then removes the force-resync annotation.
func (r *ReleaseReconciler) handleForceResync(
	ctx context.Context, log logr.Logger, rm *k4allv1alpha1.ReleaseManifest, resyncTarget string,
) (ctrl.Result, error) {
	log.Info("force-resync annotation detected", "target", resyncTarget)

	if rm.Status.Components == nil {
		rm.Status.Components = make(map[string]k4allv1alpha1.ComponentStatus)
	}

	resetCount := 0
	for name, st := range rm.Status.Components {
		if st.State != k4allv1alpha1.ComponentStateInstalled {
			continue
		}
		if resyncTarget != "all" && name != resyncTarget {
			continue
		}
		if spec, exists := rm.Spec.Components[name]; exists &&
			(spec.Management == k4allv1alpha1.ComponentManagementRemoved ||
				spec.Management == k4allv1alpha1.ComponentManagementUnmanaged) {
			continue
		}
		rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
			State:              k4allv1alpha1.ComponentStatePending,
			Message:            "force resync requested",
			InstalledVersion:   st.InstalledVersion,
			LastTransitionTime: metav1.Now(),
		}
		resetCount++
	}

	if resetCount > 0 {
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "ForceResync",
			fmt.Sprintf("force resync in progress (%d components)", resetCount))
	} else if resyncTarget != "all" {
		log.Info("force-resync target not found or not in Installed state", "target", resyncTarget)
	}

	if err := r.patchStatus(ctx, rm); err != nil {
		log.Error(err, "failed to update status during force-resync")
		return ctrl.Result{}, err
	}

	// Remove the annotation via merge-patch to avoid resourceVersion conflicts.
	patch := client.RawPatch(types.MergePatchType,
		[]byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, ForceResyncAnnotation)))
	if err := r.Patch(ctx, rm, patch); err != nil {
		log.Error(err, "failed to remove force-resync annotation")
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// markUnmanagedComponents records the Unmanaged state for every component
// whose lifecycle is not handled by the operator. A component already marked
// keeps its status as it is, so a reconcile that changes nothing writes
// nothing.
func markUnmanagedComponents(rm *k4allv1alpha1.ReleaseManifest) {
	const message = "lifecycle not managed by operator"
	for name, spec := range rm.Spec.Components {
		if spec.Management != k4allv1alpha1.ComponentManagementUnmanaged {
			continue
		}
		if st := rm.Status.Components[name]; st.State == k4allv1alpha1.ComponentStateUnmanaged && st.Message == message {
			continue
		}
		rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
			State:              k4allv1alpha1.ComponentStateUnmanaged,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		}
	}
}

// reconcileInactiveComponents handles components disabled by feature gates or
// a CNI switch (but still Managed): if they were previously installed they are
// uninstalled first, then marked Skipped. They go as removalPlan says: the
// components of one feature gate as one, all kept while one refuses, and in
// reverse dependency order. It returns false if any uninstall failed or was
// refused.
func (r *ReleaseReconciler) reconcileInactiveComponents(
	ctx context.Context, log logr.Logger, rm *k4allv1alpha1.ReleaseManifest,
	active map[string]k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
) bool {
	inactive := make(map[string]k4allv1alpha1.ComponentSpec)
	for name, spec := range rm.Spec.Components {
		mgmt := spec.Management
		if mgmt == k4allv1alpha1.ComponentManagementUnmanaged || mgmt == k4allv1alpha1.ComponentManagementRemoved {
			continue
		}
		if _, isActive := active[name]; !isActive {
			inactive[name] = spec
		}
	}
	installed := make(map[string]bool, len(inactive))
	for name, spec := range inactive {
		prevStatus := rm.Status.Components[name]
		// A refused downgrade is Failed but still runs its installedVersion.
		installed[name] = prevStatus.State == k4allv1alpha1.ComponentStateInstalled ||
			uninstallUnfinished(prevStatus) || strings.HasPrefix(prevStatus.Message, downgradeRefusedPrefix) ||
			r.engine.IsInstalled(ctx, name, spec, &prevStatus)
	}
	plan := planRemoval(inactive)
	refused := r.uninstallRefusals(ctx, log, rm, plan, installed)

	allOK := true
	gone := make(map[string]bool, len(inactive))
	for _, name := range plan.order {
		spec := inactive[name]
		prevStatus := rm.Status.Components[name]
		if installed[name] {
			if !uninstallStarted(prevStatus) {
				if reason, ok := refused[name]; ok {
					rm.Status.Components[name] = refusedUninstall(prevStatus, reason)
					allOK = false
					continue
				}
				if first := plan.waitsFor(name, gone); first != "" {
					rm.Status.Components[name] = queuedUninstall(prevStatus, first)
					continue
				}
			}
			if st := r.prepareUninstall(ctx, log, name, prevStatus); st != nil {
				rm.Status.Components[name] = *st
				allOK = allOK && st.State != k4allv1alpha1.ComponentStateFailed
				continue
			}
			log.Info("uninstalling component disabled by feature gate or CNI switch", "name", name)
			uninstallErrs := r.uninstallWithCleanup(ctx, log, name, spec, prevStatus.InstalledVersion, config)
			if len(uninstallErrs) > 0 {
				rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
					State: k4allv1alpha1.ComponentStateFailed,
					Message: fmt.Sprintf("uninstall failed (feature gate disabled): %s",
						strings.Join(uninstallErrs, "; ")),
					InstalledVersion:   prevStatus.InstalledVersion,
					LastTransitionTime: metav1.Now(),
				}
				allOK = false
				continue
			}
		}
		gone[name] = true

		if prevStatus.State == k4allv1alpha1.ComponentStateSkipped && prevStatus.Message == skippedMessage {
			continue
		}
		rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
			State:              k4allv1alpha1.ComponentStateSkipped,
			Message:            skippedMessage,
			LastTransitionTime: metav1.Now(),
		}
	}
	return allOK
}

// skippedMessage is the message of a component its feature gate or the CNI
// leaves out.
const skippedMessage = "disabled by ClusterConfig feature gate"

// reconcileActiveComponents installs or upgrades the active components in
// dependency order, running the PreInstall/PostInstall hooks around each one.
// A component with an upgradePath moves by one step per call. It returns false
// if any component failed, stepsLeft when some component is mid-path, and
// hookRequeue when a hook's Sync asked to run again that soon (0: none asked).
func (r *ReleaseReconciler) reconcileActiveComponents(
	ctx context.Context, log logr.Logger, rm *k4allv1alpha1.ReleaseManifest, order []string,
	active map[string]k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
) (allOK, stepsLeft bool, hookRequeue time.Duration) {
	allOK = true
	for _, name := range order {
		spec := active[name]
		currentStatus := rm.Status.Components[name]
		hash := r.appliedHash(name, spec, config)

		// A static pod whose manifest the operator did not write stays left
		// alone until the component's spec changes.
		if currentStatus.State == k4allv1alpha1.ComponentStateUnmanaged && currentStatus.AppliedHash == hash {
			continue
		}
		// Back in the release while its operator uninstalls it (KubeVirt,
		// CDI): the install waits until that is over, then starts afresh.
		// Applied to the CR being deleted, it would be Installed once the
		// operator is done, with nothing running.
		if uninstallStarted(currentStatus) {
			if st := r.prepareUninstall(ctx, log, name, currentStatus); st != nil {
				rm.Status.Components[name] = *st
				allOK = allOK && st.State != k4allv1alpha1.ComponentStateFailed
				continue
			}
			currentStatus = k4allv1alpha1.ComponentStatus{}
		}
		if r.engine.IsInstalled(ctx, name, spec, &currentStatus) &&
			(currentStatus.AppliedHash == hash || currentStatus.AppliedHash == "") {
			// No hash: installed by an operator that did not record hashes.
			// Adopt it instead of reinstalling every component on operator upgrade.
			currentStatus.AppliedHash = hash
			// Back in the release after a refused or queued uninstall:
			// nothing is being uninstalled now.
			if strings.HasPrefix(currentStatus.Message, uninstallRefusedPrefix) ||
				strings.HasPrefix(currentStatus.Message, uninstallQueuedPrefix) {
				currentStatus.Message = engine.InstalledMessage
			}
			// A PostInstall that failed runs again until it succeeds.
			postInstall := hookUnfinished(currentStatus.Message)
			hookRequeue = sooner(hookRequeue, r.runHooks(ctx, log, name, spec, config, &currentStatus, postInstall))
			rm.Status.Components[name] = currentStatus
			continue
		}

		if why := downgradeRefused(rm, name, spec, currentStatus); why != "" {
			allOK = false
			if currentStatus.State != k4allv1alpha1.ComponentStateFailed {
				currentStatus.State = k4allv1alpha1.ComponentStateFailed
				currentStatus.LastTransitionTime = metav1.Now()
			}
			currentStatus.Message = why
			rm.Status.Components[name] = currentStatus
			continue
		}

		start, startErr := r.upgradePathStart(ctx, name, spec, currentStatus)
		var wait waitForStep
		switch {
		case goerrors.As(startErr, &wait):
			stepsLeft = true
			if currentStatus.State != k4allv1alpha1.ComponentStateUpgrading &&
				currentStatus.State != k4allv1alpha1.ComponentStatePending {
				currentStatus.State = k4allv1alpha1.ComponentStatePending
				currentStatus.LastTransitionTime = metav1.Now()
			}
			currentStatus.Message = wait.reason
			rm.Status.Components[name] = currentStatus
			continue
		case startErr != nil:
			log.Error(startErr, "the upgrade path cannot start", "name", name)
			allOK = false
			// No installedVersion: the next reconcile reads what runs again.
			rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
				State:              k4allv1alpha1.ComponentStateFailed,
				Message:            startErr.Error(),
				LastTransitionTime: metav1.Now(),
			}
			continue
		}
		toApply, intermediate := engine.UpgradeStep(spec, start)
		log.Info("reconciling component", "name", name, "type", spec.Type, "version", toApply.Version,
			"target", spec.Version)

		extraValues, valuesErr := r.componentValues(ctx, name, toApply, config)
		if valuesErr != nil {
			log.Error(valuesErr, "preparing component values failed", "name", name)
			rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
				State:              k4allv1alpha1.ComponentStateFailed,
				Message:            fmt.Sprintf("preparing values failed: %v", valuesErr),
				LastTransitionTime: metav1.Now(),
			}
			allOK = false
			continue
		}

		var opts []engine.ReconcileOption
		if intermediate {
			opts = append(opts, engine.WaitUntilReady())
		}
		st, err := r.engine.ReconcileComponent(ctx, name, toApply, extraValues, opts...)
		if goerrors.Is(err, engine.ErrHelmReleaseBusy) {
			// A helm operation that may still run (a previous leader
			// finishing it): come back soon, as on an upgrade step.
			stepsLeft = true
			if currentStatus.State != k4allv1alpha1.ComponentStatePending {
				currentStatus.State = k4allv1alpha1.ComponentStatePending
				currentStatus.LastTransitionTime = metav1.Now()
			}
			currentStatus.Message = err.Error()
			rm.Status.Components[name] = currentStatus
			continue
		}
		if err != nil {
			log.Error(err, "component reconciliation failed", "name", name)
			allOK = false
			// Keep the version still running: the next attempt resumes the path from it.
			st.InstalledVersion = currentStatus.InstalledVersion
			if intermediate {
				st.Message = fmt.Sprintf("upgrade step %s (towards %s): %s", toApply.Version, spec.Version, st.Message)
			}
			rm.Status.Components[name] = *st
			continue
		}
		st.AppliedHash = hash
		if intermediate {
			stepsLeft = true
			st.State = k4allv1alpha1.ComponentStateUpgrading
			st.Message = fmt.Sprintf("upgrade step %s installed, upgrading towards %s", toApply.Version, spec.Version)
		}

		hookRequeue = sooner(hookRequeue, r.runHooks(ctx, log, name, spec, config, st, true))
		rm.Status.Components[name] = *st
	}
	return allOK, stepsLeft, hookRequeue
}

// runHooks runs the component's hook after an apply, or on an installed
// component that is up to date: its PostInstall when postInstall is set, then
// its Sync, when the hook is a Syncer. It returns how soon Sync wants to run
// again (0: at the periodic reconcile). The first failure goes into st's
// message; when neither fails, an installed component's earlier hook failure
// is cleared.
func (r *ReleaseReconciler) runHooks(
	ctx context.Context, log logr.Logger, name string, spec k4allv1alpha1.ComponentSpec,
	config k4allv1alpha1.ClusterConfigSpec, st *k4allv1alpha1.ComponentStatus, postInstall bool,
) time.Duration {
	hook := r.hookReg.Get(name)
	if hook == nil {
		return 0
	}
	var unfinished error
	note := func(step string, err error) {
		if err == nil {
			return
		}
		log.Error(err, "hook failed", "name", name, "step", step)
		if unfinished == nil {
			unfinished = err
		}
	}
	if postInstall {
		note("PostInstall", hook.PostInstall(ctx, spec, config))
	}
	var after time.Duration
	if syncer, ok := hook.(hooks.Syncer); ok {
		var err error
		after, err = syncer.Sync(ctx, spec, config)
		note("Sync", err)
	}
	switch {
	case unfinished != nil:
		st.Message = hookFailedPrefix + unfinished.Error()
	case st.State == k4allv1alpha1.ComponentStateInstalled && hookUnfinished(st.Message):
		st.Message = engine.InstalledMessage
	}
	return after
}

// sooner is the shorter of two positive durations, 0 standing for none.
func sooner(a, b time.Duration) time.Duration {
	if a <= 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

// waitForStep says a component's upgrade path cannot go on yet: nothing is
// applied, and the reconcile comes back soon.
type waitForStep struct{ reason string }

func (w waitForStep) Error() string { return w.reason }

// upgradePathStart is the version a component's upgrade path goes on from:
// the recorded installedVersion, or what runs when the status records none
// (a component switched from Unmanaged to Managed) or a step is in progress.
// Only Helm steps wait for their resources: a manifest step is applied, and
// the component rolls it out on its own, so a waitForStep error says it does
// not run a ready version to go on from yet. Running past the recorded step
// is fine: the status write after a step can be lost. Any other error means
// nothing may be applied: what runs cannot be read, or it is newer than the
// release (no downgrades). A CR whose operator has not reported a version yet
// is waited on too: on a fresh install the first apply can fail after the CR
// is in, and its operator is then still starting (gpillon/k4all#81).
func (r *ReleaseReconciler) upgradePathStart(
	ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, st k4allv1alpha1.ComponentStatus,
) (string, error) {
	switchedToManaged := st.InstalledVersion == ""
	if len(spec.UpgradePath) == 0 || (!switchedToManaged && st.State != k4allv1alpha1.ComponentStateUpgrading) {
		return st.InstalledVersion, nil
	}
	reporter := r.versionReporter(name, spec)
	if reporter == nil {
		return st.InstalledVersion, nil
	}
	running, ready, err := reporter.RunningVersion(ctx)
	switch {
	case err != nil && switchedToManaged && goerrors.Is(err, engine.ErrNoVersionYet):
		return "", waitForStep{fmt.Sprintf("%v; waiting for it before going on towards %s", err, spec.Version)}
	case err != nil && switchedToManaged:
		return "", fmt.Errorf("cannot tell which version of %s runs: %w", name, err)
	case err != nil:
		return "", waitForStep{fmt.Sprintf("upgrade step %s applied, waiting for it to run: %v", st.InstalledVersion, err)}
	case switchedToManaged && running == "":
		return "", nil // nothing runs: a fresh install
	case newerVersion(running, spec.Version):
		return "", fmt.Errorf("%s runs %s, newer than the release's %s: not downgrading it", name, running, spec.Version)
	case switchedToManaged && !ready:
		return "", waitForStep{fmt.Sprintf("%s runs %s but is not ready: waiting before upgrading it towards %s",
			name, running, spec.Version)}
	case !switchedToManaged && (!ready || !atLeastVersion(running, st.InstalledVersion)):
		return "", waitForStep{fmt.Sprintf("upgrade step %s applied, waiting for it to run (runs %q, ready %t) before going on towards %s",
			st.InstalledVersion, running, ready, spec.Version)}
	}
	return running, nil
}

// versionReporter returns what reports the version a component runs: its
// hook for KubeVirt and CDI, its Helm release for Helm components, nil
// otherwise.
func (r *ReleaseReconciler) versionReporter(name string, spec k4allv1alpha1.ComponentSpec) engine.VersionReporter {
	if reporter, ok := r.hookReg.Get(name).(engine.VersionReporter); ok {
		return reporter
	}
	return r.engine.HelmReleaseReporter(name, spec)
}

// downgradeRefused says why the operator does not move a component from the
// version it has installed down to the release's, or "" when it may go: the
// release pins nothing lower, or AllowDowngradeAnnotation names this
// downgrade. Versions compare as semver; a pin that is not semver ("latest")
// is never refused.
func downgradeRefused(rm *k4allv1alpha1.ReleaseManifest, name string, spec k4allv1alpha1.ComponentSpec,
	st k4allv1alpha1.ComponentStatus) string {
	if st.InstalledVersion == "" || !newerVersion(st.InstalledVersion, spec.Version) ||
		downgradeAllowed(rm.GetAnnotations()[AllowDowngradeAnnotation], name, spec.Version) {
		return ""
	}
	return fmt.Sprintf("%sthe release pins %s, older than the installed %s "+
		"(to allow it, annotate the ReleaseManifest with %s: %s=%s)",
		downgradeRefusedPrefix, spec.Version, st.InstalledVersion, AllowDowngradeAnnotation, name, spec.Version)
}

// downgradeAllowed tells whether the AllowDowngradeAnnotation value has the
// entry name=version (semver equality, else the same text).
func downgradeAllowed(value, name, version string) bool {
	for _, entry := range strings.Split(value, ",") {
		n, v, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(n) != name {
			continue
		}
		v = strings.TrimSpace(v)
		if c, ok := engine.CompareVersions(v, version); (ok && c == 0) || v == version {
			return true
		}
	}
	return false
}

// downgradeRefusedCondition lists, on the ReleaseManifest, the components
// left at a version newer than the release pins (downgradeRefused).
const downgradeRefusedCondition = "DowngradeRefused"

// downgradeRefusedPrefix starts the message of a component the reconcile
// did not downgrade.
const downgradeRefusedPrefix = "not downgrading: "

// setDowngradeRefused sets downgradeRefusedCondition from what this
// reconcile did: a component that waits for its operator's uninstall, say,
// was not refused, whatever its installedVersion.
func setDowngradeRefused(rm *k4allv1alpha1.ReleaseManifest, active map[string]k4allv1alpha1.ComponentSpec) {
	var refused []string
	for name := range active {
		if strings.HasPrefix(rm.Status.Components[name].Message, downgradeRefusedPrefix) {
			refused = append(refused, name)
		}
	}
	if len(refused) == 0 {
		meta.RemoveStatusCondition(&rm.Status.Conditions, downgradeRefusedCondition)
		return
	}
	sort.Strings(refused)
	meta.SetStatusCondition(&rm.Status.Conditions, metav1.Condition{
		Type:               downgradeRefusedCondition,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: rm.Generation,
		Reason:             downgradeRefusedCondition,
		Message: "the release pins these components lower than they are installed, and " +
			AllowDowngradeAnnotation + " does not allow it: " + strings.Join(refused, ", "),
	})
}

// newerVersion tells whether a is a later semver than b.
func newerVersion(a, b string) bool {
	c, ok := engine.CompareVersions(a, b)
	return ok && c > 0
}

// atLeastVersion tells whether a is b or later: by semver, or the same string
// when either is not semver.
func atLeastVersion(a, b string) bool {
	if c, ok := engine.CompareVersions(a, b); ok {
		return c >= 0
	}
	return a == b
}

// reconcileRemovedComponents uninstalls components explicitly set to
// management=Removed and marks them Removed. It returns false if any removal failed.
func (r *ReleaseReconciler) reconcileRemovedComponents(
	ctx context.Context, log logr.Logger, rm *k4allv1alpha1.ReleaseManifest,
	removed map[string]k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
) bool {
	allOK := true
	for name, spec := range removed {
		prevStatus, hasPrev := rm.Status.Components[name]
		if hasPrev && (prevStatus.State == k4allv1alpha1.ComponentStateInstalled ||
			prevStatus.State == k4allv1alpha1.ComponentStateFailed ||
			prevStatus.State == k4allv1alpha1.ComponentStateUninstalling) {
			if !uninstallStarted(prevStatus) {
				if reason := r.uninstallRefusal(ctx, name); reason != "" {
					log.Info("removal refused by the component's hook", "name", name, "reason", reason)
					rm.Status.Components[name] = refusedUninstall(prevStatus, reason)
					allOK = false
					continue
				}
			}
			if st := r.prepareUninstall(ctx, log, name, prevStatus); st != nil {
				rm.Status.Components[name] = *st
				allOK = allOK && st.State != k4allv1alpha1.ComponentStateFailed
				continue
			}
			log.Info("removing component (management=Removed)", "name", name)
			rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
				State:              k4allv1alpha1.ComponentStateUninstalling,
				Message:            "uninstalling component",
				InstalledVersion:   prevStatus.InstalledVersion,
				LastTransitionTime: metav1.Now(),
			}
			removedLog := log.WithValues("management", k4allv1alpha1.ComponentManagementRemoved)
			uninstallErrs := r.uninstallWithCleanup(ctx, removedLog, name, spec, prevStatus.InstalledVersion, config)
			if len(uninstallErrs) > 0 {
				rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
					State:              k4allv1alpha1.ComponentStateFailed,
					Message:            fmt.Sprintf("removal failed: %s", strings.Join(uninstallErrs, "; ")),
					InstalledVersion:   prevStatus.InstalledVersion,
					LastTransitionTime: metav1.Now(),
				}
				allOK = false
				continue
			}
		}
		rm.Status.Components[name] = k4allv1alpha1.ComponentStatus{
			State:              k4allv1alpha1.ComponentStateRemoved,
			Message:            "removed by management policy",
			LastTransitionTime: metav1.Now(),
		}
	}
	return allOK
}

// uninstallRefusedPrefix starts the message of a component whose hook
// refuses its uninstall.
const uninstallRefusedPrefix = "uninstall refused: "

// uninstallRefusal is why the component's hook, when it is an
// UninstallGuard, refuses to let the component be uninstalled now; empty
// when it lets it, or has no say. A hook that cannot tell refuses.
func (r *ReleaseReconciler) uninstallRefusal(ctx context.Context, name string) string {
	guard, ok := r.hookReg.Get(name).(hooks.UninstallGuard)
	if !ok {
		return ""
	}
	reason, err := guard.UninstallBlocked(ctx, name)
	if err != nil {
		return fmt.Sprintf("cannot tell whether %s may be uninstalled: %v", name, err)
	}
	return reason
}

// refusedUninstall is the status of a component kept installed because its
// hook refuses the uninstall: still Installed at its version, so the next
// reconcile asks the hook again, with the reason in its message.
func refusedUninstall(prev k4allv1alpha1.ComponentStatus, reason string) k4allv1alpha1.ComponentStatus {
	st := keptInstalled(prev)
	st.Message = uninstallRefusedPrefix + reason
	return st
}

// keptInstalled is prev, Installed: a component kept at the version it runs
// though it is out of the release.
func keptInstalled(prev k4allv1alpha1.ComponentStatus) k4allv1alpha1.ComponentStatus {
	st := prev
	if st.State != k4allv1alpha1.ComponentStateInstalled {
		st.State = k4allv1alpha1.ComponentStateInstalled
		st.LastTransitionTime = metav1.Now()
	}
	return st
}

// How long an operator may take to uninstall what it created (KubeVirt, CDI)
// before its component is Failed, and where to read how to recover.
const (
	uninstallWait        = 15 * time.Minute
	uninstallRecovery    = "https://github.com/gpillon/k4all/issues/94"
	uninstallStuckPrefix = "uninstall stuck: "
	// uninstallFailedPrefix starts the message of a component whose
	// uninstall failed; the next reconcile tries it again.
	uninstallFailedPrefix = "uninstall failed"
)

// uninstallStarted tells whether the component's operator is uninstalling
// what it created (its hook deleted its CR): the uninstall goes on, and the
// UninstallGuard is not asked again, it can no longer stop it.
func uninstallStarted(st k4allv1alpha1.ComponentStatus) bool {
	return st.State == k4allv1alpha1.ComponentStateUninstalling ||
		(st.State == k4allv1alpha1.ComponentStateFailed && strings.HasPrefix(st.Message, uninstallStuckPrefix))
}

// uninstallUnfinished tells whether the component's uninstall started and
// is not over: under way, stuck, or failed (tried again).
func uninstallUnfinished(st k4allv1alpha1.ComponentStatus) bool {
	return uninstallStarted(st) ||
		(st.State == k4allv1alpha1.ComponentStateFailed && strings.HasPrefix(st.Message, uninstallFailedPrefix))
}

// prepareUninstall asks the component's hook, when it is an
// UninstallPreparer, whether its operator has uninstalled what it created.
// It returns the status to record while it has not: Uninstalling, or Failed
// once that took longer than uninstallWait (from the time the component
// went Uninstalling) or the hook failed; the operator is kept either way.
// Nil when the engine's uninstall may go on.
func (r *ReleaseReconciler) prepareUninstall(
	ctx context.Context, log logr.Logger, name string, prev k4allv1alpha1.ComponentStatus,
) *k4allv1alpha1.ComponentStatus {
	preparer, ok := r.hookReg.Get(name).(hooks.UninstallPreparer)
	if !ok {
		return nil
	}
	pending, err := preparer.PrepareUninstall(ctx, name)
	if err != nil {
		log.Error(err, "the operator's uninstall cannot go on", "name", name)
		return &k4allv1alpha1.ComponentStatus{
			State:              k4allv1alpha1.ComponentStateFailed,
			Message:            fmt.Sprintf("%s: %v", uninstallFailedPrefix, err),
			InstalledVersion:   prev.InstalledVersion,
			LastTransitionTime: metav1.Now(),
		}
	}
	if pending == "" {
		return nil
	}
	st := prev
	switch {
	case uninstallStarted(prev) && prev.State == k4allv1alpha1.ComponentStateFailed:
		// stuck already: stays Failed, since the same time
	case prev.State == k4allv1alpha1.ComponentStateUninstalling && time.Since(prev.LastTransitionTime.Time) > uninstallWait:
		st.State = k4allv1alpha1.ComponentStateFailed
		st.LastTransitionTime = metav1.Now()
	case prev.State != k4allv1alpha1.ComponentStateUninstalling:
		st.State = k4allv1alpha1.ComponentStateUninstalling
		st.LastTransitionTime = metav1.Now()
	}
	st.Message = pending
	if st.State == k4allv1alpha1.ComponentStateFailed {
		log.Info("the operator's uninstall is stuck", "name", name, "pending", pending)
		st.Message = fmt.Sprintf("%s%s, for more than %d minutes: k4all-operator keeps the operator "+
			"until it is done; to recover see %s", uninstallStuckPrefix, pending, int(uninstallWait.Minutes()), uninstallRecovery)
	}
	return &st
}

// uninstallWithCleanup uninstalls a component and runs its Cleanup hook (if any),
// returning the collected error messages. Both steps always run. A hook that
// is a Remover uninstalls the component in place of the engine.
func (r *ReleaseReconciler) uninstallWithCleanup(
	ctx context.Context, log logr.Logger, name string, spec k4allv1alpha1.ComponentSpec,
	installedVersion string, config k4allv1alpha1.ClusterConfigSpec,
) []string {
	var uninstallErrs []string
	hook := r.hookReg.Get(name)
	var err error
	if remover, ok := hook.(hooks.Remover); ok {
		err = remover.Remove(ctx, spec, config)
	} else {
		err = r.engine.UninstallComponent(ctx, name, spec, installedVersion)
	}
	if err != nil {
		log.Error(err, "uninstall failed", "name", name)
		uninstallErrs = append(uninstallErrs, fmt.Sprintf("uninstall: %v", err))
	}
	if hook != nil {
		if err := hook.Cleanup(ctx, name, config); err != nil {
			log.Error(err, "cleanup hook failed", "name", name)
			uninstallErrs = append(uninstallErrs, fmt.Sprintf("cleanup hook: %v", err))
		}
	}
	return uninstallErrs
}

// setReadySummary sets the Ready condition from the outcome of a full reconcile.
// Components still on their upgrade path (Upgrading, or Pending on a step),
// being uninstalled (Uninstalling: their operator removes what it created;
// or queued behind another) or kept installed by a refused uninstall keep it
// False.
func setReadySummary(rm *k4allv1alpha1.ReleaseManifest, allOK bool, activeCount int) {
	installed, failed := 0, false
	var waiting, uninstalling, refused []string
	for name, st := range rm.Status.Components {
		if st.State == k4allv1alpha1.ComponentStateInstalled {
			installed++
		}
		switch {
		case strings.HasPrefix(st.Message, uninstallRefusedPrefix):
			refused = append(refused, name)
		case st.State == k4allv1alpha1.ComponentStateFailed:
			failed = true
		case st.State == k4allv1alpha1.ComponentStateUninstalling, strings.HasPrefix(st.Message, uninstallQueuedPrefix):
			uninstalling = append(uninstalling, name)
		case st.State == k4allv1alpha1.ComponentStateUpgrading, st.State == k4allv1alpha1.ComponentStatePending:
			waiting = append(waiting, name)
		}
	}
	sort.Strings(refused)
	sort.Strings(uninstalling)
	sort.Strings(waiting)
	switch {
	case !allOK && (failed || len(refused) == 0):
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "ComponentsFailed",
			"one or more components failed to reconcile")
	case len(refused) > 0:
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "UninstallRefused",
			"components kept installed, their uninstall refused (see their status): "+strings.Join(refused, ", "))
	case len(uninstalling) > 0:
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "ComponentsUninstalling",
			"components being uninstalled (see their status): "+strings.Join(uninstalling, ", "))
	case len(waiting) > 0:
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "ComponentsUpgrading",
			"components on their upgrade path (see their status): "+strings.Join(waiting, ", "))
	default:
		engine.SetReadyCondition(rm, metav1.ConditionTrue, "AllComponentsReconciled",
			fmt.Sprintf("%d/%d components installed", installed, activeCount))
	}
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

	if config.Spec.Networking.CNI.Type == ciliumCNI {
		meta.SetStatusCondition(&config.Status.Conditions, r.podNetworkOverlap(ctx, config, rm))
	} else {
		meta.RemoveStatusCondition(&config.Status.Conditions, PodNetworkOverlapCondition)
	}

	r.patchClusterConfigStatus(ctx, config)
}

// migrationsAheadCondition is set, on the ClusterConfig and the
// ReleaseManifest, while the ClusterConfig counts more migrations than this
// operator knows (migrations.AheadError); components are not reconciled.
const migrationsAheadCondition = "MigrationsAhead"

func setMigrationsAhead(conditions *[]metav1.Condition, generation int64, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               migrationsAheadCondition,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		Reason:             migrationsAheadCondition,
		Message:            message,
	})
}

// patchClusterConfigStatus writes config's status as it is in memory.
func (r *ReleaseReconciler) patchClusterConfigStatus(ctx context.Context, config *k4allv1alpha1.ClusterConfig) {
	statusPayload := map[string]interface{}{
		"status": config.Status,
	}
	raw, err := json.Marshal(statusPayload)
	if err != nil {
		r.Log.Error(err, "failed to marshal ClusterConfig status patch")
		return
	}
	patch := client.RawPatch(types.MergePatchType, raw)
	if err := r.Status().Patch(ctx, config, patch); err != nil {
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

// patchStatus writes the ReleaseManifest status via a merge-patch, avoiding
// resourceVersion conflicts that happen when the spec/metadata was modified
// while the reconcile loop was running. The patch is computed against the
// stored status, so fields rm.Status no longer sets are cleared, not kept.
func (r *ReleaseReconciler) patchStatus(ctx context.Context, rm *k4allv1alpha1.ReleaseManifest) error {
	stored := &k4allv1alpha1.ReleaseManifest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rm), stored); err != nil {
		return fmt.Errorf("read status to patch: %w", err)
	}
	base := rm.DeepCopy()
	base.Status = stored.Status
	return r.Status().Patch(ctx, rm, client.MergeFrom(base))
}

// nodesUpgrading lists the Nodes carrying UpgradeInProgressAnnotation as
// "node (hop), ...", sorted by name; empty when no upgrade is running.
func (r *ReleaseReconciler) nodesUpgrading(ctx context.Context) (string, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	var upgrading []string
	for _, n := range nodes.Items {
		if hop, ok := n.Annotations[UpgradeInProgressAnnotation]; ok {
			upgrading = append(upgrading, fmt.Sprintf("%s (%s)", n.Name, hop))
		}
	}
	sort.Strings(upgrading)
	return strings.Join(upgrading, ", "), nil
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

// appliedHash identifies what a component is applied from: its spec, its
// componentOverrides and, when it has a hook, the rest of the ClusterConfig
// spec the hook reads. Hooks are not run here: some prepare the cluster.
func (r *ReleaseReconciler) appliedHash(
	name string, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
) string {
	inputs := struct {
		Spec     k4allv1alpha1.ComponentSpec      `json:"spec"`
		Override *k4allv1alpha1.ComponentOverride `json:"override,omitempty"`
		Config   *k4allv1alpha1.ClusterConfigSpec `json:"config,omitempty"`
		// The hook's values revision: a new one re-applies the component
		// after an operator upgrade. Empty (left out) for most hooks, so
		// their hashes stay the same.
		HookRevision string `json:"hookRevision,omitempty"`
	}{Spec: spec}
	if override, ok := config.ComponentOverrides[name]; ok {
		inputs.Override = &override
	}
	if hook := r.hookReg.Get(name); hook != nil {
		hookConfig := config
		hookConfig.ComponentOverrides = nil
		inputs.Config = &hookConfig
		if rev, ok := hook.(hooks.ValuesRevisioner); ok {
			inputs.HookRevision = rev.ValuesRevision()
		}
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// componentValues returns the values the engine layers on top of the
// ReleaseManifest values of one component: the PreInstall hook's values
// first, then the ClusterConfig componentOverrides, so the user always wins.
// The PreInstall hook, if any, may also prepare the cluster. A hook that is
// an OverrideMerger layers the override itself.
func (r *ReleaseReconciler) componentValues(
	ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
) (map[string]interface{}, error) {
	values := make(map[string]interface{})
	hook := r.hookReg.Get(name)
	if hook != nil {
		var err error
		if values, err = hook.PreInstall(ctx, name, spec, config, values); err != nil {
			return nil, err
		}
	}
	override, ok := config.ComponentOverrides[name]
	if merger, isMerger := hook.(hooks.OverrideMerger); isMerger {
		return merger.MergeOverride(spec, config, values, override)
	}
	if !ok {
		return values, nil
	}
	return engine.ApplyOverride(values, override)
}

// SetupWithManager watches the ReleaseManifests and the ClusterConfig's
// spec. Not its status: every reconcile writes it (lastReconcileTime), and a
// reconcile that ran into the next second would start the next one at once,
// trying a failed component again with no backoff.
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
		), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("release").
		Complete(r)
}
