package hooks

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gpillon/k4all-operator/internal/engine"
)

// TopolvmHook handles pre-install tasks for TopoLVM:
//   - Labels the topolvm-system and kube-system namespaces with topolvm.io/webhook=ignore
//   - Deletes the topolvm-provisioner StorageClass when the values change its
//     volumeBindingMode, for helm to create it again
//
// Mirrors the logic from setup-topolvm.sh.
type TopolvmHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
}

func NewTopolvmHook(c client.Client, log logr.Logger) *TopolvmHook {
	return &TopolvmHook{client: c, log: log}
}

func (h *TopolvmHook) PreInstall(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec, values map[string]interface{}) (map[string]interface{}, error) {
	ns := "topolvm-system"

	if err := h.recreateStorageClassIfModeChanges(ctx, name, spec, config); err != nil {
		return values, err
	}

	if err := h.ensureNamespace(ctx, ns); err != nil {
		return values, fmt.Errorf("ensure namespace %s: %w", ns, err)
	}

	if err := h.labelNamespace(ctx, ns); err != nil {
		return values, fmt.Errorf("label namespace %s: %w", ns, err)
	}

	if err := h.labelNamespace(ctx, "kube-system"); err != nil {
		h.log.V(1).Info("failed to label kube-system (non-fatal)", "error", err)
	}

	return values, nil
}

func (h *TopolvmHook) ensureNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"topolvm.io/webhook": "ignore",
				},
			},
		}
		return h.client.Create(ctx, ns)
	}
	return nil
}

func (h *TopolvmHook) labelNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
		return err
	}

	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}

	if ns.Labels["topolvm.io/webhook"] == "ignore" {
		return nil
	}

	ns.Labels["topolvm.io/webhook"] = "ignore"
	return h.client.Update(ctx, ns)
}

// topolvmStorageClass is the StorageClass the k4all values name.
const topolvmStorageClass = "topolvm-provisioner"

// recreateStorageClassIfModeChanges deletes the topolvm-provisioner
// StorageClass when the values about to be applied give it another
// volumeBindingMode, which is immutable: helm then creates it again. PVs and
// PVCs name their class, so they stay bound. Up to k4all v2.2.x the values
// left the mode out and the API server made the class Immediate; from
// v2.4.0 they ask WaitForFirstConsumer, which multi-node clusters need.
func (h *TopolvmHook) recreateStorageClassIfModeChanges(
	ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
) error {
	want, ok, err := topolvmBindingMode(spec, config.ComponentOverrides[name])
	if err != nil || !ok {
		return err
	}
	sc := &storagev1.StorageClass{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: topolvmStorageClass}, sc); err != nil {
		return client.IgnoreNotFound(err)
	}
	have := storagev1.VolumeBindingImmediate
	if sc.VolumeBindingMode != nil {
		have = *sc.VolumeBindingMode
	}
	if have == want {
		return nil
	}
	h.log.Info("deleting the StorageClass for helm to recreate it: volumeBindingMode is immutable",
		"storageClass", topolvmStorageClass, "from", have, "to", want)
	if err := h.client.Delete(ctx, sc); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete StorageClass %s: %w", topolvmStorageClass, err)
	}
	return nil
}

// topolvmBindingMode is the volumeBindingMode the values give the
// topolvm-provisioner class, as helm will see them: the ReleaseManifest
// values with the user's componentOverrides on top. Without a
// storageClasses list it is the chart's default, WaitForFirstConsumer;
// without the field, the API server's default, Immediate. ok is false when
// the list has no topolvm-provisioner class.
func topolvmBindingMode(spec k4allv1alpha1.ComponentSpec, override k4allv1alpha1.ComponentOverride) (storagev1.VolumeBindingMode, bool, error) {
	values := map[string]interface{}{}
	if spec.Values != nil && len(spec.Values.Raw) > 0 {
		if err := json.Unmarshal(spec.Values.Raw, &values); err != nil {
			return "", false, fmt.Errorf("topolvm values: %w", err)
		}
	}
	values, err := engine.ApplyOverride(values, override)
	if err != nil {
		return "", false, err
	}
	classes, found := values["storageClasses"].([]interface{})
	if !found {
		return storagev1.VolumeBindingWaitForFirstConsumer, true, nil
	}
	for _, c := range classes {
		entry, _ := c.(map[string]interface{})
		if entry["name"] != topolvmStorageClass {
			continue
		}
		class, _ := entry["storageClass"].(map[string]interface{})
		if mode, _ := class["volumeBindingMode"].(string); mode != "" {
			return storagev1.VolumeBindingMode(mode), true, nil
		}
		return storagev1.VolumeBindingImmediate, true, nil
	}
	return "", false, nil
}
