package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/clusterupgrade"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/drain"
	"github.com/gpillon/k4all-operator/internal/nodesettings"
)

// kube-system/kubelet-config, kubeadm's KubeletConfiguration for every node:
// the joining ones get it, `kubeadm upgrade` keeps it.
const (
	kubeletConfigNamespace = "kube-system"
	kubeletConfigName      = "kubelet-config"
	kubeletConfigKey       = "kubelet"
)

// calicoCNI is spec.networking.cni.type with Calico.
const calicoCNI = "calico"

// k4allFieldManager is the field manager of everything k4all owns (spec §4 R3).
const k4allFieldManager = "k4all-operator"

// NodeSettingsReconciler rolls spec.nodes of the ClusterConfig out (k4all#60):
// it writes the kubelet fields k4all owns into kube-system/kubelet-config and
// takes the nodes one at a time (nodesettings.Decide), sharing NodeLock with
// the ClusterUpgrade: neither starts a node while the other has one. Each
// node's k4all-upgrade agent applies the settings (ADR-0006).
type NodeSettingsReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads the Nodes, kubelet-config and the pods of a node past
	// the cache.
	APIReader client.Reader
	// Self is the operator's own pod (k4all#97): its node goes last, and its
	// drain ends with the operator evicting itself.
	Self types.NamespacedName
	// Lock is shared with the ClusterUpgrade.
	Lock *NodeLock
	// Now is time.Now unless a test sets it.
	Now func() time.Time

	// selfEvictTried is the node whose drain evicted the operator's pod once.
	selfEvictTried string
}

// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k4all.magesgate.com,resources=clusterupgrades,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch

// Reconcile moves the node-settings rollout one step on.
func (r *NodeSettingsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	config, err := r.clusterConfig(ctx)
	if err != nil || config == nil || config.Name != req.Name {
		return ctrl.Result{}, err
	}

	unlock := r.Lock.lock()
	defer unlock()
	if r.Lock.left() {
		return ctrl.Result{RequeueAfter: leavingRequeue}, nil
	}
	in, cm, err := r.input(ctx, config)
	if err != nil {
		return ctrl.Result{}, err
	}
	result := nodesettings.Decide(in)
	d := r.drainer()
	// Actions first: the status must never claim a step the Nodes do not show.
	for _, a := range result.Actions {
		log.Info("node settings step", "action", a.Kind, "node", a.Node, "hash", a.Hash)
		switch a.Kind {
		case nodesettings.Evict:
			refused, err := d.evict(ctx, a.Pods)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("%s %s: %w", a.Kind, a.Node, err)
			}
			noteRefused(result.Status, a.Node, refused)
		case nodesettings.EvictSelf:
			r.selfEvictTried = a.Node
			if evictSelf(ctx, d, a.Pods) {
				r.Lock.leave()
			}
		case nodesettings.WriteConfig:
			if err := r.writeConfig(ctx, cm, a); err != nil {
				if apierrors.IsConflict(err) { // kubeadm or someone wrote it meanwhile
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}
				return ctrl.Result{}, fmt.Errorf("write %s/%s: %w", kubeletConfigNamespace, kubeletConfigName, err)
			}
		default:
			if err := r.patchNode(ctx, a.Node, nodePatch(a)); err != nil {
				return ctrl.Result{}, fmt.Errorf("%s %s: %w", a.Kind, a.Node, err)
			}
		}
	}
	if err := r.writeStatus(ctx, config.Name, result); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("patch ClusterConfig status: %w", err)
	}
	return ctrl.Result{RequeueAfter: settingsRequeue(result.Status)}, nil
}

// clusterConfig is the cluster's ClusterConfig, the first by name as the
// ReleaseReconciler takes it; nil when there is none.
func (r *NodeSettingsReconciler) clusterConfig(ctx context.Context) (*k4allv1alpha1.ClusterConfig, error) {
	list := &k4allv1alpha1.ClusterConfigList{}
	if err := r.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list ClusterConfigs: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return &list.Items[0], nil
}

