package hooks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// seedFieldManager is the field manager of the objects k4all creates and
// then leaves to their admins (create-only). Not "k4all-operator": the
// consoles show the fields of that manager read-only, as owned by k4all.
const seedFieldManager = "k4all-operator-seed"

// defaultCephImage is the Ceph image of a rook-ceph component without one.
const defaultCephImage = "quay.io/ceph/ceph:v20.2.4"

// RookHook creates the Ceph cluster of the rook-ceph component with k4all's
// defaults, once: the CephCluster, then left to its admins (Cairn, kubectl).
// k4all never writes it again, so their day-2 changes stay.
type RookHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewRookHook(c client.Client, log logr.Logger) *RookHook {
	return &RookHook{client: c, log: log}
}

// Sync creates, once each, the CephCluster, vm-pool, the StorageClass
// ceph-rbd and, when the cluster has their APIs, the VolumeSnapshotClass of
// ceph-rbd and CDI's StorageProfile, with the defaults of the nodes the
// cluster has now. Once means: an object k4all created, or found there
// before it, is never created again, so an admin's delete stays (the
// CephCluster deleted before turning Rook off). The seed record in the
// rook-ceph namespace says which; Cleanup drops it. The reconciler runs Sync
// after every apply of rook-ceph and at every reconcile.
func (h *RookHook) Sync(ctx context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) (time.Duration, error) {
	settings := rookSettingsFrom(config)
	image := spec.Image
	if image == "" {
		image = defaultCephImage
	}
	record, err := h.seedRecord(ctx)
	if err != nil {
		return 0, err
	}
	var plan *rookPlan
	for _, s := range rookSeeds(settings, image) {
		obj := s.build(rookPlan{})
		key := seedKey(s.kind, obj.GetName())
		if _, done := record.Data[key]; done {
			continue
		}
		err := h.client.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		switch {
		case err == nil:
			if err := h.recordSeed(ctx, record, key, "found"); err != nil {
				return 0, err
			}
			continue
		case meta.IsNoMatchError(err) && s.optional:
			h.log.V(1).Info("API not served: not creating", "kind", s.kind)
			continue
		case !apierrors.IsNotFound(err):
			return 0, fmt.Errorf("get %s %s: %w", s.kind, obj.GetName(), err)
		}
		if plan == nil {
			p, err := h.plan(ctx, settings)
			if err != nil {
				return 0, err
			}
			plan = &p
		}
		obj = s.build(*plan)
		h.log.Info("creating", "kind", s.kind, "name", obj.GetName(), "mons", plan.Mons, "mgrs", plan.Mgrs,
			"poolSize", plan.PoolSize, "warning", plan.Warning)
		if err := h.client.Create(ctx, obj, client.FieldOwner(seedFieldManager)); err != nil &&
			!apierrors.IsAlreadyExists(err) {
			return 0, fmt.Errorf("create %s %s: %w", s.kind, obj.GetName(), err)
		}
		if err := h.recordSeed(ctx, record, key, "created"); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

// Cleanup forgets the seed record when rook-ceph is uninstalled, so turning
// Rook on again creates a new Ceph cluster. The reconciler uninstalls
// rook-ceph only once no CephCluster is left (UninstallBlocked).
func (h *RookHook) Cleanup(ctx context.Context, _ string, _ k4allv1alpha1.ClusterConfigSpec) error {
	record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: rookNamespace, Name: seedRecordName}}
	if err := h.client.Delete(ctx, record); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the seed record %s/%s: %w", rookNamespace, seedRecordName, err)
	}
	return nil
}

// seedRecordName is the ConfigMap, in the rook-ceph namespace, that lists
// the objects the Rook hook created or found: "<kind>.<name>" to "created"
// or "found" and the time. Deleting an entry lets k4all create that object
// again.
const seedRecordName = "k4all-rook-seeds"

func seedKey(kind, name string) string {
	return strings.ToLower(kind) + "." + name
}

// seedRecord reads the seed record; an empty one when there is none yet.
func (h *RookHook) seedRecord(ctx context.Context) (*corev1.ConfigMap, error) {
	record := &corev1.ConfigMap{}
	err := h.client.Get(ctx, types.NamespacedName{Namespace: rookNamespace, Name: seedRecordName}, record)
	if apierrors.IsNotFound(err) {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: rookNamespace,
			Name:      seedRecordName,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "k4all-operator"},
		}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get the seed record %s/%s: %w", rookNamespace, seedRecordName, err)
	}
	return record, nil
}

