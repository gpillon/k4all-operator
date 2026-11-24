package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

var (
	rookCRDsOnce sync.Once
	rookCRDsErr  error
)

// rookEnvtest is the package's envtest API server with Rook's CRDs (v1.21.2,
// so what the hook creates is checked against Rook's own schema), and the
// rook-ceph namespace.
func rookEnvtest(t *testing.T) client.Client {
	t.Helper()
	c, cfg := envtestClient(t)
	rookCRDsOnce.Do(func() {
		_, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
			Paths: []string{
				filepath.Join("testdata", "crds", "ceph.rook.io_cephclusters.yaml"),
				filepath.Join("testdata", "crds", "ceph.rook.io_cephblockpools.yaml"),
			},
			ErrorIfPathMissing: true,
		})
		if err == nil {
			err = client.IgnoreAlreadyExists(c.Create(context.Background(),
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: rookNamespace}}))
		}
		rookCRDsErr = err
	})
	if rookCRDsErr != nil {
		t.Fatalf("install Rook's CRDs: %v", rookCRDsErr)
	}
	return c
}

// cephNodeNames numbers the nodes of the Rook tests.
var cephNodeNames int

// rookNodes creates n untainted nodes, deleted when the test ends.
func rookNodes(t *testing.T, c client.Client, n int) {
	t.Helper()
	for range n {
		cephNodeNames++
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ceph-node-%d", cephNodeNames)}}
		if err := c.Create(context.Background(), node); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), node) })
	}
}

// forgetRook deletes, when the test ends, what the Rook hook may have created.
func forgetRook(t *testing.T, c client.Client) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, obj := range []client.Object{
			newObject(cephAPIVersion, cephClusterKind, rookNamespace, cephClusterName),
			newObject(cephAPIVersion, cephBlockPoolKind, rookNamespace, cephPoolName),
			&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: cephStorageClassName}},
			newObject(snapshotAPIVersion, snapshotClassKind, "", cephSnapshotClassName),
			newObject(storageProfileAPIVersion, storageProfileKind, "", cephStorageClassName),
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: rookNamespace, Name: "k4all-rook-seeds"}},
		} {
			if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
				t.Errorf("delete %T %s: %v", obj, obj.GetName(), err)
			}
		}
	})
}

// optionalCRDs are the APIs of CDI and of the CSI snapshot controller: k4all
// clusters have them only with virtualization on, and with the snapshot CRDs.
var optionalCRDs = []string{
	filepath.Join("testdata", "crds", "cdi.kubevirt.io_storageprofiles.yaml"),
	filepath.Join("testdata", "crds", "snapshot.storage.k8s.io_volumesnapshotclasses.yaml"),
}

// withOptionalAPIs installs the optional CRDs for the test, and removes them
// when it ends, waiting until they are gone.
func withOptionalAPIs(t *testing.T, c client.Client) {
	t.Helper()
	_, cfg := envtestClient(t)
	opts := envtest.CRDInstallOptions{Paths: optionalCRDs, ErrorIfPathMissing: true}
	if _, err := envtest.InstallCRDs(cfg, opts); err != nil {
		t.Fatalf("install the optional CRDs: %v", err)
	}
	t.Cleanup(func() {
		if err := envtest.UninstallCRDs(cfg, opts); err != nil {
			t.Errorf("uninstall the optional CRDs: %v", err)
		}
		ctx := context.Background()
		for _, name := range []string{"storageprofiles.cdi.kubevirt.io", "volumesnapshotclasses.snapshot.storage.k8s.io"} {
			crd := newObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", name)
			err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
				func(ctx context.Context) (bool, error) {
					return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(crd), crd)), nil
				})
			if err != nil {
				t.Errorf("CRD %s still there: %v", name, err)
			}
		}
	})
}

// getObject reads an object of the API, failing the test when it is not there.
func getObject(t *testing.T, c client.Client, apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	u := newObject(apiVersion, kind, namespace, name)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
		t.Fatalf("get %s %s: %v", kind, name, err)
	}
	return u
}

