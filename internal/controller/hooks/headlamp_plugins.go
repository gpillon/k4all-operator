package hooks

import (
	"encoding/json"
	"fmt"
	"strings"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/engine"
)

// Headlamp loads each plugin k4all ships (k4all-release.yaml, headlamp
// values) from three entries named after it: an initContainer that copies the
// plugin from its image into an emptyDir volume, and the volume's read-only
// mount under the plugins dir.
var pluginLists = []string{"initContainers", "volumes", "volumeMounts"}

// featurePlugins are the plugins Headlamp gets only with their feature on,
// or held (its components still installed): Hangar is the console of
// KubeVirt, Cairn the console of Rook.
var featurePlugins = map[string]string{
	"hangar-plugin": engine.FeatureVirt,
	"cairn-plugin":  engine.FeatureRook,
}

// MergeOverride gives Headlamp the plugins of the release whose feature is
// on or held: a feature turned off keeps its console while its uninstall is
// refused or under way (the refusal tells the admin to clean up there), and
// loses it once its components are gone. It layers
// componentOverrides.headlamp on the values. Helm replaces
// lists, so an override that added a plugin used to drop the release's; the
// plugin lists merge by key instead (initContainers and volumes by name,
// volumeMounts by mountPath): an override entry takes the place of the
// release's entry with its key, the others come after the release's. k4all
// pins the versions of its plugins: an override initContainer that runs the
// image repository of the release's entry with its name is a copy of that
// entry (made while Helm replaced the lists), and the release's stays. An
// empty list in the override empties the list, as with Helm.
func (h *HeadlampHook) MergeOverride(spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
	held HeldFeatures, values map[string]interface{}, override k4allv1alpha1.ComponentOverride) (map[string]interface{}, error) {
	out, err := engine.ApplyOverride(values, override)
	if err != nil {
		return nil, err
	}
	release, err := specValues(spec)
	if err != nil {
		return nil, err
	}
	overlay, err := overrideValues(override)
	if err != nil {
		return nil, err
	}
	for _, list := range pluginLists {
		entries, inRelease := release[list].([]interface{})
		extra, inOverride := overlay[list].([]interface{})
		if !inRelease && !inOverride {
			continue
		}
		if inOverride && len(extra) == 0 {
			out[list] = []interface{}{} // as with Helm: no plugins
			continue
		}
		kept := make([]interface{}, 0, len(entries)+len(extra))
		for _, e := range entries {
			if !featureOff(entryName(e), config, held) {
				kept = append(kept, e)
			}
		}
		out[list] = mergeByKey(kept, extra, listKey(list), list == "initContainers")
	}
	return out, nil
}

// HeldFeaturesUsed implements OverrideMerger: the held features with a
// console plugin.
func (h *HeadlampHook) HeldFeaturesUsed(held HeldFeatures) HeldFeatures {
	var used HeldFeatures
	for _, gate := range held {
		for _, feature := range featurePlugins {
			if gate == feature {
				used = append(used, gate)
				break
			}
		}
	}
	return used
}

// mergeByKey puts each entry of extra in place of the entry of base with
// the same key, or after base. With pinned, an entry of base stays when the
// one of extra runs another tag of its image.
func mergeByKey(base, extra []interface{}, key string, pinned bool) []interface{} {
	out := append([]interface{}{}, base...)
	for _, e := range extra {
		at := -1
		for i, b := range out {
			if field(b, key) != "" && field(b, key) == field(e, key) {
				at = i
				break
			}
		}
		switch {
		case at < 0:
			out = append(out, e)
		case pinned && imageRepository(field(out[at], "image")) == imageRepository(field(e, "image")):
			// a copy of the release's entry: the release pins its tag
		default:
			out[at] = e
		}
	}
	return out
}

// imageRepository is an image reference without its tag and digest.
func imageRepository(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		image = image[:colon]
	}
	return image
}

// listKey is what tells two entries of a list apart: Kubernetes refuses two
// initContainers or volumes with one name, or two mounts on one path.
func listKey(list string) string {
	if list == "volumeMounts" {
		return "mountPath"
	}
	return "name"
}

// featureOff: the entry belongs to a plugin whose feature is off and not
// held.
func featureOff(name string, config k4allv1alpha1.ClusterConfigSpec, held HeldFeatures) bool {
	feature, gated := featurePlugins[name]
	return gated && !engine.FeatureEnabled(feature, config) && !held.Holds(feature)
}

// entryName is the name of an initContainer, a volume or a volume mount.
func entryName(entry interface{}) string {
	return field(entry, "name")
}

// field is a string field of a list entry, "" when it has none.
func field(entry interface{}, key string) string {
	m, _ := entry.(map[string]interface{})
	s, _ := m[key].(string)
	return s
}

// overrideValues are the values of componentOverrides.headlamp.
func overrideValues(override k4allv1alpha1.ComponentOverride) (map[string]interface{}, error) {
	values := map[string]interface{}{}
	if override.Values == nil || len(override.Values.Raw) == 0 {
		return values, nil
	}
	if err := json.Unmarshal(override.Values.Raw, &values); err != nil {
		return nil, fmt.Errorf("componentOverrides.headlamp values: %w", err)
	}
	return values, nil
}

// specValues are the component's values in the ReleaseManifest.
func specValues(spec k4allv1alpha1.ComponentSpec) (map[string]interface{}, error) {
	values := map[string]interface{}{}
	if spec.Values == nil || len(spec.Values.Raw) == 0 {
		return values, nil
	}
	if err := json.Unmarshal(spec.Values.Raw, &values); err != nil {
		return nil, fmt.Errorf("headlamp values: %w", err)
	}
	return values, nil
}
