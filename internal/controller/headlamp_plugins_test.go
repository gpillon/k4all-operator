package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/engine"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// pluginEntries is one Headlamp plugin as k4all-release.yaml ships it: an
// initContainer that copies the plugin from image into an emptyDir, mounted
// read-only under the plugins dir.
func pluginEntries(name, image string) (initContainer, volume, mount map[string]interface{}) {
	initContainer = map[string]interface{}{
		"name":    name,
		"image":   image,
		"command": []interface{}{"sh", "-c", "cp -R /plugins/x/. /" + name + "/"},
		"volumeMounts": []interface{}{
			map[string]interface{}{"name": name, "mountPath": "/" + name},
		},
	}
	volume = map[string]interface{}{"name": name, "emptyDir": map[string]interface{}{"sizeLimit": "16Mi"}}
	mount = map[string]interface{}{"name": name, "mountPath": "/headlamp/plugins/" + name, "readOnly": true}
	return initContainer, volume, mount
}

const (
	k4allPluginImage  = "ghcr.io/gpillon/k4all-headlamp-plugin:0.2.0"
	hangarPluginImage = "ghcr.io/kalik1/hangar-headlamp-plugin:0.1.0"
	cairnPluginImage  = "ghcr.io/kalik1/cairn-headlamp-plugin:0.1.0"
)

// pluginValues are Helm values with the plugins, given as name=image pairs.
func pluginValues(t *testing.T, plugins ...string) map[string]interface{} {
	t.Helper()
	var inits, volumes, mounts []interface{}
	for i := 0; i+1 < len(plugins); i += 2 {
		c, v, m := pluginEntries(plugins[i], plugins[i+1])
		inits, volumes, mounts = append(inits, c), append(volumes, v), append(mounts, m)
	}
	return map[string]interface{}{
		"config":         map[string]interface{}{"pluginsDir": "/headlamp/plugins"},
		"initContainers": inits,
		"volumes":        volumes,
		"volumeMounts":   mounts,
	}
}

func rawValues(t *testing.T, values map[string]interface{}) *apiextensionsv1.JSON {
	t.Helper()
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return &apiextensionsv1.JSON{Raw: raw}
}

// headlampRelease is the headlamp component of the v2.7.0 release: the k4all,
// Hangar and Cairn plugins.
func headlampRelease(t *testing.T) k4allv1alpha1.ComponentSpec {
	return k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeHelm, Version: "0.46.0", Namespace: "headlamp",
		Values: rawValues(t, pluginValues(t,
			"k4all-plugin", k4allPluginImage,
			"hangar-plugin", hangarPluginImage,
			"cairn-plugin", cairnPluginImage)),
	}
}

// helmValues are the values Helm gets: the release's values with the
// reconciler's on top, merged as the engine does (lists replace).
func helmValues(t *testing.T, r *ReleaseReconciler, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) map[string]interface{} {
	t.Helper()
	extra, err := r.componentValues(context.Background(), "headlamp", spec, config)
	if err != nil {
		t.Fatal(err)
	}
	release := map[string]interface{}{}
	if err := json.Unmarshal(spec.Values.Raw, &release); err != nil {
		t.Fatal(err)
	}
	merged, err := engine.ApplyOverride(release, k4allv1alpha1.ComponentOverride{Values: rawValues(t, extra)})
	if err != nil {
		t.Fatal(err)
	}
	return normalize(t, merged)
}

// names lists the name of each entry of a Helm values list ("mountPath" for
// volumeMounts, to tell two mounts of one volume apart).
func names(t *testing.T, values map[string]interface{}, list string) []string {
	t.Helper()
	key := "name"
	if list == "volumeMounts" {
		key = "mountPath"
	}
	entries, _ := values[list].([]interface{})
	out := []string{}
	for _, e := range entries {
		out = append(out, fmt.Sprint(e.(map[string]interface{})[key]))
	}
	return out
}

func expectPlugins(t *testing.T, values map[string]interface{}, plugins ...string) {
	t.Helper()
	mounts := make([]string, 0, len(plugins))
	for _, p := range plugins {
		mounts = append(mounts, "/headlamp/plugins/"+p)
	}
	for list, want := range map[string][]string{
		"initContainers": plugins, "volumes": plugins, "volumeMounts": mounts,
	} {
		if got := names(t, values, list); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", list, got, want)
		}
	}
}

func virtOn() k4allv1alpha1.ClusterConfigSpec {
	return k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{Virt: k4allv1alpha1.VirtConfig{Enabled: true}}}
}