// isSeed tells whether k4all created the object as a seed, and nobody wrote it since.
func isSeed(t *testing.T, obj client.Object) {
	t.Helper()
	if got := managers(obj); len(got) != 1 || got[0] != seedFieldManager {
		t.Errorf("%s: field managers %v, want only %q", obj.GetName(), got, seedFieldManager)
	}
}

func newObject(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func getCephCluster(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	u := newObject(cephAPIVersion, cephClusterKind, rookNamespace, cephClusterName)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
		t.Fatalf("get CephCluster: %v", err)
	}
	return u
}

// managers lists the field managers of an object.
func managers(obj client.Object) []string {
	out := make([]string, 0, len(obj.GetManagedFields()))
	for _, e := range obj.GetManagedFields() {
		out = append(out, e.Manager)
	}
	return out
}

// rookSpec is the rook-ceph component as the k4all ReleaseManifest has it.
var rookSpec = k4allv1alpha1.ComponentSpec{
	Type: k4allv1alpha1.ComponentTypeHelm, Version: "1.21.2", Chart: "rook-ceph",
	Namespace: rookNamespace, Image: "quay.io/ceph/ceph:v20.2.4", FeatureGate: "rook",
}

func syncRook(t *testing.T, h *RookHook) error {
	t.Helper()
	_, err := h.Sync(context.Background(), rookSpec, k4allv1alpha1.ClusterConfigSpec{})
	return err
}

// k4all creates the CephCluster with its defaults under its own field
// manager for created objects, not "k4all-operator": the consoles (Cairn)
// show the fields of "k4all-operator" read-only, and these are day-2 fields.
// The one exception is the Ceph image (D18, TestRookHook_OwnsOnlyTheCephImage).
func TestRookHook_CreatesTheCephClusterAsASeed(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 3)
	forgetRook(t, c)

	if err := syncRook(t, NewRookHook(c, logr.Discard())); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	cluster := getCephCluster(t, c)
	if mons, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "mon", "count"); mons != 3 {
		t.Errorf("mon.count %d, want 3 on 3 nodes", mons)
	}
	if image, _, _ := unstructured.NestedString(cluster.Object, "spec", "cephVersion", "image"); image != rookSpec.Image {
		t.Errorf("cephVersion.image %q, want the component's image %q", image, rookSpec.Image)
	}
	got := managers(cluster)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{ownedFieldManager, seedFieldManager}) {
		t.Errorf("field managers %v, want %q and, for the image, %q", got, seedFieldManager, ownedFieldManager)
	}
}

// ownedFields are the fields the managedFields entry of manager owns, as
// paths ("spec.cephVersion.image").
func ownedFields(t *testing.T, obj client.Object, manager string) []string {
	t.Helper()
	var out []string
	var walk func(prefix string, set map[string]interface{})
	walk = func(prefix string, set map[string]interface{}) {
		for key, sub := range set {
			if key == "." {
				continue
			}
			path := prefix + strings.TrimPrefix(key, "f:")
			children, _ := sub.(map[string]interface{})
			if len(children) == 0 {
				out = append(out, path)
				continue
			}
			walk(path+".", children)
		}
	}
	for _, e := range obj.GetManagedFields() {
		if e.Manager != manager || e.FieldsV1 == nil {
			continue
		}
		set := map[string]interface{}{}
		if err := json.Unmarshal(e.FieldsV1.Raw, &set); err != nil {
			t.Fatal(err)
		}
		walk("", set)
	}
	sort.Strings(out)
	return out
}

func cephImage(t *testing.T, c client.Client) string {
	t.Helper()
	image, _, _ := unstructured.NestedString(getCephCluster(t, c).Object, "spec", "cephVersion", "image")
	return image
}

