package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/engine"
)

// Reasons of the RookReady condition.
const (
	rookReasonCheckFailed  = "CheckFailed"
	rookReasonInstalling   = "RookInstalling"
	rookReasonNotEnough    = "NotEnoughNodes"
	rookReasonPending      = "CephClusterPending"
	rookReasonDeleted      = "CephClusterDeleted"
	rookReasonDeleting     = "CephClusterDeleting"
	rookReasonProgressing  = "CephClusterProgressing"
	rookReasonFailure      = "CephClusterFailure"
	rookReasonHealthOK     = "CephHealthOK"
	rookReasonHealthWarn   = "CephHealthWarn"
	rookReasonHealthError  = "CephHealthError"
	rookCephComponent      = "rook-ceph"
	cephHealthOK           = "HEALTH_OK"
	cephHealthWarn         = "HEALTH_WARN"
	cephHealthErr          = "HEALTH_ERR"
	cephPhaseReady         = "Ready"
	cephPhaseFailure       = "Failure"
	cephHealthChecksInLine = 5
)

// rookReadyPollInterval is how soon a reconcile comes back while Rook works
// towards a ready Ceph cluster (rookComingUp): nothing watches the
// CephCluster, whose CRD comes with rook-ceph, so RookReady would otherwise
// lag behind it until the periodic resync.
const rookReadyPollInterval = 30 * time.Second

// rookComingUp tells whether conditions have RookReady not True for a
// reason the cluster moves past on its own: Rook installing, k4all about to
// create the CephCluster, the CephCluster's state or health, a failed
// check. Not while it waits on a person or on nodes (NotEnoughNodes,
// CephClusterDeleted): the resync is enough there, and a single node with
// smallCluster Refuse would poll forever.
func rookComingUp(conditions []metav1.Condition) bool {
	cond := meta.FindStatusCondition(conditions, k4allv1alpha1.ConditionRookReady)
	if cond == nil || cond.Status == metav1.ConditionTrue {
		return false
	}
	return cond.Reason != rookReasonNotEnough && cond.Reason != rookReasonDeleted
}

// rookReadyCondition is the RookReady condition of the ClusterConfig: the
// Ceph cluster of features.rook, or why k4all has not created it. false:
// Rook is off, and the ClusterConfig has no RookReady.
//
//   - no CephCluster: False, with why (Rook still installing; below 3 nodes
//     with smallCluster Refuse; the one k4all created was deleted, and is
//     not created again; or it comes at the next reconcile);
//   - a CephCluster: its phase as Rook reports it, then Ceph's health once
//     Ready. HEALTH_OK and HEALTH_WARN are True (the cluster serves
//     volumes; a single node allowed by smallCluster warns forever about its
//     single copy), HEALTH_ERR False; the message lists Ceph's checks.
func (r *ReleaseReconciler) rookReadyCondition(
	ctx context.Context, config *k4allv1alpha1.ClusterConfig, rm *k4allv1alpha1.ReleaseManifest,
) (metav1.Condition, bool) {
	if !engine.FeatureEnabled(engine.FeatureRook, config.Spec) {
		return metav1.Condition{}, false
	}
	cond := metav1.Condition{Type: k4allv1alpha1.ConditionRookReady, ObservedGeneration: config.Generation}
	set := func(status metav1.ConditionStatus, reason, message string) (metav1.Condition, bool) {
		cond.Status, cond.Reason, cond.Message = status, reason, message
		return cond, true
	}

	state, err := hooks.ReadRookCluster(ctx, r.Client, config.Spec)
	if err != nil {
		return set(metav1.ConditionUnknown, rookReasonCheckFailed, err.Error())
	}
	if state.CephCluster != nil {
		return cephClusterCondition(cond, state)
	}
	switch rookCeph := rm.Status.Components[rookCephComponent]; {
	case state.Seeded != "":
		return set(metav1.ConditionFalse, rookReasonDeleted, fmt.Sprintf(
			"CephCluster %s, which k4all %s, is gone: k4all does not create it again. "+
				"To start over, turn Rook off and wipe /var/lib/rook and the OSD disks before turning it on",
			state.Name, state.Seeded))
	case state.Refusal != "":
		return set(metav1.ConditionFalse, rookReasonNotEnough, state.Refusal)
	case rookCeph.State != k4allv1alpha1.ComponentStateInstalled:
		message := fmt.Sprintf("%s is %s", rookCephComponent, orPending(rookCeph.State))
		if rookCeph.Message != "" {
			message += ": " + rookCeph.Message
		}
		return set(metav1.ConditionFalse, rookReasonInstalling, message)
	default:
		return set(metav1.ConditionFalse, rookReasonPending, fmt.Sprintf(
			"k4all creates CephCluster %s at the next reconcile", state.Name))
	}
}