// Hangar is the console of KubeVirt: with virtualization on, Headlamp loads
// it next to the k4all plugin. Cairn needs Rook, which the ClusterConfig
// cannot turn on yet.
func TestHeadlampPlugins_HangarWithVirtualization(t *testing.T) {
	r := newValuesReconciler()

	got := helmValues(t, r, headlampRelease(t), virtOn())

	expectPlugins(t, got, "k4all-plugin", "hangar-plugin")
}

// Without virtualization there is no KubeVirt for Hangar to manage.
func TestHeadlampPlugins_NoHangarWithoutVirtualization(t *testing.T) {
	r := newValuesReconciler()

	got := helmValues(t, r, headlampRelease(t), k4allv1alpha1.ClusterConfigSpec{})

	expectPlugins(t, got, "k4all-plugin")
}

// withHeadlampOverride sets componentOverrides.headlamp.values.
func withHeadlampOverride(t *testing.T, config k4allv1alpha1.ClusterConfigSpec, values map[string]interface{}) k4allv1alpha1.ClusterConfigSpec {
	t.Helper()
	config.ComponentOverrides = map[string]k4allv1alpha1.ComponentOverride{
		"headlamp": {Values: rawValues(t, values)},
	}
	return config
}

// Helm replaces lists: an override that adds a plugin used to drop the
// plugins of the release. The lists merge by name instead.
func TestHeadlampPlugins_AnOverrideAddsItsPluginToTheRelease(t *testing.T) {
	r := newValuesReconciler()
	config := withHeadlampOverride(t, virtOn(), pluginValues(t, "mine-plugin", "registry.example.com/mine:1.0"))

	got := helmValues(t, r, headlampRelease(t), config)

	expectPlugins(t, got, "k4all-plugin", "hangar-plugin", "mine-plugin")
}

// images maps each initContainer to its image.
func images(t *testing.T, values map[string]interface{}) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, _ := values["initContainers"].([]interface{})
	for _, e := range entries {
		m := e.(map[string]interface{})
		out[fmt.Sprint(m["name"])] = fmt.Sprint(m["image"])
	}
	return out
}

// Hangar's k4all-headlamp-overrides.yaml, for releases before v2.7.0, copies
// the k4all plugin's entries (Helm replaced the lists) at the tag of its
// day. k4all pins its plugins: a copy of the published image at another tag
// gives way to the release's, so the plugin follows k4all upgrades.
func TestHeadlampPlugins_ACopyOfAPinnedPluginFollowsTheRelease(t *testing.T) {
	r := newValuesReconciler()
	config := withHeadlampOverride(t, virtOn(), pluginValues(t,
		"k4all-plugin", "ghcr.io/gpillon/k4all-headlamp-plugin:0.1.2",
		"hangar-plugin", "ghcr.io/kalik1/hangar-headlamp-plugin:0.0.9"))

	got := helmValues(t, r, headlampRelease(t), config)

	expectPlugins(t, got, "k4all-plugin", "hangar-plugin")
	want := map[string]string{"k4all-plugin": k4allPluginImage, "hangar-plugin": hangarPluginImage}
	if img := images(t, got); !reflect.DeepEqual(img, want) {
		t.Errorf("images = %v, want %v", img, want)
	}
}

// A plugin built elsewhere (another image repository: a development build)
// takes the place of the release's: the user asked for that build.
func TestHeadlampPlugins_AnotherBuildReplacesTheReleasePlugin(t *testing.T) {
	r := newValuesReconciler()
	dev := "localhost/hangar-headlamp-plugin:0.1.0-dev"
	config := withHeadlampOverride(t, virtOn(), pluginValues(t, "hangar-plugin", dev))

	got := helmValues(t, r, headlampRelease(t), config)

	expectPlugins(t, got, "k4all-plugin", "hangar-plugin")
	if img := images(t, got)["hangar-plugin"]; img != dev {
		t.Errorf("hangar-plugin image = %s, want %s", img, dev)
	}
}

// An empty list in the override still empties the list, as with Helm: the
// way to run Headlamp without plugins.
func TestHeadlampPlugins_AnEmptyOverrideListEmptiesIt(t *testing.T) {
	r := newValuesReconciler()
	config := withHeadlampOverride(t, virtOn(), map[string]interface{}{"initContainers": []interface{}{}})

	got := helmValues(t, r, headlampRelease(t), config)

	if inits := names(t, got, "initContainers"); len(inits) != 0 {
		t.Errorf("initContainers = %v, want none", inits)
	}
}