// D18: of the CephCluster it created, k4all owns spec.cephVersion.image and
// nothing else, so Ceph follows the Rook chart the release pins. The
// "k4all-operator" entry covers that field alone (Cairn shows it
// read-only); Cairn's edits of the rest stay, an image changed by hand is
// put back, and a new image in the release reaches the cluster.
func TestRookHook_OwnsOnlyTheCephImage(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 3)
	forgetRook(t, c)
	h := NewRookHook(c, logr.Discard())
	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := ownedFields(t, getCephCluster(t, c), ownedFieldManager); !reflect.DeepEqual(got, []string{"spec.cephVersion.image"}) {
		t.Fatalf("%s owns %v, want only spec.cephVersion.image", ownedFieldManager, got)
	}

	// Cairn's edit (a JSON patch from Headlamp), and the image by hand.
	edit := `[{"op":"replace","path":"/spec/mgr/count","value":1}]`
	if err := c.Patch(context.Background(), getCephCluster(t, c), client.RawPatch(types.JSONPatchType, []byte(edit)),
		client.FieldOwner("Headlamp")); err != nil {
		t.Fatalf("Cairn's edit: %v", err)
	}
	byHand := `{"spec":{"cephVersion":{"image":"quay.io/ceph/ceph:v20.2.1"}}}`
	if err := c.Patch(context.Background(), getCephCluster(t, c), client.RawPatch(types.MergePatchType, []byte(byHand)),
		client.FieldOwner("kubectl-edit")); err != nil {
		t.Fatalf("image by hand: %v", err)
	}

	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync after the edits: %v", err)
	}
	cluster := getCephCluster(t, c)
	if mgrs, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "mgr", "count"); mgrs != 1 {
		t.Errorf("mgr.count %d, want Cairn's 1", mgrs)
	}
	if image := cephImage(t, c); image != rookSpec.Image {
		t.Errorf("cephVersion.image %q, want the release's %q back", image, rookSpec.Image)
	}

	newer := rookSpec
	newer.Image = "quay.io/ceph/ceph:v20.2.5"
	if _, err := h.Sync(context.Background(), newer, k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatalf("Sync with a new image: %v", err)
	}
	if image := cephImage(t, c); image != newer.Image {
		t.Errorf("cephVersion.image %q, want the new release's %q", image, newer.Image)
	}
	if mgrs, _, _ := unstructured.NestedInt64(getCephCluster(t, c).Object, "spec", "mgr", "count"); mgrs != 1 {
		t.Errorf("after the image change: mgr.count %d, want Cairn's 1", mgrs)
	}
}

// A CephCluster k4all found (made before k4all ran Rook: the lab's) is its
// admin's, image included: k4all could otherwise move it to an older Ceph.
func TestRookHook_LeavesTheImageOfAFoundCephCluster(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 3)
	forgetRook(t, c)
	byHand := cephClusterObject(rookPlan{Mons: 3, Mgrs: 2}, RookSettings{}, "quay.io/ceph/ceph:v20.2.9")
	if err := c.Create(context.Background(), byHand, client.FieldOwner("kubectl-client-side-apply")); err != nil {
		t.Fatal(err)
	}

	if err := syncRook(t, NewRookHook(c, logr.Discard())); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if image := cephImage(t, c); image != "quay.io/ceph/ceph:v20.2.9" {
		t.Errorf("cephVersion.image %q, want the admin's v20.2.9", image)
	}
	if got := managers(getCephCluster(t, c)); slices.Contains(got, ownedFieldManager) {
		t.Errorf("field managers %v: k4all wrote the admin's CephCluster", got)
	}
}

// The image apply never creates a CephCluster: one deleted between the read
// and the apply stays deleted (an apply of the image alone would create a
// cluster with Rook's defaults).
func TestRookHook_TheImageApplyCreatesNoCephCluster(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 3)
	forgetRook(t, c)
	h := NewRookHook(c, logr.Discard())
	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	live := getCephCluster(t, c)
	deleteObject(t, c, newObject(cephAPIVersion, cephClusterKind, rookNamespace, cephClusterName))

	if err := h.applyCephImage(context.Background(), live, "quay.io/ceph/ceph:v20.2.5"); err == nil || !apierrors.IsConflict(err) {
		t.Errorf("the image apply of a deleted CephCluster: %v, want a conflict (its uid)", err)
	}
	isGone(t, c, newObject(cephAPIVersion, cephClusterKind, rookNamespace, cephClusterName))
}

