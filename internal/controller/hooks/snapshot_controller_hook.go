package hooks

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// SnapshotControllerHook guards and removes the snapshot-controller
// component: external-snapshotter's CRDs and the CSI snapshot controller,
// which k4all installs with virt or rook (D13).
type SnapshotControllerHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewSnapshotControllerHook(c client.Client, log logr.Logger) *SnapshotControllerHook {
	return &SnapshotControllerHook{client: c, log: log}
}

// snapshotKind is a kind of object that holds a snapshot of volumes, with
// the API versions to list it in, the newest first: an older cluster may
// serve only an older one.
type snapshotKind struct {
	kind     string
	group    string
	versions []string
}

// snapshotKinds are the objects whose finalizers the snapshot controller
// removes: a VolumeSnapshot (and the PVC it is taken from, while it is
// taken) or a VolumeGroupSnapshot left without it can neither be deleted
// nor restored.
var snapshotKinds = []snapshotKind{
	{kind: "VolumeSnapshot", group: "snapshot.storage.k8s.io", versions: []string{"v1"}},
	{kind: "VolumeGroupSnapshot", group: "groupsnapshot.storage.k8s.io", versions: []string{"v1", "v1beta2", "v1beta1"}},
}

// snapshotsNamed is how many snapshots a refusal names; the rest are counted.
const snapshotsNamed = 5

// UninstallBlocked refuses the uninstall of the snapshot controller (virt
// and rook both off, or management Removed) while a VolumeSnapshot or a
// VolumeGroupSnapshot exists, in any namespace (R8). Empty once none is
// left, or the cluster does not serve their APIs.
func (h *SnapshotControllerHook) UninstallBlocked(ctx context.Context, name string) (string, error) {
	var held []string
	for _, kind := range snapshotKinds {
		items, err := listServed(ctx, h.client, kind)
		if err != nil {
			return "", err
		}
		for _, item := range items {
			held = append(held, kind.kind+" "+item.GetNamespace()+"/"+item.GetName())
		}
	}
	if len(held) == 0 {
		return "", nil
	}
	named := held
	if len(held) > snapshotsNamed {
		named = append(held[:snapshotsNamed:snapshotsNamed], fmt.Sprintf("%d more", len(held)-snapshotsNamed))
	}
	return fmt.Sprintf("%s exist, and without %s they can neither be deleted nor restored: "+
		"delete them first (VM snapshots from Hangar), or turn virt or rook back on",
		strings.Join(named, ", "), name), nil
}

// listServed lists the objects of kind in the first of its versions the
// cluster serves; none when it serves none.
func listServed(ctx context.Context, c client.Client, kind snapshotKind) ([]unstructured.Unstructured, error) {
	for _, version := range kind.versions {
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion(kind.group + "/" + version)
		list.SetKind(kind.kind + "List")
		err := c.List(ctx, list)
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", kind.kind+"s", err)
		}
		return list.Items, nil
	}
	return nil, nil
}

// snapshotControllerObjects are the objects of the CSI snapshot controller
// as external-snapshotter publishes them (deploy/kubernetes/
// snapshot-controller, the same names since v4): its Deployment and RBAC.
var snapshotControllerObjects = []struct{ apiVersion, kind, namespace, name string }{
	{"apps/v1", "Deployment", "kube-system", "snapshot-controller"},
	{"rbac.authorization.k8s.io/v1", "RoleBinding", "kube-system", "snapshot-controller-leaderelection"},
	{"rbac.authorization.k8s.io/v1", "Role", "kube-system", "snapshot-controller-leaderelection"},
	{"rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "snapshot-controller-role"},
	{"rbac.authorization.k8s.io/v1", "ClusterRole", "", "snapshot-controller-runner"},
	{"v1", "ServiceAccount", "kube-system", "snapshot-controller"},
}

// Remove deletes the snapshot controller and keeps external-snapshotter's
// CRDs, as Helm keeps a chart's CRDs: deleting them would delete every
// VolumeSnapshotClass (the consoles', Rook's ceph-rbd-snapshot) and every
// VolumeSnapshotContent, some of which may hold snapshots kept on the
// storage (Retain), and the CRDs may have been there before k4all (R7).
// The reconciler calls it once UninstallBlocked lets the uninstall go on.
func (h *SnapshotControllerHook) Remove(ctx context.Context, _ k4allv1alpha1.ComponentSpec, _ k4allv1alpha1.ClusterConfigSpec) error {
	for _, o := range snapshotControllerObjects {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(o.apiVersion)
		obj.SetKind(o.kind)
		obj.SetNamespace(o.namespace)
		obj.SetName(o.name)
		if err := h.client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s %s: %w", o.kind, o.name, err)
		}
	}
	h.log.Info("snapshot controller removed; external-snapshotter's CRDs, classes and contents kept")
	return nil
}