func orPending(state k4allv1alpha1.ComponentState) k4allv1alpha1.ComponentState {
	if state == "" {
		return k4allv1alpha1.ComponentStatePending
	}
	return state
}

// cephClusterCondition summarizes the CephCluster's status: Rook's phase
// and message, then Ceph's health and its checks.
func cephClusterCondition(cond metav1.Condition, state hooks.RookCluster) (metav1.Condition, bool) {
	cluster := state.CephCluster
	phase, _, _ := unstructured.NestedString(cluster.Object, "status", "phase")
	message, _, _ := unstructured.NestedString(cluster.Object, "status", "message")
	health, _, _ := unstructured.NestedString(cluster.Object, "status", "ceph", "health")
	about := "CephCluster " + state.Name
	switch {
	case cluster.GetDeletionTimestamp() != nil:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, rookReasonDeleting, about+" is being deleted"
		return cond, true
	case phase == "":
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, rookReasonProgressing,
			about+": waiting for Rook to report its state"
		return cond, true
	case phase == cephPhaseFailure:
		cond.Status, cond.Reason = metav1.ConditionFalse, rookReasonFailure
		cond.Message = joinNonEmpty(about+": "+phase, message)
		return cond, true
	case phase != cephPhaseReady:
		cond.Status, cond.Reason = metav1.ConditionFalse, rookReasonProgressing
		cond.Message = joinNonEmpty(about+": "+phase, message)
		return cond, true
	}

	cond.Message = about + ": " + phase + ", " + orUnknown(health)
	if checks := cephHealthChecks(cluster); checks != "" {
		cond.Message += ": " + checks
	}
	switch health {
	case cephHealthOK:
		cond.Status, cond.Reason = metav1.ConditionTrue, rookReasonHealthOK
	case cephHealthWarn:
		cond.Status, cond.Reason = metav1.ConditionTrue, rookReasonHealthWarn
	case cephHealthErr:
		cond.Status, cond.Reason = metav1.ConditionFalse, rookReasonHealthError
	default: // Ready, health not reported yet
		cond.Status, cond.Reason = metav1.ConditionFalse, rookReasonProgressing
	}
	return cond, true
}

// cephHealthChecks lists Ceph's health checks (status.ceph.details) as
// "NAME: message", by name, at most cephHealthChecksInLine of them.
func cephHealthChecks(cluster *unstructured.Unstructured) string {
	details, _, _ := unstructured.NestedMap(cluster.Object, "status", "ceph", "details")
	names := make([]string, 0, len(details))
	for name := range details {
		names = append(names, name)
	}
	sort.Strings(names)
	checks := make([]string, 0, len(names))
	for i, name := range names {
		if i == cephHealthChecksInLine {
			checks = append(checks, fmt.Sprintf("and %d more", len(names)-i))
			break
		}
		msg, _, _ := unstructured.NestedString(details, name, "message")
		checks = append(checks, joinNonEmpty(name, msg))
	}
	return strings.Join(checks, "; ")
}

func joinNonEmpty(head, tail string) string {
	if tail == "" {
		return head
	}
	return head + ": " + tail
}

func orUnknown(health string) string {
	if health == "" {
		return "health not reported yet"
	}
	return health
}