// recordSeed adds an object to the seed record, creating the record if
// needed, and updates record to what was written.
func (h *RookHook) recordSeed(ctx context.Context, record *corev1.ConfigMap, key, how string) error {
	value := how + " " + time.Now().UTC().Format(time.RFC3339)
	if record.ResourceVersion == "" {
		record.Data = map[string]string{key: value}
		if err := h.client.Create(ctx, record); err != nil {
			return fmt.Errorf("create the seed record: %w", err)
		}
		return nil
	}
	patch := client.MergeFrom(record.DeepCopy())
	if record.Data == nil {
		record.Data = map[string]string{}
	}
	record.Data[key] = value
	if err := h.client.Patch(ctx, record, patch); err != nil {
		return fmt.Errorf("record %s in the seed record: %w", key, err)
	}
	return nil
}

// rookSeed is an object the Rook hook creates once.
type rookSeed struct {
	kind string
	// optional: the API comes with another component (CDI, the snapshot
	// CRDs), and a cluster without it gets no such object.
	optional bool
	build    func(rookPlan) client.Object
}

// rookSeeds are the objects the Rook hook creates, in order.
func rookSeeds(settings RookSettings, image string) []rookSeed {
	return []rookSeed{
		{kind: cephClusterKind, build: func(p rookPlan) client.Object { return cephClusterObject(p, settings, image) }},
		{kind: cephBlockPoolKind, build: func(p rookPlan) client.Object { return cephBlockPoolObject(p) }},
		{kind: "StorageClass", build: func(p rookPlan) client.Object { return cephStorageClass(p) }},
		{kind: snapshotClassKind, optional: true, build: func(rookPlan) client.Object { return cephSnapshotClass() }},
		{kind: storageProfileKind, optional: true, build: func(rookPlan) client.Object { return cephStorageProfile() }},
	}
}

// plan is the Ceph plan for the nodes the cluster has now.
func (h *RookHook) plan(ctx context.Context, settings RookSettings) (rookPlan, error) {
	nodes := &corev1.NodeList{}
	if err := h.client.List(ctx, nodes); err != nil {
		return rookPlan{}, fmt.Errorf("list nodes: %w", err)
	}
	return planRook(cephNodes(nodes.Items), settings)
}

// UninstallBlocked refuses the uninstall of rook-ceph while a CephCluster
// exists: see cephClustersBlock.
func (h *RookHook) UninstallBlocked(ctx context.Context, name string) (string, error) {
	return cephClustersBlock(ctx, h.client, name)
}

// CephCSIDriversHook keeps the Ceph CSI drivers (ceph-csi-drivers) while a
// CephCluster exists: without them its volumes cannot be attached, and VMs
// on ceph-rbd cannot start.
type CephCSIDriversHook struct {
	BaseHook
	client client.Client
}

func NewCephCSIDriversHook(c client.Client) *CephCSIDriversHook {
	return &CephCSIDriversHook{client: c}
}

// UninstallBlocked refuses the uninstall of ceph-csi-drivers while a
// CephCluster exists: see cephClustersBlock.
func (h *CephCSIDriversHook) UninstallBlocked(ctx context.Context, name string) (string, error) {
	return cephClustersBlock(ctx, h.client, name)
}

// cephClustersBlock is why the component name may not be uninstalled: a
// CephCluster exists, in any namespace. Helm's uninstall of the Rook
// operator keeps Rook's CRDs (resource-policy keep): the Ceph daemons would
// keep running with nothing managing them, the CephCluster's finalizer would
// never be removed, and the data would be stranded. Empty once no
// CephCluster is left, or Rook's CRDs are not there.
func cephClustersBlock(ctx context.Context, c client.Client, name string) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(cephAPIVersion)
	list.SetKind(cephClusterKind + "List")
	if err := c.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) {
			return "", nil
		}
		return "", fmt.Errorf("list CephClusters: %w", err)
	}
	if len(list.Items) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(list.Items))
	for _, cluster := range list.Items {
		names = append(names, cluster.GetNamespace()+"/"+cluster.GetName())
	}
	return fmt.Sprintf("CephCluster %s exists, and uninstalling %s would strand its data: "+
		"turn Rook back on, or delete the CephCluster first (that destroys its data: see Rook's cleanup guide)",
		strings.Join(names, ", "), name), nil
}