// input reads what Decide needs, and kubelet-config (nil when missing) for
// the write.
func (r *NodeSettingsReconciler) input(ctx context.Context, config *k4allv1alpha1.ClusterConfig) (nodesettings.Input, *corev1.ConfigMap, error) {
	log := logf.FromContext(ctx)
	in := nodesettings.Input{Spec: config.Spec.Nodes, Now: r.now(), Generation: config.Generation, Pods: map[string][]drain.Pod{}}
	nodeList := &corev1.NodeList{}
	if err := r.reader().List(ctx, nodeList); err != nil {
		return in, nil, fmt.Errorf("list nodes: %w", err)
	}
	d := r.drainer()
	inStep := ""
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		in.Nodes = append(in.Nodes, nodesettings.Node{
			Name: n.Name, Annotations: n.Annotations, Ready: nodeReady(n), Unschedulable: n.Spec.Unschedulable,
			CPUs: n.Status.Capacity.Cpu().Value(), Memory: n.Status.Capacity.Memory().Value(), PodCIDR: n.Spec.PodCIDR,
		})
		if _, ok := n.Annotations[nodesettings.StepAnnotation]; ok {
			inStep = n.Name
			pods, err := d.pods(ctx, n.Name)
			if err != nil {
				return in, nil, err
			}
			in.Pods[n.Name] = pods
		}
	}

	cm := &corev1.ConfigMap{}
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: kubeletConfigNamespace, Name: kubeletConfigName}, cm)
	switch {
	case apierrors.IsNotFound(err):
		in.KubeletConfigMissing, cm = true, nil
	case err != nil:
		return in, nil, fmt.Errorf("read %s/%s: %w", kubeletConfigNamespace, kubeletConfigName, err)
	default:
		in.KubeletConfig = cm.Data[kubeletConfigKey]
		if text, ok := cm.Annotations[nodesettings.ConfigMapAnnotation]; ok {
			if in.Record, err = nodesettings.ParseRecord(text); err != nil {
				// What k4all wrote is lost: nothing it wrote can be removed,
				// every node is taken as having anything (drain-reboot).
				log.Error(err, "kubelet-config annotation unreadable: it is written again")
				in.Record = nil
			}
		}
	}

	cu := &k4allv1alpha1.ClusterUpgrade{}
	if err := r.Get(ctx, types.NamespacedName{Name: k4allv1alpha1.ClusterUpgradeName}, cu); apierrors.IsNotFound(err) {
		cu = nil
	} else if err != nil {
		return in, nil, fmt.Errorf("read ClusterUpgrade %s: %w", k4allv1alpha1.ClusterUpgradeName, err)
	}
	in.UpgradeBusy = upgradeBusy(cu, nodeList.Items)
	in.Drain = clusterupgrade.DrainSettings(k4allv1alpha1.ClusterUpgradeSpec{})
	if cu != nil {
		in.Drain = clusterupgrade.DrainSettings(cu.Spec)
	}

	if config.Spec.Nodes != nil && config.Spec.Nodes.Kubelet != nil && config.Spec.Nodes.Kubelet.MaxPods != nil {
		if in.PodNetwork, err = r.podNetwork(ctx, config); err != nil {
			return in, nil, err
		}
	}
	if in.UnhealthyPDBs, err = d.unhealthyPDBs(ctx); err != nil {
		return in, nil, err
	}
	if len(nodeList.Items) > 1 && (config.Spec.Nodes != nil || in.Record != nil) {
		if in.Blockers, err = d.drainBlockers(ctx); err != nil {
			return in, nil, err
		}
	}
	in.Self = d.self(ctx)
	if r.selfEvictTried != inStep {
		r.selfEvictTried = ""
	}
	in.SelfEvictTried = inStep != "" && r.selfEvictTried == inStep
	return in, cm, nil
}

// upgradeBusy says what of an upgrade keeps a node-settings step from
// starting: a ClusterUpgrade in progress or paused, a node in its hop (also
// one run by hand: the upgrade-in-progress pause gate). "" when nothing.
func upgradeBusy(cu *k4allv1alpha1.ClusterUpgrade, nodes []corev1.Node) string {
	for _, n := range nodes {
		if hop := n.Annotations[UpgradeInProgressAnnotation]; hop != "" {
			return fmt.Sprintf("node %s runs the hop %s", n.Name, hop)
		}
		if n.Annotations[k4allv1alpha1.NodeUpgradeCordonedAnnotation] != "" {
			return fmt.Sprintf("node %s is in its upgrade hop", n.Name)
		}
	}
	if cu != nil && (cu.Status.Phase == k4allv1alpha1.ClusterUpgradeProgressing || cu.Status.Phase == k4allv1alpha1.ClusterUpgradePaused) {
		return fmt.Sprintf("the ClusterUpgrade to %s is %s", cu.Spec.TargetRelease, strings.ToLower(string(cu.Status.Phase)))
	}
	return ""
}

// podNetwork is how the nodes get pod addresses, for the maxPods check:
// Cilium's cluster-pool mask (kube-system/cilium-config), on-demand blocks
// with Calico, or each node's spec.podCIDR.
func (r *NodeSettingsReconciler) podNetwork(ctx context.Context, config *k4allv1alpha1.ClusterConfig) (nodesettings.PodNetwork, error) {
	switch config.Spec.Networking.CNI.Type {
	case calicoCNI:
		return nodesettings.PodNetwork{Unlimited: true}, nil
	case ciliumCNI:
		ipam, err := hooks.ReadCiliumIPAM(ctx, r.reader(), "", config.Spec)
		if err != nil {
			return nodesettings.PodNetwork{}, err
		}
		if ipam.Mode == "cluster-pool" && ipam.MaskSize > 0 {
			return nodesettings.PodNetwork{MaskSize: ipam.MaskSize, Source: "Cilium's cluster-pool"}, nil
		}
	}
	return nodesettings.PodNetwork{Source: "the node's spec.podCIDR"}, nil
}

