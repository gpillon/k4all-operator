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

// resyncInterval is how soon a reconcile comes back when nothing asks for sooner.
const resyncInterval = 5 * time.Minute

// hookFailedPrefix starts the message of an installed component whose
// PostInstall or Sync failed.
const hookFailedPrefix = "installed but hook failed: "

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
	// Removals first: a replacement can then take over what the removed
	// component held (traefik: the node's ports and the "nginx" IngressClass).
	removedOK := r.reconcileRemovedComponents(ctx, log, rm, removed, config.Spec)
	inactiveOK := r.reconcileInactiveComponents(ctx, log, rm, active, config.Spec)
	activeOK, stepsLeft, hookRequeue := r.reconcileActiveComponents(ctx, log, rm, order, active, config.Spec)
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
// uninstalled first, then marked Skipped. It returns false if any uninstall failed.
func (r *ReleaseReconciler) reconcileInactiveComponents(
	ctx context.Context, log logr.Logger, rm *k4allv1alpha1.ReleaseManifest,
	active map[string]k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
) bool {
	allOK := true
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
		if r.engine.IsInstalled(ctx, name, spec, &currentStatus) &&
			(currentStatus.AppliedHash == hash || currentStatus.AppliedHash == "") {
			// No hash: installed by an operator that did not record hashes.
			// Adopt it instead of reinstalling every component on operator upgrade.
			currentStatus.AppliedHash = hash
			hookRequeue = sooner(hookRequeue, r.syncHook(ctx, log, name, spec, config, &currentStatus))
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

		// Run PostInstall hook for post-install configuration
		if hook := r.hookReg.Get(name); hook != nil {
			if hookErr := hook.PostInstall(ctx, spec, config); hookErr != nil {
				log.Error(hookErr, "post-install hook failed", "name", name)
				st.Message = hookFailedPrefix + hookErr.Error()
			}
		}
		hookRequeue = sooner(hookRequeue, r.syncHook(ctx, log, name, spec, config, st))
		rm.Status.Components[name] = *st
	}
	return allOK, stepsLeft, hookRequeue
}

// syncHook runs the Sync of the component's hook, when the hook is a Syncer,
// and returns how soon it wants to run again (0: at the periodic reconcile).
// A failure goes into st's message like a PostInstall one; a success clears
// such a message from an installed component.
func (r *ReleaseReconciler) syncHook(
	ctx context.Context, log logr.Logger, name string, spec k4allv1alpha1.ComponentSpec,
	config k4allv1alpha1.ClusterConfigSpec, st *k4allv1alpha1.ComponentStatus,
) time.Duration {
	syncer, ok := r.hookReg.Get(name).(hooks.Syncer)
	if !ok {
		return 0
	}
	after, err := syncer.Sync(ctx, spec, config)
	switch {
	case err != nil:
		log.Error(err, "hook sync failed", "name", name)
		st.Message = hookFailedPrefix + err.Error()
	case st.State == k4allv1alpha1.ComponentStateInstalled && strings.HasPrefix(st.Message, hookFailedPrefix):
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
	return fmt.Sprintf("the release pins %s, older than the installed %s: not downgrading "+
		"(to allow it, annotate the ReleaseManifest with %s: %s=%s)",
		spec.Version, st.InstalledVersion, AllowDowngradeAnnotation, name, spec.Version)
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

func setDowngradeRefused(rm *k4allv1alpha1.ReleaseManifest, active map[string]k4allv1alpha1.ComponentSpec) {
	var refused []string
	for name, spec := range active {
		if downgradeRefused(rm, name, spec, rm.Status.Components[name]) != "" {
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
			prevStatus.State == k4allv1alpha1.ComponentStateFailed) {
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

// uninstallWithCleanup uninstalls a component and runs its Cleanup hook (if any),
// returning the collected error messages. Both steps always run.
func (r *ReleaseReconciler) uninstallWithCleanup(
	ctx context.Context, log logr.Logger, name string, spec k4allv1alpha1.ComponentSpec,
	installedVersion string, config k4allv1alpha1.ClusterConfigSpec,
) []string {
	var uninstallErrs []string
	if err := r.engine.UninstallComponent(ctx, name, spec, installedVersion); err != nil {
		log.Error(err, "uninstall failed", "name", name)
		uninstallErrs = append(uninstallErrs, fmt.Sprintf("uninstall: %v", err))
	}
	if hook := r.hookReg.Get(name); hook != nil {
		if err := hook.Cleanup(ctx, name, config); err != nil {
			log.Error(err, "cleanup hook failed", "name", name)
			uninstallErrs = append(uninstallErrs, fmt.Sprintf("cleanup hook: %v", err))
		}
	}
	return uninstallErrs
}

// setReadySummary sets the Ready condition from the outcome of a full reconcile.
// Components still on their upgrade path (Upgrading, or Pending on a step)
// keep it False.
func setReadySummary(rm *k4allv1alpha1.ReleaseManifest, allOK bool, activeCount int) {
	if !allOK {
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "ComponentsFailed",
			"one or more components failed to reconcile")
		return
	}
	installed := 0
	var waiting []string
	for name, st := range rm.Status.Components {
		switch st.State {
		case k4allv1alpha1.ComponentStateInstalled:
			installed++
		case k4allv1alpha1.ComponentStateUpgrading, k4allv1alpha1.ComponentStatePending:
			waiting = append(waiting, name)
		}
	}
	if len(waiting) > 0 {
		sort.Strings(waiting)
		engine.SetReadyCondition(rm, metav1.ConditionFalse, "ComponentsUpgrading",
			"components on their upgrade path (see their status): "+strings.Join(waiting, ", "))
		return
	}
	engine.SetReadyCondition(rm, metav1.ConditionTrue, "AllComponentsReconciled",
		fmt.Sprintf("%d/%d components installed", installed, activeCount))
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
// The PreInstall hook, if any, may also prepare the cluster.
func (r *ReleaseReconciler) componentValues(
	ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
) (map[string]interface{}, error) {
	values := make(map[string]interface{})
	if hook := r.hookReg.Get(name); hook != nil {
		var err error
		if values, err = hook.PreInstall(ctx, name, spec, config, values); err != nil {
			return nil, err
		}
	}
	override, ok := config.ComponentOverrides[name]
	if !ok {
		return values, nil
	}
	return engine.ApplyOverride(values, override)
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
