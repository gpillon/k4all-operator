package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// snapshotCRDs are external-snapshotter v8.7.0's CRDs of the objects that
// hold snapshots of volumes, and the class (for the objects Remove keeps).
var snapshotCRDs = []string{
	filepath.Join("testdata", "crds", "snapshot.storage.k8s.io_volumesnapshots.yaml"),
	filepath.Join("testdata", "crds", "groupsnapshot.storage.k8s.io_volumegroupsnapshots.yaml"),
	filepath.Join("testdata", "crds", "snapshot.storage.k8s.io_volumesnapshotclasses.yaml"),
}

var snapshotCRDNames = []string{
	"volumesnapshots.snapshot.storage.k8s.io",
	"volumegroupsnapshots.groupsnapshot.storage.k8s.io",
	"volumesnapshotclasses.snapshot.storage.k8s.io",
}

// withSnapshotAPIs installs the snapshot CRDs for the test, and removes them
// when it ends, waiting until they are gone: the other tests of the package
// run without them.
func withSnapshotAPIs(t *testing.T) client.Client {
	t.Helper()
	c, cfg := envtestClient(t)
	opts := envtest.CRDInstallOptions{Paths: snapshotCRDs, ErrorIfPathMissing: true}
	if _, err := envtest.InstallCRDs(cfg, opts); err != nil {
		t.Fatalf("install the snapshot CRDs: %v", err)
	}
	t.Cleanup(func() {
		if err := envtest.UninstallCRDs(cfg, opts); err != nil {
			t.Errorf("uninstall the snapshot CRDs: %v", err)
		}
		for _, name := range snapshotCRDNames {
			crd := newObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", name)
			err := wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 30*time.Second, true,
				func(ctx context.Context) (bool, error) {
					return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(crd), crd)), nil
				})
			if err != nil {
				t.Errorf("CRD %s still there: %v", name, err)
			}
		}
	})
	return c
}

// volumeSnapshot creates a VolumeSnapshot of the PVC "disk" in namespace ns.
func volumeSnapshot(t *testing.T, c client.Client, ns, name string) *unstructured.Unstructured {
	t.Helper()
	ensureNamespaceHooks(t, c, ns)
	vs := newObject("snapshot.storage.k8s.io/v1", "VolumeSnapshot", ns, name)
	vs.Object["spec"] = map[string]interface{}{
		"source": map[string]interface{}{"persistentVolumeClaimName": "disk"},
	}
	if err := c.Create(context.Background(), vs); err != nil {
		t.Fatalf("create VolumeSnapshot %s/%s: %v", ns, name, err)
	}
	return vs
}

func ensureNamespaceHooks(t *testing.T, c client.Client, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

// D13, R8: the CSI snapshot controller removes the finalizers of
// VolumeSnapshots (and of the PVCs they are taken from). Uninstalled while
// one exists, that snapshot can neither be deleted nor restored: virt and
// rook both off is refused until the snapshots are gone.
func TestSnapshotControllerHook_RefusesTheUninstallWhileSnapshotsExist(t *testing.T) {
	c := withSnapshotAPIs(t)
	h := NewSnapshotControllerHook(c, logr.Discard())

	vs := volumeSnapshot(t, c, "vms", "rootdisk-snap")
	reason, err := h.UninstallBlocked(context.Background(), "snapshot-controller")
	if err != nil {
		t.Fatalf("UninstallBlocked: %v", err)
	}
	for _, want := range []string{"VolumeSnapshot vms/rootdisk-snap", "snapshot-controller"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q lacks %q", reason, want)
		}
	}

	if err := c.Delete(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	group := newObject("groupsnapshot.storage.k8s.io/v1", "VolumeGroupSnapshot", "vms", "all-disks")
	group.Object["spec"] = map[string]interface{}{
		"source": map[string]interface{}{"selector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"vm": "web"}}},
	}
	if err := c.Create(context.Background(), group); err != nil {
		t.Fatalf("create VolumeGroupSnapshot: %v", err)
	}
	if reason, err := h.UninstallBlocked(context.Background(), "snapshot-controller"); err != nil ||
		!strings.Contains(reason, "VolumeGroupSnapshot vms/all-disks") {
		t.Errorf("with a VolumeGroupSnapshot: %q, %v; want it named", reason, err)
	}

	if err := c.Delete(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	if reason, err := h.UninstallBlocked(context.Background(), "snapshot-controller"); reason != "" || err != nil {
		t.Errorf("no snapshot left: %q, %v; want the uninstall to go on", reason, err)
	}
}

// Without the snapshot CRDs there is no snapshot to strand.
func TestSnapshotControllerHook_UninstallsWithoutItsCRDs(t *testing.T) {
	c, _ := envtestClient(t)
	h := NewSnapshotControllerHook(c, logr.Discard())

	if reason, err := h.UninstallBlocked(context.Background(), "snapshot-controller"); reason != "" || err != nil {
		t.Errorf("UninstallBlocked = %q, %v; want the uninstall to go on", reason, err)
	}
}

// upstreamSnapshotController reads the snapshot controller's objects as
// external-snapshotter v8.7.0 publishes them (deploy/kubernetes).
func upstreamSnapshotController(t *testing.T) []unstructured.Unstructured {
	t.Helper()
	var out []unstructured.Unstructured
	dir := filepath.Join("testdata", "upstream", "external-snapshotter", "v8.7.0")
	for _, file := range []string{"rbac-snapshot-controller.yaml", "setup-snapshot-controller.yaml"} {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range strings.Split(string(raw), "\n---") {
			obj := map[string]interface{}{}
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			if len(obj) > 0 {
				out = append(out, unstructured.Unstructured{Object: obj})
			}
		}
	}
	return out
}

// Remove deletes the controller (its Deployment and RBAC, every object of
// upstream's deploy files) and keeps the CRDs, as Helm keeps a chart's
// CRDs: deleting them would delete every VolumeSnapshotClass (the
// consoles', Rook's ceph-rbd-snapshot) and VolumeSnapshotContent, some of
// which may point at snapshots kept on the storage (Retain), and CRDs that
// were there before k4all (R7).
func TestSnapshotControllerHook_RemoveKeepsTheCRDs(t *testing.T) {
	c := withSnapshotAPIs(t)
	ctx := context.Background()
	upstream := upstreamSnapshotController(t)
	for i := range upstream {
		obj := &upstream[i]
		if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	class := newObject("snapshot.storage.k8s.io/v1", "VolumeSnapshotClass", "", "topolvm-snapshot")
	class.Object["driver"] = "topolvm.io"
	class.Object["deletionPolicy"] = "Delete"
	if err := c.Create(ctx, class); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), class) })

	if err := NewSnapshotControllerHook(c, logr.Discard()).Remove(ctx, k4allv1alpha1.ComponentSpec{},
		k4allv1alpha1.ClusterConfigSpec{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	for i := range upstream {
		obj := upstream[i].DeepCopy()
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Errorf("%s %s/%s left: %v", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
	}
	for _, name := range snapshotCRDNames {
		crd := newObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", name)
		if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil || crd.GetDeletionTimestamp() != nil {
			t.Errorf("CRD %s: %v, want it kept", name, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(class), class); err != nil {
		t.Errorf("VolumeSnapshotClass %s: %v, want it kept", class.GetName(), err)
	}
}