// Create-only: the mgr and mon counts set from Cairn after k4all created the
// CephCluster stay as they are, even when the nodes now ask for others.
func TestRookHook_KeepsTheCephClusterAsEdited(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 3)
	forgetRook(t, c)
	h := NewRookHook(c, logr.Discard())
	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Cairn's change: a JSON patch from Headlamp.
	edit := []map[string]interface{}{
		{"op": "replace", "path": "/spec/mgr/count", "value": 1},
		{"op": "add", "path": "/spec/mgr/modules", "value": []interface{}{
			map[string]interface{}{"name": "pg_autoscaler", "enabled": true}}},
	}
	raw, _ := json.Marshal(edit)
	if err := c.Patch(context.Background(), getCephCluster(t, c), client.RawPatch(types.JSONPatchType, raw),
		client.FieldOwner("Headlamp")); err != nil {
		t.Fatalf("edit the CephCluster: %v", err)
	}
	edited := getCephCluster(t, c)
	rookNodes(t, c, 2) // 5 nodes now: k4all's default would be 5 mons

	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync after the edit: %v", err)
	}

	after := getCephCluster(t, c)
	if after.GetResourceVersion() != edited.GetResourceVersion() {
		t.Errorf("the CephCluster was written again (resourceVersion %s -> %s)",
			edited.GetResourceVersion(), after.GetResourceVersion())
	}
	if mgrs, _, _ := unstructured.NestedInt64(after.Object, "spec", "mgr", "count"); mgrs != 1 {
		t.Errorf("mgr.count %d, want the edited 1", mgrs)
	}
	if mons, _, _ := unstructured.NestedInt64(after.Object, "spec", "mon", "count"); mons != 3 {
		t.Errorf("mon.count %d, want 3 as created", mons)
	}
}

// With the default small-cluster policy, 2 nodes get no Ceph cluster: Sync
// waits for a third node and says so, and the component's status shows it
// as waiting, not as a hook failure.
func TestRookHook_CreatesNothingBelowThreeNodes(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 2)
	forgetRook(t, c)

	err := syncRook(t, NewRookHook(c, logr.Discard()))

	var waiting Waiting
	if !errors.As(err, &waiting) || !strings.Contains(waiting.Reason, "this cluster has 2") {
		t.Fatalf("Sync = %v, want it waiting for a third node", err)
	}
	cluster := newObject(cephAPIVersion, cephClusterKind, rookNamespace, cephClusterName)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cluster), cluster); !apierrors.IsNotFound(err) {
		t.Fatalf("get CephCluster = %v, want NotFound", err)
	}
}

// smallCluster Allow in the ClusterConfig (D5): on 2 nodes k4all creates
// the CephCluster with 1 mon and the OSDs of the ClusterConfig's filter.
func TestRookHook_ReadsTheClusterConfig(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 2)
	forgetRook(t, c)
	config := k4allv1alpha1.ClusterConfigSpec{Features: k4allv1alpha1.FeaturesConfig{Rook: &k4allv1alpha1.RookConfig{
		Enabled:      true,
		SmallCluster: k4allv1alpha1.RookSmallClusterAllow,
		Storage:      &k4allv1alpha1.RookStorage{DeviceFilter: "^sdb$"},
	}}}

	if _, err := NewRookHook(c, logr.Discard()).Sync(context.Background(), rookSpec, config); err != nil {
		t.Fatalf("Sync with smallCluster Allow on 2 nodes: %v", err)
	}

	cluster := getCephCluster(t, c)
	for path, want := range map[string]interface{}{
		"spec.mon.count":             int64(1),
		"spec.storage.useAllDevices": false,
		"spec.storage.deviceFilter":  "^sdb$",
	} {
		if got := fieldAt(t, cluster.Object, path); got != want {
			t.Errorf("%s = %#v, want %#v", path, got, want)
		}
	}
}

