package controller

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/go-logr/logr"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/engine"
)

// removalPlan says how the components that go away from the release (their
// feature off, or a CNI switch) are uninstalled (gpillon/k4all#96):
//   - the components that name the same featureGate are one group, and go as
//     one: while any of them refuses its uninstall (UninstallGuard), all of
//     them stay. A group is a gate name, not a feature: a component that
//     several features need (snapshot-controller, for virt and Rook) has a
//     gate of its own, on while any of them is, so it goes only with that
//     gate, never with one of the features. A component without a
//     featureGate (a CNI switch) is a group of its own;
//   - they go in reverse dependency order, dependents first, ties by name:
//     the consoles, then CDI, then KubeVirt; Cairn, then the CSI drivers,
//     then the Rook operator. The components of a group go one after the
//     other, each once the one before it is gone; any component also waits
//     for what depends on it, in any group (snapshot-controller for Hangar).
//
// A component the release sets management Removed is not part of it: it is
// removed on its own (reconcileRemovedComponents).
type removalPlan struct {
	// order is the removal order.
	order []string
	// group is each component's featureGate, "" for none.
	group map[string]string
	// before lists, in order, what must be gone before each component may
	// start its uninstall.
	before map[string][]string
}

// planRemoval is the removalPlan of components, the managed components out
// of the release.
func planRemoval(components map[string]k4allv1alpha1.ComponentSpec) removalPlan {
	// The dependency graph turned around: a component "depends on" what
	// depends on it, so its topological order has dependents first.
	reversed := make(map[string]k4allv1alpha1.ComponentSpec, len(components))
	for name := range components {
		reversed[name] = k4allv1alpha1.ComponentSpec{}
	}
	for name, spec := range components {
		for _, dep := range spec.DependsOn {
			if r, ok := reversed[dep]; ok && dep != name {
				r.DependsOn = append(r.DependsOn, name)
				reversed[dep] = r
			}
		}
	}
	order, err := engine.NewDependencyGraph(reversed).TopologicalSort()
	if err != nil {
		// A dependency cycle among them: no order to follow.
		order = make([]string, 0, len(components))
		for name := range components {
			order = append(order, name)
		}
		sort.Strings(order)
	}
	plan := removalPlan{order: order, group: map[string]string{}, before: map[string][]string{}}
	for _, name := range order {
		plan.group[name] = components[name].FeatureGate
	}
	dependsOn := func(name, dep string) bool { return name != dep && slices.Contains(components[name].DependsOn, dep) }
	for i, name := range order {
		for _, earlier := range order[:i] {
			sameGroup := plan.group[name] != "" && plan.group[earlier] == plan.group[name]
			if sameGroup || dependsOn(earlier, name) {
				plan.before[name] = append(plan.before[name], earlier)
			}
		}
	}
	return plan
}

// waitsFor is the first component that must be gone before name may start
// its uninstall and is not; "" when none.
func (p removalPlan) waitsFor(name string, gone map[string]bool) string {
	for _, earlier := range p.before[name] {
		if !gone[earlier] {
			return earlier
		}
	}
	return ""
}

// uninstallRefusals asks the UninstallGuard of every component of the plan
// that is installed and has not started its uninstall, and returns the
// refusal to record on each component kept: for a group, every such member
// gets the same one, "<member>: <reason>" for each member that refuses,
// joined by "; ", the components the others run on first; a component
// without a group gets its own reason, as it is.
func (r *ReleaseReconciler) uninstallRefusals(
	ctx context.Context, log logr.Logger, rm *k4allv1alpha1.ReleaseManifest, plan removalPlan,
	installed map[string]bool,
) map[string]string {
	mayRefuse := func(name string) bool {
		return installed[name] && !uninstallStarted(rm.Status.Components[name])
	}
	refused := map[string]string{}
	byGroup := map[string][]string{}
	for i := len(plan.order) - 1; i >= 0; i-- {
		name := plan.order[i]
		if !mayRefuse(name) {
			continue
		}
		reason := r.uninstallRefusal(ctx, name)
		if reason == "" {
			continue
		}
		if group := plan.group[name]; group != "" {
			byGroup[group] = append(byGroup[group], name+": "+reason)
			continue
		}
		log.Info("uninstall refused by the component's hook", "name", name, "reason", reason)
		refused[name] = reason
	}
	for group, reasons := range byGroup {
		reason := strings.Join(reasons, "; ")
		var held []string
		for _, name := range plan.order {
			if plan.group[name] == group && mayRefuse(name) {
				refused[name] = reason
				held = append(held, name)
			}
		}
		log.Info("uninstall of a feature gate's components refused by their hooks: all of them stay",
			"featureGate", group, "components", held, "reason", reason)
	}
	return refused
}

// uninstallQueuedPrefix starts the message of a component, out of the
// release, that waits for another to be uninstalled before its own uninstall
// starts (removalPlan). It is still Installed, untouched.
const uninstallQueuedPrefix = "uninstall queued: "

// queuedUninstall is the status of a component whose uninstall waits for
// first's: Installed at its version, with first named in its message.
func queuedUninstall(prev k4allv1alpha1.ComponentStatus, first string) k4allv1alpha1.ComponentStatus {
	st := keptInstalled(prev)
	st.Message = uninstallQueuedPrefix + "waiting for " + first + " to be uninstalled first"
	return st
}
