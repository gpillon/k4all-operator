package engine

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Lists the cluster's admins add to.
//
// Manifests are applied with server-side apply, forcing ownership: a field
// upstream's manifest sets holds upstream's value after every apply. For an
// atomic list that is the whole list. kubevirt-cr.yaml sets
// spec.configuration.developerConfiguration.featureGates to [], so each
// apply (every upgradePath step, every spec change) turned off the KubeVirt
// feature gates an admin had turned on; cdi-cr.yaml forces
// [HonorWaitForFirstConsumer] the same way.
//
// For the lists below the apply writes the live object's entries, then
// upstream's that it lacks. An entry removed by hand stays out unless
// upstream's manifest sets it. A fresh install gets upstream's list as it is.

// additiveList is a list of strings in one kind of object, set by upstream's
// manifest and added to by the cluster's admins.
type additiveList struct {
	group, kind string
	path        []string
	// exclusive are pairs of entries never written together by the merge:
	// upstream's entry is left out when the list already has its partner.
	exclusive [][2]string
}

var additiveLists = []additiveList{
	{
		group: "kubevirt.io", kind: "KubeVirt",
		path: []string{"spec", "configuration", "developerConfiguration", "featureGates"},
		// HotplugVolumes, deprecated in v1.9, replaced by
		// DeclarativeHotplugVolumes, wins when both are set: the pair turns
		// declarative hotplug off.
		exclusive: [][2]string{{"HotplugVolumes", "DeclarativeHotplugVolumes"}},
	},
	{
		group: "cdi.kubevirt.io", kind: "CDI",
		path: []string{"spec", "config", "featureGates"},
	},
}

// additiveListsOf returns the additive lists of obj's kind.
func additiveListsOf(obj *unstructured.Unstructured) []additiveList {
	gvk := obj.GroupVersionKind()
	var lists []additiveList
	for _, l := range additiveLists {
		if l.group == gvk.Group && l.kind == gvk.Kind {
			lists = append(lists, l)
		}
	}
	return lists
}

// keepLiveEntries sets each additive list of obj, the object about to be
// applied, to the union of live's list and its own (mergeAdditive). With no
// live object (a fresh install) obj is left as it is. A list the cluster has
// and upstream's manifest stopped setting is still written: the operator owns
// it since its last apply, and the API server deletes a field its owner
// leaves out of an apply.
func keepLiveEntries(obj, live *unstructured.Unstructured) error {
	if live == nil {
		return nil
	}
	for _, l := range additiveListsOf(obj) {
		field := strings.Join(l.path, ".")
		upstream, inObj, err := unstructured.NestedStringSlice(obj.Object, l.path...)
		if err != nil {
			return fmt.Errorf("%s %s in the manifest: %w", obj.GetKind(), field, err)
		}
		current, inLive, err := unstructured.NestedStringSlice(live.Object, l.path...)
		if err != nil {
			return fmt.Errorf("%s %s in the cluster: %w", obj.GetKind(), field, err)
		}
		if !inObj && !inLive {
			continue
		}
		if err := unstructured.SetNestedStringSlice(obj.Object, mergeAdditive(current, upstream, l.exclusive), l.path...); err != nil {
			return fmt.Errorf("%s %s: %w", obj.GetKind(), field, err)
		}
	}
	return nil
}

// mergeAdditive returns live's entries in their order, then upstream's that
// live lacks, each entry once. An entry of upstream's whose exclusive partner
// is already in the list is left out: what the cluster has wins. A pair live
// already has is kept, as the admin set it.
func mergeAdditive(live, upstream []string, exclusive [][2]string) []string {
	merged := make([]string, 0, len(live)+len(upstream))
	have := map[string]bool{}
	for _, e := range live {
		if !have[e] {
			have[e] = true
			merged = append(merged, e)
		}
	}
	for _, e := range upstream {
		if have[e] {
			continue
		}
		if p, ok := partner(e, exclusive); ok && have[p] {
			continue
		}
		have[e] = true
		merged = append(merged, e)
	}
	return merged
}

// partner returns the entry e is exclusive with, if any.
func partner(e string, exclusive [][2]string) (string, bool) {
	for _, pair := range exclusive {
		switch e {
		case pair[0]:
			return pair[1], true
		case pair[1]:
			return pair[0], true
		}
	}
	return "", false
}