// What VMs need on Ceph: vm-pool with 3 replicas, the shared (RWX Block)
// StorageClass ceph-rbd for live migration, its VolumeSnapshotClass, and
// CDI's StorageProfile, all as seeds.
func TestRookHook_CreatesThePoolAndClassesForVMs(t *testing.T) {
	c := rookEnvtest(t)
	withOptionalAPIs(t, c)
	rookNodes(t, c, 3)
	forgetRook(t, c)

	if err := syncRook(t, NewRookHook(c, logr.Discard())); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	pool := getObject(t, c, cephAPIVersion, cephBlockPoolKind, rookNamespace, "vm-pool")
	isSeed(t, pool)
	if size, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicated", "size"); size != 3 {
		t.Errorf("vm-pool size %d, want 3", size)
	}
	if domain, _, _ := unstructured.NestedString(pool.Object, "spec", "failureDomain"); domain != "host" {
		t.Errorf("vm-pool failureDomain %q, want host", domain)
	}

	sc := &storagev1.StorageClass{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ceph-rbd"}, sc); err != nil {
		t.Fatalf("get StorageClass ceph-rbd: %v", err)
	}
	isSeed(t, sc)
	for key, want := range map[string]string{
		"pool":          "vm-pool",
		"clusterID":     "rook-ceph",
		"imageFeatures": "layering,deep-flatten,exclusive-lock,object-map,fast-diff",
		"mapOptions":    "krbd:rxbounce",
	} {
		if got := sc.Parameters[key]; got != want {
			t.Errorf("ceph-rbd parameter %s = %q, want %q", key, got, want)
		}
	}
	if sc.Provisioner != "rook-ceph.rbd.csi.ceph.com" {
		t.Errorf("ceph-rbd provisioner %q", sc.Provisioner)
	}

	snapshots := getObject(t, c, "snapshot.storage.k8s.io/v1", "VolumeSnapshotClass", "", "ceph-rbd-snapshot")
	isSeed(t, snapshots)
	if driver, _, _ := unstructured.NestedString(snapshots.Object, "driver"); driver != "rook-ceph.rbd.csi.ceph.com" {
		t.Errorf("VolumeSnapshotClass driver %q", driver)
	}

	profile := getObject(t, c, "cdi.kubevirt.io/v1beta1", "StorageProfile", "", "ceph-rbd")
	isSeed(t, profile)
	// VM disks default to the first set, shared block volumes (live
	// migration). KubeVirt creates the persistent VM state (EFI, TPM) on
	// vmStateStorageClass as a Filesystem volume and picks its access mode
	// from the profile's Filesystem sets: without one, with ceph-rbd as
	// vmStateStorageClass, those VMs would never start. RBD has no RWX
	// Filesystem; on RWO KubeVirt copies the state when the VM migrates.
	sets, _, _ := unstructured.NestedSlice(profile.Object, "spec", "claimPropertySets")
	want := []interface{}{
		map[string]interface{}{"accessModes": []interface{}{"ReadWriteMany"}, "volumeMode": "Block"},
		map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}, "volumeMode": "Filesystem"},
	}
	if !reflect.DeepEqual(sets, want) {
		t.Errorf("StorageProfile claimPropertySets %v, want %v", sets, want)
	}
}

// Without CDI or the snapshot CRDs (virtualization off, no snapshot
// controller), the pool and the StorageClass are still created.
func TestRookHook_SkipsTheAPIsTheClusterLacks(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 3)
	forgetRook(t, c)

	if err := syncRook(t, NewRookHook(c, logr.Discard())); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	getObject(t, c, cephAPIVersion, cephBlockPoolKind, rookNamespace, "vm-pool")
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ceph-rbd"}, &storagev1.StorageClass{}); err != nil {
		t.Fatalf("get StorageClass ceph-rbd: %v", err)
	}
}

// deleteObject deletes an object as an admin would.
func deleteObject(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Delete(context.Background(), obj); err != nil {
		t.Fatalf("delete %s: %v", obj.GetName(), err)
	}
}

// isGone fails the test when the object exists.
func isGone(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
		t.Errorf("%s %s: get = %v, want NotFound", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
	}
}

