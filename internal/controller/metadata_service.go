package controller

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/metadata"
)

// metadataSettings are the settings the metadata service runs with: the
// ClusterConfig's, and on also while virt is off but features.virt.metadata
// is on and the release still has the k4all-metadata component Installed.
// Its uninstall then waits for the virt group's (engine.RemovedAfter), which
// the VMs hold while they exist, and it serves those VMs until it goes.
// Turned off itself, it is off at once. rm may be nil (no release yet).
func metadataSettings(spec k4allv1alpha1.ClusterConfigSpec, rm *k4allv1alpha1.ReleaseManifest) hooks.MetadataSettings {
	s := hooks.MetadataSettingsFrom(spec)
	md := spec.Features.Virt.Metadata
	if s.Enabled || md == nil || !md.Enabled || rm == nil {
		return s
	}
	s.Enabled = rm.Status.Components[hooks.MetadataComponent].State == k4allv1alpha1.ComponentStateInstalled
	return s
}

// metadataServiceCondition is the ClusterConfig's MetadataService
// condition: whether the k4all-metadata component's policies are in place,
// from its status in the release. ok is false while the service is off (no
// condition then; metadataSettings).
func metadataServiceCondition(config *k4allv1alpha1.ClusterConfig, rm *k4allv1alpha1.ReleaseManifest) (metav1.Condition, bool) {
	s := metadataSettings(config.Spec, rm)
	if !s.Enabled {
		return metav1.Condition{}, false
	}
	cond := metav1.Condition{
		Type: k4allv1alpha1.ConditionMetadataService, Status: metav1.ConditionFalse,
		ObservedGeneration: config.Generation,
	}
	if _, inRelease := rm.Spec.Components[hooks.MetadataComponent]; !inRelease {
		cond.Reason = "NotInRelease"
		cond.Message = "the release has no k4all-metadata component (k4all v2.7.0 or later)"
		return cond, true
	}
	st, known := rm.Status.Components[hooks.MetadataComponent]
	switch {
	case !known || st.State == k4allv1alpha1.ComponentStatePending || st.State == k4allv1alpha1.ComponentStateInstalling:
		cond.Reason = "Pending"
		cond.Message = "the k4all-metadata component is not installed yet"
	case st.State == k4allv1alpha1.ComponentStateInstalled && strings.HasPrefix(st.Message, hookFailedPrefix):
		cond.Reason = "PolicyNotApplied"
		cond.Message = strings.TrimPrefix(st.Message, hookFailedPrefix)
	case st.State == k4allv1alpha1.ComponentStateInstalled:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Serving"
		cond.Message = metadataScopeText(s)
	default:
		cond.Reason = string(st.State)
		cond.Message = st.Message
	}
	return cond, true
}

func metadataScopeText(s hooks.MetadataSettings) string {
	who := fmt.Sprintf("VMs labelled %s=%s", metadata.LabelOptIn, metadata.OptInEnabled)
	if s.ServesAll() {
		who = fmt.Sprintf("every VM but those labelled %s=%s", metadata.LabelOptIn, metadata.OptInDisabled)
	}
	return fmt.Sprintf("%s get %s, in namespaces whose PodSecurity is not baseline or restricted, "+
		"through the masquerade binding; token audiences %s", who, metadata.Address, strings.Join(s.Audiences, ", "))
}
