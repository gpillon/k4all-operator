package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/controller/hooks"
	"github.com/gpillon/k4all-operator/internal/engine"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
// reconciler's on top, merged as the engine does (lists replace), with the
// features held.
func helmValues(t *testing.T, r *ReleaseReconciler, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
	held ...string) map[string]interface{} {
	t.Helper()
	extra, err := r.componentValues(context.Background(), "headlamp", spec, config, held)
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
// it next to the k4all plugin. Cairn needs Rook, off here.
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

// Cairn is the console of Rook: Headlamp loads it with features.rook
// enabled, with or without virtualization, and drops it when Rook is off.
func TestHeadlampPlugins_CairnWithRook(t *testing.T) {
	r := newValuesReconciler()
	rookOn := func(config k4allv1alpha1.ClusterConfigSpec) k4allv1alpha1.ClusterConfigSpec {
		config.Features.Rook = &k4allv1alpha1.RookConfig{Enabled: true}
		return config
	}

	expectPlugins(t, helmValues(t, r, headlampRelease(t), rookOn(k4allv1alpha1.ClusterConfigSpec{})),
		"k4all-plugin", "cairn-plugin")
	expectPlugins(t, helmValues(t, r, headlampRelease(t), rookOn(virtOn())),
		"k4all-plugin", "hangar-plugin", "cairn-plugin")

	off := virtOn()
	off.Features.Rook = &k4allv1alpha1.RookConfig{Enabled: false}
	expectPlugins(t, helmValues(t, r, headlampRelease(t), off), "k4all-plugin", "hangar-plugin")
}

// The v2.7.0 VM round (F6): Rook turned off with a CephCluster is refused,
// yet Headlamp lost Cairn, the console the refusal tells the admin to clean
// up with; virt turned off with VMs lost Hangar the same way. A console
// stays while its feature is held (components still installed: refused,
// queued, uninstalling) and goes with them. Another held gate (the metadata
// service queued, the snapshot controller) keeps no console.
func TestHeadlampPlugins_AConsoleStaysWhileItsFeatureIsHeld(t *testing.T) {
	r := newValuesReconciler()
	rookOff := k4allv1alpha1.ClusterConfigSpec{}
	rookOff.Features.Rook = &k4allv1alpha1.RookConfig{Enabled: false}

	expectPlugins(t, helmValues(t, r, headlampRelease(t), rookOff, "rook"), "k4all-plugin", "cairn-plugin")
	expectPlugins(t, helmValues(t, r, headlampRelease(t), rookOff, "virt"), "k4all-plugin", "hangar-plugin")
	expectPlugins(t, helmValues(t, r, headlampRelease(t), rookOff, "rook", "virt"),
		"k4all-plugin", "hangar-plugin", "cairn-plugin")
	expectPlugins(t, helmValues(t, r, headlampRelease(t), rookOff, "snapshots", "virtMetadata"), "k4all-plugin")
	expectPlugins(t, helmValues(t, r, headlampRelease(t), rookOff), "k4all-plugin")
}

// Headlamp is applied again when a console's feature stops being held: the
// held features its plugins follow are in its appliedHash, the others not,
// and no other component's hash has them.
func TestAppliedHash_HeadlampFollowsTheHeldConsoles(t *testing.T) {
	r := newValuesReconciler()
	spec := headlampRelease(t)
	var config k4allv1alpha1.ClusterConfigSpec
	none := r.appliedHash("headlamp", spec, config, nil)

	if r.appliedHash("headlamp", spec, config, hooks.HeldFeatures{"rook"}) == none {
		t.Error("headlamp: Rook held or not, the same hash: Cairn would stay after the Rook group is gone")
	}
	if r.appliedHash("headlamp", spec, config, hooks.HeldFeatures{"snapshots", "virtMetadata"}) != none {
		t.Error("headlamp: a held gate without a console changes the hash: Headlamp re-applied for nothing")
	}
	for _, name := range []string{"cilium", "kubevirt", "topolvm"} {
		if r.appliedHash(name, spec, config, hooks.HeldFeatures{"rook", "virt"}) != r.appliedHash(name, spec, config, nil) {
			t.Errorf("%s: the held features change its hash, want only Headlamp's", name)
		}
	}
}

// Through the reconcile: Rook off with a CephCluster holds the Rook group,
// and Headlamp, applied after the group's uninstall is decided, has the
// hash with Rook held; once the group is gone, the hash without, so the
// next apply drops Cairn.
func TestReconcile_HeadlampFollowsTheRookGroup(t *testing.T) {
	components := rookGroup()
	components["headlamp"] = gatedComponent("headlamp", "")
	c, r := groupsOnEnvtest(t, components)
	withCephClusterCRD(t, c)
	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "ceph.rook.io/v1",
		"kind":       "CephCluster",
		"metadata":   map[string]interface{}{"name": "rook-ceph", "namespace": metav1.NamespaceDefault},
		"spec": map[string]interface{}{
			"cephVersion":     map[string]interface{}{"image": "quay.io/ceph/ceph:v20.2.4"},
			"dataDirHostPath": "/var/lib/rook",
		},
	}}
	createAll(t, c, cluster)
	// Headlamp's hash as the reconcile computes it: from the component and
	// the ClusterConfig as the API server has them (defaults, migrations).
	hash := func(held hooks.HeldFeatures) string {
		t.Helper()
		cc := &k4allv1alpha1.ClusterConfig{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: "k4all-cluster-config"}, cc); err != nil {
			t.Fatal(err)
		}
		rm := &k4allv1alpha1.ReleaseManifest{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: "k4all"}, rm); err != nil {
			t.Fatal(err)
		}
		return r.appliedHash("headlamp", rm.Spec.Components["headlamp"], cc.Spec, held)
	}

	reconcileProbe(t, r)
	held := hash(hooks.HeldFeatures{"rook"})
	if st := componentStatus(t, c, "headlamp"); st.AppliedHash != held {
		t.Errorf("headlamp applied from %q, want the hash with Rook held (%q)", st.AppliedHash, held)
	}

	if err := deleteNow(c, cluster); err != nil {
		t.Fatal(err)
	}
	reconcileProbe(t, r)
	assertGone(t, c, "cairn", "ceph-csi-drivers", "rook-ceph")
	free := hash(nil)
	if st := componentStatus(t, c, "headlamp"); st.AppliedHash != free {
		t.Errorf("headlamp applied from %q, want the hash with nothing held (%q)", st.AppliedHash, free)
	}
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
