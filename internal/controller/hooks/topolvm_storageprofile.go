package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CDI keeps its settings for a StorageClass in the StorageProfile of the same
// name: the spec is the admin's (cloneStrategy, claimPropertySets,
// snapshotClass, dataImportCronSourceFormat), the status is CDI's. CDI
// deletes the profile once its class is gone, and creates it empty when the
// class comes back (CDI v1.64.0, storageprofile-controller.go). So the hook
// saves the spec before it deletes topolvm-provisioner, and Sync puts it back
// on the profile CDI creates for the new class (gpillon/k4all#80).
const (
	storageProfileAPIVersion, storageProfileKind = "cdi.kubevirt.io/v1beta1", "StorageProfile"

	// savedProfileConfigMap keeps the spec, in the TopoLVM namespace, from
	// before the delete until the restore: an operator restart in between
	// does not lose it.
	savedProfileConfigMap = "topolvm-storageprofile-saved"
	savedSpecKey          = "spec"
	savedUIDKey           = "storageProfileUID"
	savedAtKey            = "savedAt"

	// profileRecheckInterval is how soon Sync runs again while CDI has not
	// created the new profile yet, seconds after helm created the class.
	profileRecheckInterval = 10 * time.Second
	// profileWaitWindow is how long after the save Sync keeps coming back
	// every profileRecheckInterval: after it (CDI down), the periodic
	// reconcile is enough, and the saved spec stays until the profile is
	// there.
	profileWaitWindow = 10 * time.Minute
	// profileKeptSettle is how long the new class must exist before a profile
	// CDI has not replaced counts as kept: helm may create the class again
	// before CDI sees it gone, and CDI then keeps the profile and its spec.
	profileKeptSettle = time.Minute
)

// savedStorageProfile is the spec saved before the recreate, and the profile
// it came from.
type savedStorageProfile struct {
	spec map[string]interface{}
	uid  types.UID
	at   time.Time
}

// saveStorageProfile saves the spec of the class's StorageProfile before the
// class is deleted. Nothing is saved without CDI, without a profile, or when
// its spec is empty. A spec saved by an earlier recreate that has not been
// put back yet stays: the profile there now is CDI's empty one.
func (h *TopolvmHook) saveStorageProfile(ctx context.Context) error {
	profile, found, err := getOperatorCR(ctx, h.client, storageProfileAPIVersion, storageProfileKind,
		types.NamespacedName{Name: topolvmStorageClass})
	if err != nil || !found {
		return err
	}
	spec, _, _ := unstructured.NestedMap(profile.Object, "spec")
	if len(spec) == 0 {
		return nil
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("StorageProfile %s spec: %w", topolvmStorageClass, err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: topolvmNamespace,
			Name:      savedProfileConfigMap,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "k4all-operator"},
		},
		Data: map[string]string{
			savedSpecKey: string(raw),
			savedUIDKey:  string(profile.GetUID()),
			savedAtKey:   h.now().UTC().Format(time.RFC3339),
		},
	}
	if err := h.client.Create(ctx, cm); err != nil {
		if errors.IsAlreadyExists(err) {
			h.log.Info("a StorageProfile spec saved by an earlier recreate is not back yet: keeping it",
				"storageProfile", topolvmStorageClass, "configMap", topolvmNamespace+"/"+savedProfileConfigMap)
			return nil
		}
		return fmt.Errorf("save StorageProfile %s spec: %w", topolvmStorageClass, err)
	}
	h.log.Info("saved the StorageProfile spec: CDI empties it when the StorageClass is recreated",
		"storageProfile", topolvmStorageClass, "configMap", topolvmNamespace+"/"+savedProfileConfigMap)
	return nil
}

