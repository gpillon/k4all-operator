package hooks

import (
	"context"
	"errors"
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
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// seedFieldManager is the field manager of the objects k4all creates and
// then leaves to their admins (create-only). Not "k4all-operator": the
// consoles show the fields of that manager read-only, as owned by k4all.
const seedFieldManager = "k4all-operator-seed"

// ownedFieldManager is the field manager of what k4all owns and puts back
// at every reconcile; the consoles show its fields read-only.
const ownedFieldManager = "k4all-operator"

// defaultCephImage is the Ceph image of a rook-ceph component without one.
const defaultCephImage = "quay.io/ceph/ceph:v20.2.4"

// RookHook creates the Ceph cluster of the rook-ceph component with k4all's
// defaults, once: the CephCluster, then left to its admins (Cairn, kubectl),
// so their day-2 changes stay. Of the CephCluster it created k4all keeps one
// field, the Ceph image (D18).
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
// CephCluster deleted before turning Rook off). The StorageProfile CDI
// creates empty for ceph-rbd gets the seed's spec once. The seed record in the
// rook-ceph namespace says which; Cleanup drops it. Then it puts the
// component's image in the CephCluster it created (keepCephImage). The
// reconciler runs Sync after every apply of rook-ceph and at every
// reconcile.
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
			how := seedFound
			if s.fillsEmptySpec {
				if how, err = h.fillEmptySpec(ctx, s.build(rookPlan{})); err != nil {
					return 0, err
				}
			}
			if err := h.recordSeed(ctx, record, key, how); err != nil {
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
		how := seedCreated
		err = h.client.Create(ctx, obj, client.FieldOwner(seedFieldManager))
		switch {
		case apierrors.IsAlreadyExists(err) && s.fillsEmptySpec:
			// Made in between: CDI's profile, as the StorageClass appeared.
			if how, err = h.fillEmptySpec(ctx, obj); err != nil {
				return 0, err
			}
		case err != nil && !apierrors.IsAlreadyExists(err):
			return 0, fmt.Errorf("create %s %s: %w", s.kind, obj.GetName(), err)
		}
		if err := h.recordSeed(ctx, record, key, how); err != nil {
			return 0, err
		}
	}
	return 0, h.keepCephImage(ctx, record, image)
}

// fillEmptySpec writes the spec of seed on the object of its name when that
// object's spec is empty, as the seed manager, and says "created"; it says
// "found", writing nothing, when the spec has anything in it. CDI creates
// the StorageProfile of a StorageClass at once, its spec empty: that is
// CDI's, not an admin's, and k4all's seed has not been written yet. The
// write is conditional on the object as read (its resourceVersion), so a
// spec set in between is never overwritten.
func (h *RookHook) fillEmptySpec(ctx context.Context, seed client.Object) (string, error) {
	desired, ok := seed.(*unstructured.Unstructured)
	if !ok {
		return "", fmt.Errorf("seed %s is %T, want unstructured", seed.GetName(), seed)
	}
	how := seedFound
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(desired.GroupVersionKind())
		if err := h.client.Get(ctx, client.ObjectKeyFromObject(desired), live); err != nil {
			return err
		}
		if spec, _, _ := unstructured.NestedMap(live.Object, "spec"); len(spec) > 0 {
			return nil
		}
		filled := live.DeepCopy()
		filled.Object["spec"] = desired.Object["spec"]
		if err := h.client.Patch(ctx, filled, client.MergeFromWithOptions(live, client.MergeFromWithOptimisticLock{}),
			client.FieldOwner(seedFieldManager)); err != nil {
			return err
		}
		h.log.Info("writing the seed spec on the empty one", "kind", desired.GetKind(), "name", desired.GetName())
		how = seedCreated
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("seed the spec of %s %s: %w", desired.GetKind(), desired.GetName(), err)
	}
	return how, nil
}

