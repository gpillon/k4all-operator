package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// releaseConditionTypes are the ClusterConfig condition types the
// ReleaseReconciler writes, and the only ones: it sets or removes each of
// them on every write of the status, and leaves every other type (the
// node-settings controller's NodeSettingsRollout) as the API server has it.
// A new condition the release reconcile sets goes here.
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