// k4all creates each object once. One an admin deleted stays deleted: a
// CephCluster deleted to turn Rook off must not come back before the
// feature is off, nor a pool removed from Cairn.
func TestRookHook_DoesNotCreateWhatAnAdminDeleted(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 3)
	forgetRook(t, c)
	h := NewRookHook(c, logr.Discard())
	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	cluster := newObject(cephAPIVersion, cephClusterKind, rookNamespace, cephClusterName)
	pool := newObject(cephAPIVersion, cephBlockPoolKind, rookNamespace, cephPoolName)
	deleteObject(t, c, cluster)
	deleteObject(t, c, pool)

	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync after the deletes: %v", err)
	}

	isGone(t, c, cluster)
	isGone(t, c, pool)
}

// Objects that were there before k4all managed Rook (the lab's, made with
// kubectl) are the admin's: k4all does not create them again either.
func TestRookHook_DoesNotCreateWhatWasThereBefore(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 3)
	forgetRook(t, c)
	byHand := cephBlockPoolObject(rookPlan{PoolSize: 2})
	if err := c.Create(context.Background(), byHand, client.FieldOwner("kubectl-client-side-apply")); err != nil {
		t.Fatal(err)
	}
	h := NewRookHook(c, logr.Discard())
	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	pool := getObject(t, c, cephAPIVersion, cephBlockPoolKind, rookNamespace, cephPoolName)
	if size, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicated", "size"); size != 2 {
		t.Errorf("vm-pool size %d, want the 2 it was made with", size)
	}
	deleteObject(t, c, pool)

	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync after the delete: %v", err)
	}

	isGone(t, c, newObject(cephAPIVersion, cephBlockPoolKind, rookNamespace, cephPoolName))
}

// Uninstalling rook-ceph (its feature turned off, after the admin deleted
// the CephCluster) forgets what k4all created: turning Rook on again
// creates a new Ceph cluster.
func TestRookHook_CreatesAgainAfterAnUninstall(t *testing.T) {
	c := rookEnvtest(t)
	rookNodes(t, c, 3)
	forgetRook(t, c)
	h := NewRookHook(c, logr.Discard())
	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	deleteObject(t, c, newObject(cephAPIVersion, cephClusterKind, rookNamespace, cephClusterName))

	if err := h.Cleanup(context.Background(), "rook-ceph", k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if err := syncRook(t, h); err != nil {
		t.Fatalf("Sync after the reinstall: %v", err)
	}

	getCephCluster(t, c)
}

// Neither the Rook operator nor the CSI drivers may be uninstalled while a
// CephCluster exists, wherever it is: its data would be stranded, and its
// finalizers never run.
func TestRookHooks_RefuseTheUninstallWhileACephClusterExists(t *testing.T) {
	c := rookEnvtest(t)
	reg := NewRegistry(c, logr.Discard())
	byHand := cephClusterObject(rookPlan{Mons: 1, Mgrs: 1}, RookSettings{}, defaultCephImage)
	byHand.SetNamespace("default")
	if err := c.Create(context.Background(), byHand); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), byHand) })

	for _, name := range []string{"rook-ceph", "ceph-csi-drivers"} {
		guard, ok := reg.Get(name).(UninstallGuard)
		if !ok {
			t.Fatalf("the %s hook is not an UninstallGuard", name)
		}
		reason, err := guard.UninstallBlocked(context.Background(), name)
		if err != nil || !strings.Contains(reason, "CephCluster default/rook-ceph") {
			t.Errorf("%s: UninstallBlocked = %q, %v; want a refusal naming the CephCluster", name, reason, err)
		}
	}

	deleteObject(t, c, byHand)
	for _, name := range []string{"rook-ceph", "ceph-csi-drivers"} {
		reason, err := reg.Get(name).(UninstallGuard).UninstallBlocked(context.Background(), name)
		if reason != "" || err != nil {
			t.Errorf("%s without a CephCluster: UninstallBlocked = %q, %v; want it allowed", name, reason, err)
		}
	}
}