// Sync puts back the StorageProfile spec saved before the StorageClass was
// recreated, once CDI has created the profile for the new class, and then
// drops the saved copy. The reconciler runs it after every install of TopoLVM
// and on every reconcile; without a saved spec it does nothing. While the
// profile is not there yet it asks to run again soon, for profileWaitWindow.
// A profile CDI did not replace still has its spec: once the new class has
// existed for profileKeptSettle, the saved copy is dropped untouched.
func (h *TopolvmHook) Sync(ctx context.Context, _ k4allv1alpha1.ComponentSpec, _ k4allv1alpha1.ClusterConfigSpec) (time.Duration, error) {
	saved, found, err := h.savedStorageProfile(ctx)
	if err != nil || !found {
		return 0, err
	}
	now := h.now()
	wait := time.Duration(0)
	if now.Sub(saved.at) < profileWaitWindow {
		wait = profileRecheckInterval
	}

	// Read from the API server, not a cache: an old class seen here would
	// pass for one CDI kept.
	sc := &unstructured.Unstructured{}
	sc.SetAPIVersion("storage.k8s.io/v1")
	sc.SetKind("StorageClass")
	if err := h.client.Get(ctx, types.NamespacedName{Name: topolvmStorageClass}, sc); err != nil {
		if errors.IsNotFound(err) {
			h.log.V(1).Info("StorageProfile spec saved: waiting for helm to create the StorageClass",
				"storageClass", topolvmStorageClass)
			return wait, nil
		}
		return wait, fmt.Errorf("get StorageClass %s: %w", topolvmStorageClass, err)
	}
	profile, found, err := getOperatorCR(ctx, h.client, storageProfileAPIVersion, storageProfileKind,
		types.NamespacedName{Name: topolvmStorageClass})
	if err != nil {
		return wait, err
	}
	if !found || profile.GetDeletionTimestamp() != nil {
		h.log.V(1).Info("StorageProfile spec saved: waiting for CDI to create the StorageProfile",
			"storageProfile", topolvmStorageClass)
		return wait, nil
	}

	if profile.GetUID() == saved.uid {
		if age := now.Sub(sc.GetCreationTimestamp().Time); age < profileKeptSettle {
			h.log.V(1).Info("StorageProfile spec saved: waiting to see whether CDI replaces the StorageProfile",
				"storageProfile", topolvmStorageClass, "classAge", age)
			return profileRecheckInterval, nil
		}
		h.log.Info("CDI kept the StorageProfile and its spec: dropping the saved copy",
			"storageProfile", topolvmStorageClass)
		return 0, h.dropSavedStorageProfile(ctx)
	}

	current, _, _ := unstructured.NestedMap(profile.Object, "spec")
	if !specHolds(current, saved.spec) {
		patch, err := json.Marshal(map[string]interface{}{"spec": saved.spec})
		if err != nil {
			return wait, err
		}
		if err := h.client.Patch(ctx, profile, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return wait, fmt.Errorf("restore StorageProfile %s spec: %w", topolvmStorageClass, err)
		}
		h.log.Info("restored the StorageProfile spec CDI emptied when the StorageClass was recreated",
			"storageProfile", topolvmStorageClass, "spec", string(patch))
	}
	return 0, h.dropSavedStorageProfile(ctx)
}

// savedStorageProfile reads the saved spec. The ConfigMap is read as
// unstructured, straight from the API server: a typed read would make the
// manager cache every ConfigMap of the cluster.
func (h *TopolvmHook) savedStorageProfile(ctx context.Context) (savedStorageProfile, bool, error) {
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	key := types.NamespacedName{Namespace: topolvmNamespace, Name: savedProfileConfigMap}
	if err := h.client.Get(ctx, key, cm); err != nil {
		if errors.IsNotFound(err) {
			return savedStorageProfile{}, false, nil
		}
		return savedStorageProfile{}, false, fmt.Errorf("get ConfigMap %s: %w", key, err)
	}
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	saved := savedStorageProfile{uid: types.UID(data[savedUIDKey])}
	if err := json.Unmarshal([]byte(data[savedSpecKey]), &saved.spec); err != nil {
		return saved, false, fmt.Errorf("ConfigMap %s: %s: %w", key, savedSpecKey, err)
	}
	at, err := time.Parse(time.RFC3339, data[savedAtKey])
	if err != nil {
		return saved, false, fmt.Errorf("ConfigMap %s: %s: %w", key, savedAtKey, err)
	}
	saved.at = at
	return saved, true, nil
}

func (h *TopolvmHook) dropSavedStorageProfile(ctx context.Context) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: topolvmNamespace, Name: savedProfileConfigMap}}
	if err := h.client.Delete(ctx, cm); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete ConfigMap %s/%s: %w", topolvmNamespace, savedProfileConfigMap, err)
	}
	return nil
}

// specHolds tells whether current already has every saved field with the
// saved value. Both sides go through JSON: the saved spec was decoded from
// the ConfigMap, the current one from the API server.
func specHolds(current, saved map[string]interface{}) bool {
	for field, want := range saved {
		a, errA := json.Marshal(current[field])
		b, errB := json.Marshal(want)
		if errA != nil || errB != nil || !bytes.Equal(a, b) {
			return false
		}
	}
	return true
}
