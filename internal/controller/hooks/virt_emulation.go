package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// useEmulationPath is spec.configuration.developerConfiguration.useEmulation
// in the KubeVirt CR. True: VMs run in software emulation, because
// virt-launcher then requests no devices.kubevirt.io/kvm and gets no /dev/kvm,
// even on nodes that have it. Unset is false.
var useEmulationPath = []string{"spec", "configuration", "developerConfiguration", "useEmulation"}

// emulationMode is features.virt.emulation.
type emulationMode int

const (
	// emulationAuto: "auto", or unset. The nodes decide.
	emulationAuto emulationMode = iota
	// emulationOn: "true".
	emulationOn
	// emulationOff: "false".
	emulationOff
)

func parseEmulationMode(s string) emulationMode {
	switch s {
	case "true":
		return emulationOn
	case "false":
		return emulationOff
	}
	return emulationAuto
}

const (
	// emulationRecheckInterval is how soon Sync runs again while a change
	// waits for the nodes to settle, or while nothing reports yet.
	emulationRecheckInterval = 30 * time.Second
	// unreportedRecheckWindow is how long Sync keeps coming back every
	// emulationRecheckInterval while nothing reports (virt-handler not up,
	// no KubeVirt CR): after it, the periodic reconcile is enough.
	unreportedRecheckWindow = 10 * time.Minute
	// kvmSettle is how long a node must keep reporting KVM before auto turns
	// emulation off: on a node without /dev/kvm, virt-handler's kvm plugin
	// reports its devices healthy for an instant when it registers.
	kvmSettle = 30 * time.Second
	// noKVMSettle is how long the nodes must keep reporting no KVM before
	// auto turns emulation on: a node with /dev/kvm whose virt-handler
	// crashed has nothing allocatable until virt-handler is back.
	noKVMSettle = 10 * time.Minute
)

// Sync sets useEmulation from features.virt.emulation. The reconciler runs it
// after every apply of KubeVirt and on every reconcile while KubeVirt is up to
// date, so the ClusterConfig owns the field: a value changed by hand in the
// KubeVirt CR is put back.
//
// "true" and "false" are set as they are. "auto" follows the nodes once what
// they report has held without a break, as seen by consecutive Syncs:
//   - a node offers KVM, for kvmSettle: emulation off. useEmulation is
//     cluster-wide, and with emulation on VMs get no /dev/kvm even where it
//     exists, so a mixed cluster runs VMs on its KVM nodes rather than
//     everywhere emulated;
//   - virt-handler is up and no node offers KVM, for noKVMSettle: emulation on;
//   - no node reports (virt-handler not up yet): the CR is left as it is.
//
// This also corrects the useEmulation true that operators up to v0.5.3 set on
// every cluster, whatever its nodes (gpillon/k4all#57).
func (h *VirtHook) Sync(ctx context.Context, _ k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) (time.Duration, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()

	// An error shows in the component's status; the periodic reconcile tries again.
	kv, found, err := getOperatorCR(ctx, h.client, kubeVirtAPIVersion, kubeVirtKind, kubeVirtKey)
	if err != nil {
		return 0, err
	}
	if !found {
		return untilReported(h.streak.observe(kvmUnknown, now)), nil
	}
	current, _, _ := unstructured.NestedBool(kv.Object, useEmulationPath...)

	mode := parseEmulationMode(config.Features.Virt.Emulation)
	if mode != emulationAuto {
		h.streak = kvmStreak{}
		want := mode == emulationOn
		if current == want {
			return 0, nil
		}
		h.log.Info("configuring KubeVirt emulation", "emulation", config.Features.Virt.Emulation, "useEmulation", want)
		return 0, h.patchEmulation(ctx, kv, want)
	}

	nodes := &corev1.NodeList{}
	if err := h.client.List(ctx, nodes); err != nil {
		return 0, fmt.Errorf("list nodes: %w", err)
	}
	kvm := nodesKVM(nodes.Items)
	held := h.streak.observe(kvm, now)
	var want bool
	var settle time.Duration
	switch kvm {
	case kvmAvailable:
		want, settle = false, kvmSettle
	case kvmMissing:
		want, settle = true, noKVMSettle
	default:
		return untilReported(held), nil
	}
	if current == want {
		return 0, nil
	}
	if held < settle {
		h.log.V(1).Info("KubeVirt emulation auto: waiting for the nodes to settle",
			"nodes", kvm.String(), "for", held, "settle", settle, "useEmulation", want)
		return min(emulationRecheckInterval, settle-held), nil
	}
	h.log.Info("configuring KubeVirt emulation", "emulation", "auto", "nodes", kvm.String(), "for", held,
		"useEmulation", want)
	return 0, h.patchEmulation(ctx, kv, want)
}

// untilReported is how soon Sync wants to run again when nothing has
// reported for that long.
func untilReported(unreported time.Duration) time.Duration {
	if unreported < unreportedRecheckWindow {
		return emulationRecheckInterval
	}
	return 0
}

// kvmOnNodes is what the nodes report about KVM.
type kvmOnNodes int

const (
	// kvmUnknown: no node runs virt-handler with its device plugins up.
	kvmUnknown kvmOnNodes = iota
	// kvmAvailable: at least one node offers devices.kubevirt.io/kvm.
	kvmAvailable
	// kvmMissing: virt-handler is up, and no node offers KVM.
	kvmMissing
)

func (k kvmOnNodes) String() string {
	switch k {
	case kvmAvailable:
		return "kvm"
	case kvmMissing:
		return "no-kvm"
	}
	return "unknown"
}

// kvmStreak is what consecutive Syncs read from the nodes, and since when.
type kvmStreak struct {
	kvm   kvmOnNodes
	since time.Time
}

// observe records what the nodes report now, and returns for how long they
// have reported it without a break.
func (s *kvmStreak) observe(kvm kvmOnNodes, now time.Time) time.Duration {
	if s.since.IsZero() || s.kvm != kvm {
		*s = kvmStreak{kvm: kvm, since: now}
	}
	return now.Sub(s.since)
}

// kvmResource is the resource virt-handler's kvm device plugin registers on
// every node it runs on, whatever useEmulation says. The kubelet counts the
// devices as allocatable only while the plugin reports them healthy, that is
// while /dev/kvm exists: without it the node has capacity and nothing
// allocatable.
const kvmResource = corev1.ResourceName("devices.kubevirt.io/kvm")

// schedulableLabel is "true" while virt-handler runs on the node with its
// device plugins started. virt-handler sets it "false" when it stops, and
// virt-controller when its heartbeat stops.
const schedulableLabel = "kubevirt.io/schedulable"

// nodesKVM reads KVM from the nodes where virt-handler is up.
func nodesKVM(nodes []corev1.Node) kvmOnNodes {
	result := kvmUnknown
	for _, n := range nodes {
		if n.Labels[schedulableLabel] != "true" {
			continue
		}
		if q, ok := n.Status.Allocatable[kvmResource]; ok && q.Sign() > 0 {
			return kvmAvailable
		}
		if q, ok := n.Status.Capacity[kvmResource]; ok && q.Sign() > 0 {
			result = kvmMissing
		}
	}
	return result
}

func (h *VirtHook) patchEmulation(ctx context.Context, kv *unstructured.Unstructured, useEmulation bool) error {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"configuration": map[string]interface{}{
				"developerConfiguration": map[string]interface{}{
					"useEmulation": useEmulation,
				},
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return h.client.Patch(ctx, kv, client.RawPatch(types.MergePatchType, patchBytes))
}
