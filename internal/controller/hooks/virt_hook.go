package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VirtHook handles post-install logic for KubeVirt:
//   - Enables/disables software emulation based on ClusterConfig
//
// It is a Syncer: emulation "auto" follows what the nodes report, which is
// only known once virt-handler runs on them, after KubeVirt is applied.
type VirtHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewVirtHook(c client.Client, log logr.Logger) *VirtHook {
	return &VirtHook{client: c, log: log}
}

var kubeVirtKey = types.NamespacedName{Name: "kubevirt", Namespace: "kubevirt"}

// useEmulationPath is spec.configuration.developerConfiguration.useEmulation
// in the KubeVirt CR. True: VMs run in software emulation (no
// devices.kubevirt.io/kvm request, so no /dev/kvm in virt-launcher), even on
// nodes with KVM. Unset is false.
var useEmulationPath = []string{"spec", "configuration", "developerConfiguration", "useEmulation"}

// The explicit values of features.virt.emulation. Anything else ("auto", or
// unset) is auto.
const (
	emulationOn  = "true"
	emulationOff = "false"
)

// emulationRecheck is how soon Sync wants to run again while it cannot tell
// whether the nodes have KVM, or cannot read the KubeVirt CR yet.
const emulationRecheck = 30 * time.Second

// PostInstall sets useEmulation once KubeVirt is applied (installed, upgraded,
// or its ClusterConfig changed). With "auto" and no node reporting yet (a
// fresh install: virt-handler is not up), the CR is left as it is, and Sync
// sets it once the nodes report.
func (h *VirtHook) PostInstall(ctx context.Context, _ k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) error {
	kv, err := h.waitForKubeVirt(ctx)
	if err != nil {
		return err
	}
	_, err = h.setEmulation(ctx, kv, config.Features.Virt.Emulation, false)
	return err
}

// Sync keeps useEmulation in line between installs: it finishes the decision
// PostInstall could not make on a fresh install, corrects the useEmulation
// true that operators before v0.5.4 set on every cluster
// (gpillon/k4all#57), and puts back a value changed by hand.
func (h *VirtHook) Sync(ctx context.Context, _ k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) (time.Duration, error) {
	kv := newKubeVirtCR()
	if err := h.client.Get(ctx, kubeVirtKey, kv); err != nil {
		if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return emulationRecheck, nil
		}
		return emulationRecheck, fmt.Errorf("get KubeVirt CR: %w", err)
	}
	return h.setEmulation(ctx, kv, config.Features.Virt.Emulation, true)
}

// setEmulation patches useEmulation when it differs from what mode asks for.
// It returns emulationRecheck while "auto" cannot tell from the nodes yet.
func (h *VirtHook) setEmulation(ctx context.Context, kv *unstructured.Unstructured, mode string, recheck bool) (time.Duration, error) {
	nodes := kvmUnknown
	if mode != emulationOn && mode != emulationOff {
		list := &corev1.NodeList{}
		if err := h.client.List(ctx, list); err != nil {
			return emulationRecheck, fmt.Errorf("list nodes: %w", err)
		}
		nodes = nodesKVM(list.Items)
	}
	var current *bool
	if v, found, _ := unstructured.NestedBool(kv.Object, useEmulationPath...); found {
		current = &v
	}

	want, set := emulationFor(mode, nodes, current, recheck)
	switch {
	case !set && nodes == kvmUnknown:
		h.log.V(1).Info("KubeVirt emulation auto: no node reports KVM yet, checking again later")
		return emulationRecheck, nil
	case !set, current != nil && *current == want:
		return 0, nil
	}
	h.log.Info("configuring KubeVirt emulation", "emulation", mode, "nodes", nodes.String(), "useEmulation", want)
	return 0, h.patchEmulation(ctx, kv, want)
}

// kvmOnNodes is what the nodes report about KVM.
type kvmOnNodes int

const (
	// kvmUnknown: no node runs virt-handler with its device plugins up yet.
	kvmUnknown kvmOnNodes = iota
	// kvmAvailable: at least one node offers devices.kubevirt.io/kvm.
	kvmAvailable
	// kvmMissing: virt-handler runs, and no node offers KVM.
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

// kvmResource is the resource virt-handler's device plugin registers on every
// node it runs on. The kubelet counts the devices as allocatable only while
// the plugin reports them healthy, that is while /dev/kvm exists on the node:
// with no /dev/kvm the node has capacity and nothing allocatable. The plugin
// runs whatever useEmulation says.
const kvmResource = corev1.ResourceName("devices.kubevirt.io/kvm")

// schedulableLabel is "true" while virt-handler runs on the node with its
// device plugins started; virt-handler sets it "false" when it stops, and
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

// emulationFor is the useEmulation that mode asks for, and false when the CR
// is to be left as it is. current is the CR's value, nil when unset; recheck
// tells a Sync from the PostInstall that follows applying KubeVirt.
//
// "true" and "false" are what they say. "auto" uses KVM as soon as a node
// offers it: useEmulation is cluster-wide, and with emulation on, VMs get no
// /dev/kvm even where it exists, so a mixed cluster runs VMs on its KVM nodes
// rather than everywhere emulated. With no node offering KVM, auto emulates,
// except that Sync never turns an explicit false into true: a KVM node whose
// virt-handler crashed reports no allocatable KVM until it is back. Applying
// KubeVirt again (a ClusterConfig change, a new version) decides afresh.
func emulationFor(mode string, nodes kvmOnNodes, current *bool, recheck bool) (useEmulation, set bool) {
	switch mode {
	case emulationOn:
		return true, true
	case emulationOff:
		return false, true
	}
	switch nodes {
	case kvmAvailable:
		return false, true
	case kvmMissing:
		if recheck && current != nil && !*current {
			return false, false
		}
		return true, true
	}
	return false, false
}

func newKubeVirtCR() *unstructured.Unstructured {
	kv := &unstructured.Unstructured{}
	kv.SetAPIVersion("kubevirt.io/v1")
	kv.SetKind("KubeVirt")
	return kv
}

// waitForKubeVirt waits up to two minutes for the KubeVirt CR that
// kubevirt-cr.yaml creates.
func (h *VirtHook) waitForKubeVirt(ctx context.Context) (*unstructured.Unstructured, error) {
	kv := newKubeVirtCR()

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		err := h.client.Get(waitCtx, kubeVirtKey, kv)
		if err == nil {
			return kv, nil
		}
		if !errors.IsNotFound(err) {
			return nil, fmt.Errorf("get KubeVirt CR: %w", err)
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("timed out waiting for KubeVirt CR")
		case <-ticker.C:
		}
	}
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

// RunningVersion reads the KubeVirt CR: virt-operator reports there the
// version it runs and the one it is rolling out.
func (h *VirtHook) RunningVersion(ctx context.Context) (string, bool, error) {
	return operatorCRVersion(ctx, h.client, "kubevirt.io/v1", "KubeVirt",
		kubeVirtKey, "observedKubeVirtVersion", "targetKubeVirtVersion")
}
