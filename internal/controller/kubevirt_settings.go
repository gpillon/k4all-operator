package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

// setKubeVirtSettings sets the KubeVirtSettings condition of config from its
// features.virt.kubevirt (hooks.KubeVirtSettingsCondition), and removes it
// while virt is off or the ClusterConfig sets none of these settings.
func (r *ReleaseReconciler) setKubeVirtSettings(ctx context.Context, config *k4allv1alpha1.ClusterConfig) {
	virt := config.Spec.Features.Virt
	if !virt.Enabled || virt.KubeVirt == nil {
		meta.RemoveStatusCondition(&config.Status.Conditions, k4allv1alpha1.ConditionKubeVirtSettings)
		return
	}
	cond := hooks.KubeVirtSettingsCondition(ctx, r.Client, *virt.KubeVirt)
	cond.ObservedGeneration = config.Generation
	meta.SetStatusCondition(&config.Status.Conditions, cond)
}
