package engine

import (
	"fmt"
	"slices"
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
//
// A component hook that owns entries of such a list (the VirtHook: the
// KubeVirt gates the ClusterConfig turns on and off) merges it itself, as an
// ObjectMutator, with MergeSharedList and the entries it owns.

// additiveList is a list of strings in one kind of object, set by upstream's
// manifest and added to by the cluster's admins.
type additiveList struct {
	group, kind string
	path        []string
	// exclusive are pairs of entries never written together by the merge:
	// an entry it adds (the ClusterConfig's, upstream's) is left out when the
	// list already has its partner.
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
// applied, to the union of live's list and its own (MergeSharedList, owning
// no entry). With no live object (a fresh install) obj is left as it is. A
// list the cluster has and upstream's manifest stopped setting is still
// written: the operator owns it since its last apply, and the API server
// deletes a field its owner leaves out of an apply.
func keepLiveEntries(obj, live *unstructured.Unstructured) error {
	if live == nil {
		return nil
	}
	for _, l := range additiveListsOf(obj) {
		if err := MergeSharedList(obj, live, l.path, ListOwnership{}); err != nil {
			return err
		}
	}
	return nil
}

// ListOwnership is what k4all owns in a list shared with the cluster's
// admins and consoles (spec §7.2, R6): the entries the ClusterConfig turns
// on and off, and the ones k4all added at its previous apply, as the
// object's annotation records them. The zero value owns nothing: the merge
// is then the union of the cluster's entries and upstream's.
type ListOwnership struct {
	// Added are the entries k4all added at its previous apply.
	Added []string
	// On are the entries the ClusterConfig turns on, Off the ones it turns off.
	On, Off []string
}

// MergeSharedList sets the string list at path of desired, the object about
// to be applied, to the merge (mergeShared) of live's list, desired's own
// (upstream's) and own, with the exclusive pairs of the additive list at
// path, if any. live is nil on a fresh install. An empty result is left out
// of the apply when live has no entry either, so the apply does not own an
// empty list (spec R5); it is written empty when live has entries: they are
// all k4all's to remove, and leaving the list out would remove them only if
// k4all-operator owned the list, not if another manager wrote it last.
func MergeSharedList(desired, live *unstructured.Unstructured, path []string, own ListOwnership) error {
	field := strings.Join(path, ".")
	upstream, _, err := unstructured.NestedStringSlice(desired.Object, path...)
	if err != nil {
		return fmt.Errorf("%s %s in the manifest: %w", desired.GetKind(), field, err)
	}
	var current []string
	if live != nil {
		if current, _, err = unstructured.NestedStringSlice(live.Object, path...); err != nil {
			return fmt.Errorf("%s %s in the cluster: %w", desired.GetKind(), field, err)
		}
	}
	merged := mergeShared(current, upstream, own, exclusiveAt(desired, path))
	if len(merged) == 0 && len(current) == 0 {
		removeField(desired.Object, path)
		return nil
	}
	if err := unstructured.SetNestedStringSlice(desired.Object, merged, path...); err != nil {
		return fmt.Errorf("%s %s: %w", desired.GetKind(), field, err)
	}
	return nil
}

// mergeShared returns the list to apply, spec §7.2:
//
//	((live − (own.Added − own.On)) ∪ own.On ∪ upstream) − own.Off
//
// live's entries in their order, then own.On's and upstream's that the list
// lacks, each entry once: the cluster's entries stay, except the ones k4all
// added and no longer wants, and the ones turned off. An entry of own.On or
// upstream whose exclusive partner is already in the list is left out: what
// the cluster has wins, then the ClusterConfig over upstream. A pair the
// cluster already has is kept, as the admin set it.
func mergeShared(live, upstream []string, own ListOwnership, exclusive [][2]string) []string {
	off := make(map[string]bool, len(own.Off))
	for _, e := range own.Off {
		off[e] = true
	}
	drop := make(map[string]bool, len(own.Added)+len(own.Off))
	for e := range off {
		drop[e] = true
	}
	for _, e := range own.Added {
		if !slices.Contains(own.On, e) {
			drop[e] = true
		}
	}
	merged := make([]string, 0, len(live)+len(own.On)+len(upstream))
	have := map[string]bool{}
	for _, e := range live {
		if !drop[e] && !have[e] {
			have[e] = true
			merged = append(merged, e)
		}
	}
	for _, list := range [][]string{own.On, upstream} {
		for _, e := range list {
			if have[e] || off[e] {
				continue
			}
			if p, ok := partner(e, exclusive); ok && have[p] {
				continue
			}
			have[e] = true
			merged = append(merged, e)
		}
	}
	return merged
}

// exclusiveAt returns the exclusive pairs of obj's additive list at path;
// none when path is not one.
func exclusiveAt(obj *unstructured.Unstructured, path []string) [][2]string {
	for _, l := range additiveListsOf(obj) {
		if slices.Equal(l.path, path) {
			return l.exclusive
		}
	}
	return nil
}

// ExclusivePartner returns the entry that entry is never written with in the
// additive list at path of the objects of group and kind, if any: KubeVirt's
// HotplugVolumes and DeclarativeHotplugVolumes.
func ExclusivePartner(group, kind string, path []string, entry string) (string, bool) {
	for _, l := range additiveLists {
		if l.group == group && l.kind == kind && slices.Equal(l.path, path) {
			return partner(entry, l.exclusive)
		}
	}
	return "", false
}

// removeField removes the field at path from obj, then the maps above it
// that it leaves empty: the apply then owns none of them.
func removeField(obj map[string]interface{}, path []string) {
	unstructured.RemoveNestedField(obj, path...)
	for i := len(path) - 1; i > 0; i-- {
		parent, found, err := unstructured.NestedMap(obj, path[:i]...)
		if err != nil || !found || len(parent) > 0 {
			return
		}
		unstructured.RemoveNestedField(obj, path[:i]...)
	}
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