// writeConfig patches kubelet-config, as read for this reconcile: a write by
// kubeadm meanwhile is a conflict, and the next reconcile starts over.
func (r *NodeSettingsReconciler) writeConfig(ctx context.Context, cm *corev1.ConfigMap, a nodesettings.Action) error {
	if cm == nil {
		return fmt.Errorf("not found")
	}
	base := cm.DeepCopy()
	if a.KubeletConfig != "" {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[kubeletConfigKey] = a.KubeletConfig
	}
	if a.Record != nil {
		if cm.Annotations == nil {
			cm.Annotations = map[string]string{}
		}
		cm.Annotations[nodesettings.ConfigMapAnnotation] = a.Record.Encode()
	}
	return r.Patch(ctx, cm, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}), client.FieldOwner(k4allFieldManager))
}

// nodePatch is the merge patch of a Node action.
func nodePatch(a nodesettings.Action) map[string]interface{} {
	annotations := map[string]interface{}{}
	labels := map[string]interface{}{}
	spec := map[string]interface{}{}
	switch a.Kind {
	case nodesettings.Start, nodesettings.UpdateStep:
		annotations[nodesettings.StepAnnotation] = a.Step.Encode()
		if a.Kind == nodesettings.Start && a.Step.Class.Drains() {
			labels[drain.Label] = "true"
			if a.Step.Cordon == nodesettings.CordonByK4all {
				spec["unschedulable"] = true
			}
		}
	case nodesettings.SetTarget:
		annotations[nodesettings.TargetAnnotation] = a.Hash
	case nodesettings.Finish, nodesettings.LetGo:
		annotations[nodesettings.StepAnnotation] = nil
		if a.Step.Class.Drains() {
			labels[drain.Label] = nil
		}
		if a.Step.Cordon == nodesettings.CordonByK4all {
			spec["unschedulable"] = false
		}
		if a.Kind == nodesettings.LetGo {
			annotations[nodesettings.MessageAnnotation] = nil
			annotations[nodesettings.TargetAnnotation] = nil
			if a.Target != "" {
				annotations[nodesettings.TargetAnnotation] = a.Target
			}
		}
	}
	patch := map[string]interface{}{"metadata": map[string]interface{}{"annotations": annotations}}
	if len(labels) > 0 {
		patch["metadata"].(map[string]interface{})["labels"] = labels
	}
	if len(spec) > 0 {
		patch["spec"] = spec
	}
	return patch
}

func (r *NodeSettingsReconciler) patchNode(ctx context.Context, name string, patch map[string]interface{}) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	return r.Patch(ctx, node, client.RawPatch(types.MergePatchType, data), client.FieldOwner(k4allFieldManager))
}

// noteRefused adds why evictions from node were refused to its status.
func noteRefused(status *k4allv1alpha1.NodesStatus, node string, refused []string) {
	if status == nil || len(refused) == 0 {
		return
	}
	why := strings.Join(refused, "; ")
	for i := range status.PerNode {
		if status.PerNode[i].Name == node {
			status.PerNode[i].Message += "; " + why
		}
	}
	status.Rollout.Message += "; " + why
}

// writeStatus sets status.nodes and the NodeSettingsRollout condition on the
// latest ClusterConfig, nothing else: the ReleaseReconciler owns the rest.
func (r *NodeSettingsReconciler) writeStatus(ctx context.Context, name string, result nodesettings.Result) error {
	latest := &k4allv1alpha1.ClusterConfig{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: name}, latest); err != nil {
		return err
	}
	base := latest.DeepCopy()
	latest.Status.Nodes = result.Status
	if result.Condition != nil {
		meta.SetStatusCondition(&latest.Status.Conditions, *result.Condition)
	} else {
		meta.RemoveStatusCondition(&latest.Status.Conditions, k4allv1alpha1.ConditionNodeSettingsRollout)
	}
	if equality.Semantic.DeepEqual(base.Status, latest.Status) {
		return nil
	}
	return r.Status().Patch(ctx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// settingsRequeue is how soon to look again. Node changes trigger a
// reconcile anyway; drains, PDBs and a ClusterUpgrade ending need polling.
func settingsRequeue(status *k4allv1alpha1.NodesStatus) time.Duration {
	if status == nil {
		return 10 * time.Minute
	}
	for _, n := range status.PerNode {
		if n.Phase == nodesettings.PhaseDraining {
			return 10 * time.Second
		}
	}
	switch status.Rollout.Phase {
	case k4allv1alpha1.NodeRolloutRolling:
		return 30 * time.Second
	case k4allv1alpha1.NodeRolloutIdle:
		return 10 * time.Minute
	}
	return 2 * time.Minute
}

func (r *NodeSettingsReconciler) drainer() drainer {
	return drainer{Client: r.Client, APIReader: r.reader(), Self: r.Self}
}

func (r *NodeSettingsReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *NodeSettingsReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager sets up the controller with the Manager. Node changes
// reach it (the agents report through Node annotations), and ClusterUpgrade
// changes (the rollout waits for one to end).
func (r *NodeSettingsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toConfig := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		config, err := r.clusterConfig(ctx)
		if err != nil || config == nil {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: config.Name}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&k4allv1alpha1.ClusterConfig{}).
		Watches(&corev1.Node{}, toConfig).
		Watches(&k4allv1alpha1.ClusterUpgrade{}, toConfig).
		Named("nodesettings").
		Complete(r)
}
