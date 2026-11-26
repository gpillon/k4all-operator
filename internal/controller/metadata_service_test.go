package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
)

func mdClusterConfig(md *k4allv1alpha1.MetadataSettings) *k4allv1alpha1.ClusterConfig {
	c := &k4allv1alpha1.ClusterConfig{ObjectMeta: metav1.ObjectMeta{Generation: 7}}
	c.Spec.Features.Virt.Enabled = true
	c.Spec.Features.Virt.Metadata = md
	return c
}

func mdRelease(st *k4allv1alpha1.ComponentStatus) *k4allv1alpha1.ReleaseManifest {
	rm := &k4allv1alpha1.ReleaseManifest{Spec: k4allv1alpha1.ReleaseManifestSpec{
		Components: map[string]k4allv1alpha1.ComponentSpec{hooks.MetadataComponent: {Type: k4allv1alpha1.ComponentTypeManifest}},
	}}
	if st != nil {
		rm.Status.Components = map[string]k4allv1alpha1.ComponentStatus{hooks.MetadataComponent: *st}
	}
	return rm
}

func TestMetadataServiceCondition(t *testing.T) {
	on := &k4allv1alpha1.MetadataSettings{Enabled: true}
	installed := &k4allv1alpha1.ComponentStatus{State: k4allv1alpha1.ComponentStateInstalled, Message: "installed successfully"}
	for name, tc := range map[string]struct {
		config       *k4allv1alpha1.ClusterConfig
		rm           *k4allv1alpha1.ReleaseManifest
		status       metav1.ConditionStatus
		reason, text string
	}{
		"serving": {mdClusterConfig(on), mdRelease(installed), metav1.ConditionTrue, "Serving",
			"labelled k4all.magesgate.com/metadata=enabled"},
		"serving all": {mdClusterConfig(&k4allv1alpha1.MetadataSettings{Enabled: true, Scope: "All",
			Tokens: &k4allv1alpha1.MetadataTokens{Audiences: []string{"a", "b"}}}), mdRelease(installed),
			metav1.ConditionTrue, "Serving", "token audiences a, b"},
		"policy refused": {mdClusterConfig(on), mdRelease(&k4allv1alpha1.ComponentStatus{
			State: k4allv1alpha1.ComponentStateInstalled, Message: hookFailedPrefix + `token audience "kubernetes" refused`}),
			metav1.ConditionFalse, "PolicyNotApplied", `token audience "kubernetes" refused`},
		"failed": {mdClusterConfig(on), mdRelease(&k4allv1alpha1.ComponentStatus{
			State: k4allv1alpha1.ComponentStateFailed, Message: "boom"}), metav1.ConditionFalse, "Failed", "boom"},
		"not yet": {mdClusterConfig(on), mdRelease(nil), metav1.ConditionFalse, "Pending", "not installed yet"},
		"no component": {mdClusterConfig(on), &k4allv1alpha1.ReleaseManifest{}, metav1.ConditionFalse, "NotInRelease",
			"has no k4all-metadata component"},
	} {
		cond, ok := metadataServiceCondition(tc.config, tc.rm)
		if !ok || cond.Type != k4allv1alpha1.ConditionMetadataService || cond.Status != tc.status ||
			cond.Reason != tc.reason || !strings.Contains(cond.Message, tc.text) || cond.ObservedGeneration != 7 {
			t.Errorf("%s: %v %+v", name, ok, cond)
		}
	}
	// off: no condition
	virtOff := mdClusterConfig(on)
	virtOff.Spec.Features.Virt.Enabled = false
	for name, tc := range map[string]struct {
		config *k4allv1alpha1.ClusterConfig
		rm     *k4allv1alpha1.ReleaseManifest
	}{
		"absent":   {mdClusterConfig(nil), mdRelease(installed)},
		"disabled": {mdClusterConfig(&k4allv1alpha1.MetadataSettings{}), mdRelease(installed)},
		"virt off, uninstalled": {virtOff, mdRelease(&k4allv1alpha1.ComponentStatus{
			State: k4allv1alpha1.ComponentStateSkipped, Message: "disabled by ClusterConfig feature gate"})},
	} {
		if cond, ok := metadataServiceCondition(tc.config, tc.rm); ok {
			t.Errorf("%s: %+v", name, cond)
		}
	}
	// Virt off while VMs hold the virt group: k4all-metadata stays, queued
	// behind the group, and still serves (F2).
	queued := &k4allv1alpha1.ComponentStatus{State: k4allv1alpha1.ComponentStateInstalled,
		Message: "uninstall queued: waiting for hangar to be uninstalled first"}
	if cond, ok := metadataServiceCondition(virtOff, mdRelease(queued)); !ok ||
		cond.Status != metav1.ConditionTrue || cond.Reason != "Serving" {
		t.Errorf("virt off, k4all-metadata kept: %v %+v, want True (Serving)", ok, cond)
	}
}
