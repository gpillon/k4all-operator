package controller

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// releaseConditionTypes are the ClusterConfig condition types the
// ReleaseReconciler writes, and the only ones: it sets or removes each of
// them on every write of the status, and leaves every other type (the
// node-settings controller's NodeSettingsRollout) as the API server has it.
// A new condition the release reconcile sets goes here: unlisted, it is
// never written (reportUnlistedConditions logs it, the tests fail on it). Another
// controller's condition (NodeSettingsRollout) must not: every release
// reconcile would remove it, or write back a stale copy.
var releaseConditionTypes = []string{
	"Reconciled",
	"Degraded",
	migrationsAheadCondition,
	k4allv1alpha1.ConditionPodNetworkOverlap,
	k4allv1alpha1.ConditionKubeVirtSettings,
	k4allv1alpha1.ConditionRookReady,
	k4allv1alpha1.ConditionMetadataService,
}

// copyReleaseStatus copies onto latest, the ClusterConfig status as the API
// server has it, what the ReleaseReconciler owns of own, its copy of the
// status after a reconcile: observedGeneration, activeCNI, features,
// lastReconcileTime, and each of releaseConditionTypes (set as own has it,
// removed when own has none). status.nodes and the other condition types
// stay as latest has them: own was read at the start of the reconcile, and
// another controller may have written them since.
func copyReleaseStatus(own k4allv1alpha1.ClusterConfigStatus, latest *k4allv1alpha1.ClusterConfigStatus) {
	latest.ObservedGeneration = own.ObservedGeneration
	latest.ActiveCNI = own.ActiveCNI
	latest.Features = own.Features
	latest.LastReconcileTime = own.LastReconcileTime
	for _, conditionType := range releaseConditionTypes {
		if cond := meta.FindStatusCondition(own.Conditions, conditionType); cond != nil {
			meta.SetStatusCondition(&latest.Conditions, *cond)
		} else {
			meta.RemoveStatusCondition(&latest.Conditions, conditionType)
		}
	}
}

// unlistedConditions is an error naming the condition types the release
// reconcile set, changed or removed on its copy of the ClusterConfig status,
// from read (the status before it did) to now, that releaseConditionTypes
// does not list: copyReleaseStatus never writes them. Nil when none.
func unlistedConditions(read, now []metav1.Condition) error {
	var unlisted []string
	for _, conds := range [][]metav1.Condition{read, now} {
		for _, cond := range conds {
			t := cond.Type
			if slices.Contains(releaseConditionTypes, t) || slices.Contains(unlisted, t) {
				continue
			}
			if !equality.Semantic.DeepEqual(meta.FindStatusCondition(read, t), meta.FindStatusCondition(now, t)) {
				unlisted = append(unlisted, t)
			}
		}
	}
	if len(unlisted) == 0 {
		return nil
	}
	sort.Strings(unlisted)
	return fmt.Errorf("the release reconcile sets ClusterConfig condition types that releaseConditionTypes "+
		"does not list, so they are never written: %s (add them to releaseConditionTypes)", strings.Join(unlisted, ", "))
}

// unlistedConditionsHook, when set, is called with every unlistedConditions
// error a reconcile finds. The package's tests set it (TestMain) to fail, so
// every test through Reconcile that reaches the code setting an unlisted
// condition trips; the operator leaves it nil.
var unlistedConditionsHook func(error)

// reportUnlistedConditions logs, at error level, the condition types the
// release reconcile set without listing them (unlistedConditions), and
// hands them to unlistedConditionsHook. A programming error the tests catch:
// the reconcile carries on as it would without them.
func reportUnlistedConditions(log logr.Logger, read, now []metav1.Condition) {
	err := unlistedConditions(read, now)
	if err == nil {
		return
	}
	log.Error(err, "ClusterConfig conditions not written")
	if unlistedConditionsHook != nil {
		unlistedConditionsHook(err)
	}
}