// keepCephImage keeps spec.cephVersion.image of the CephCluster k4all
// created at the component's image, so Ceph moves with the Rook chart the
// release pins: create-only, Ceph would never be upgraded (D18). It is the
// only field k4all owns there (server-side apply of that field alone, as
// k4all-operator: Cairn shows it read-only); every other one stays the
// console's. A CephCluster k4all found is its admin's, image included, and
// one deleted is not created again.
func (h *RookHook) keepCephImage(ctx context.Context, record *corev1.ConfigMap, image string) error {
	if !strings.HasPrefix(record.Data[seedKey(cephClusterKind, cephClusterName)], seedCreated) {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		live := &unstructured.Unstructured{}
		live.SetAPIVersion(cephAPIVersion)
		live.SetKind(cephClusterKind)
		err := h.client.Get(ctx, types.NamespacedName{Namespace: rookNamespace, Name: cephClusterName}, live)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get CephCluster %s/%s: %w", rookNamespace, cephClusterName, err)
		}
		return h.applyCephImage(ctx, live, image)
	})
}

// applyCephImage applies image as spec.cephVersion.image of live, on the
// condition that the cluster still has that object (its uid: the API server
// answers Conflict otherwise). An apply would otherwise create a CephCluster
// deleted in between, with the image alone and Rook's defaults; a
// resourceVersion does not stop that. Unchanged, it writes nothing.
func (h *RookHook) applyCephImage(ctx context.Context, live *unstructured.Unstructured, image string) error {
	apply := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": cephAPIVersion,
		"kind":       cephClusterKind,
		"metadata": map[string]interface{}{
			"name":      live.GetName(),
			"namespace": live.GetNamespace(),
			"uid":       string(live.GetUID()),
		},
		"spec": map[string]interface{}{
			"cephVersion": map[string]interface{}{"image": image},
		},
	}}
	if err := h.client.Patch(ctx, apply, client.Apply,
		client.FieldOwner(ownedFieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply the Ceph image %s to CephCluster %s/%s: %w",
			image, live.GetNamespace(), live.GetName(), err)
	}
	return nil
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
// or "found" and the time. "created" also stands for CDI's StorageProfile
// that the hook found with an empty spec and gave its seed's
// (fillEmptySpec). Deleting an entry lets k4all create that object again.
const seedRecordName = "k4all-rook-seeds"

// What the seed record says of an object: k4all created it (or gave CDI's
// empty StorageProfile its spec), or found it there.
const (
	seedCreated = "created"
	seedFound   = "found"
)

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
	// fillsEmptySpec: found with an empty spec, the object gets the seed's
	// (fillEmptySpec), and counts as created.
	fillsEmptySpec bool
	build          func(rookPlan) client.Object
}

// rookSeeds are the objects the Rook hook creates, in order.
func rookSeeds(settings RookSettings, image string) []rookSeed {
	return []rookSeed{
		{kind: cephClusterKind, build: func(p rookPlan) client.Object { return cephClusterObject(p, settings, image) }},
		{kind: cephBlockPoolKind, build: func(p rookPlan) client.Object { return cephBlockPoolObject(p) }},
		{kind: "StorageClass", build: func(p rookPlan) client.Object { return cephStorageClass(p) }},
		{kind: snapshotClassKind, optional: true, build: func(rookPlan) client.Object { return cephSnapshotClass() }},
		{kind: storageProfileKind, optional: true, fillsEmptySpec: true,
			build: func(rookPlan) client.Object { return cephStorageProfile() }},
	}
}

// plan is the Ceph plan for the nodes the cluster has now. A refusal
// (too few nodes) is Waiting: the CephCluster comes once the nodes do.
func (h *RookHook) plan(ctx context.Context, settings RookSettings) (rookPlan, error) {
	nodes := &corev1.NodeList{}
	if err := h.client.List(ctx, nodes); err != nil {
		return rookPlan{}, fmt.Errorf("list nodes: %w", err)
	}
	plan, err := planRook(cephNodes(nodes.Items), settings)
	var refusal rookRefusal
	if errors.As(err, &refusal) {
		return rookPlan{}, Waiting{Reason: refusal.reason}
	}
	return plan, err
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
		if kindNotServed(err) {
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
