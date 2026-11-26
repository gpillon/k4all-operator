package hooks

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gpillon/k4all-operator/internal/engine"
)

// The KubeVirt settings of the ClusterConfig (features.virt.kubevirt, spec
// §7), folded into the KubeVirt CR at its apply (ManifestMutator).

// Annotations of the KubeVirt CR recording the gates k4all turned on and off
// at its last apply: comma-separated, sorted, absent when none. They are how
// the next apply tells k4all's entries from the ones Hangar or an admin set
// (both lists are atomic: managedFields name one manager for the whole list),
// and how consoles tell which entries are k4all's (spec R6, R9).
const (
	FeatureGatesAnnotation         = "k4all.magesgate.com/feature-gates"
	DisabledFeatureGatesAnnotation = "k4all.magesgate.com/disabled-feature-gates"
)

var (
	featureGatesPath         = []string{"spec", "configuration", "developerConfiguration", "featureGates"}
	disabledFeatureGatesPath = []string{"spec", "configuration", "developerConfiguration", "disabledFeatureGates"}
	vmStateStorageClassPath  = []string{"spec", "configuration", "vmStateStorageClass"}
)

// MutatesManifest implements ManifestMutator: the KubeVirt CR.
func (h *VirtHook) MutatesManifest(obj *unstructured.Unstructured) bool {
	gvk := obj.GroupVersionKind()
	return gvk.Group == "kubevirt.io" && gvk.Kind == kubeVirtKind
}

// MutateManifest implements ManifestMutator. It folds features.virt.kubevirt
// into the KubeVirt CR about to be applied; what the ClusterConfig leaves
// out, k4all does not own:
//   - featureGates: ((live − (ours before − on)) ∪ on ∪ upstream's) − off,
//     where "ours before" is FeatureGatesAnnotation. The gates Hangar or an
//     admin set stay; of the exclusive pair HotplugVolumes /
//     DeclarativeHotplugVolumes, the one in the CR wins (KubeVirtSettings
//     reports it). An operator that wrote no annotation (up to v0.5.7) leaves
//     none of its gates to remove: they were never owned;
//   - disabledFeatureGates, the same with on and off swapped, written only
//     where the CRD has it (servesDisabledFeatureGates);
//   - vmStateStorageClass, written when set. Left out otherwise, the apply
//     removes it if k4all had written it, and keeps a value set by others.
func (h *VirtHook) MutateManifest(
	_ context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
	desired, live *unstructured.Unstructured,
) error {
	var s k4allv1alpha1.KubeVirtSettings
	if config.Features.Virt.KubeVirt != nil {
		s = *config.Features.Virt.KubeVirt
	}
	if err := engine.MergeSharedList(desired, live, featureGatesPath, engine.ListOwnership{
		Added: annotatedGates(live, FeatureGatesAnnotation), On: s.FeatureGates, Off: s.DisabledFeatureGates,
	}); err != nil {
		return err
	}
	setGatesAnnotation(desired, FeatureGatesAnnotation, s.FeatureGates)
	if servesDisabledFeatureGates(spec.Version, live) {
		if err := engine.MergeSharedList(desired, live, disabledFeatureGatesPath, engine.ListOwnership{
			Added: annotatedGates(live, DisabledFeatureGatesAnnotation), On: s.DisabledFeatureGates, Off: s.FeatureGates,
		}); err != nil {
			return err
		}
		setGatesAnnotation(desired, DisabledFeatureGatesAnnotation, s.DisabledFeatureGates)
	}
	if s.VMStateStorageClass != "" {
		if err := unstructured.SetNestedField(desired.Object, s.VMStateStorageClass, vmStateStorageClassPath...); err != nil {
			return fmt.Errorf("KubeVirt vmStateStorageClass: %w", err)
		}
	}
	return nil
}

// servesDisabledFeatureGates tells whether the KubeVirt CRD of the version
// being applied has developerConfiguration.disabledFeatureGates: KubeVirt 1.8
// and later. The component's own kubevirt-operator.yaml, applied before its
// kubevirt-cr.yaml, brings the CRD, and its virt-operator reads the field:
// on the upgrade step to 1.8 the CRD serves it while 1.7 still runs, which
// the version KubeVirt reports would not tell. Only a version that is no
// semver falls back on that one; with neither known, the field is not
// written (an older CRD would drop it).
func servesDisabledFeatureGates(version string, live *unstructured.Unstructured) bool {
	v, err := semver.NewVersion(version)
	if err != nil && live != nil {
		observed, _, _ := unstructured.NestedString(live.Object, "status", "observedKubeVirtVersion")
		v, err = semver.NewVersion(observed)
	}
	if err != nil {
		return false
	}
	return v.Major() > 1 || (v.Major() == 1 && v.Minor() >= 8)
}

// annotatedGates reads the gates an annotation of the live CR lists.
func annotatedGates(live *unstructured.Unstructured, key string) []string {
	if live == nil {
		return nil
	}
	var gates []string
	for _, g := range strings.Split(live.GetAnnotations()[key], ",") {
		if g = strings.TrimSpace(g); g != "" {
			gates = append(gates, g)
		}
	}
	return gates
}

// setGatesAnnotation records gates in the annotation key of desired, sorted;
// none leaves the annotation out, and the apply removes it.
func setGatesAnnotation(desired *unstructured.Unstructured, key string, gates []string) {
	annotations := desired.GetAnnotations()
	if len(gates) == 0 {
		if _, ok := annotations[key]; ok {
			delete(annotations, key)
			desired.SetAnnotations(annotations)
		}
		return
	}
	sorted := slices.Clone(gates)
	slices.Sort(sorted)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[key] = strings.Join(slices.Compact(sorted), ",")
	desired.SetAnnotations(annotations)
}

// Reasons of the KubeVirtSettings condition.
const (
	kubeVirtSettingsValid = "Valid"
	// storageClassNotFound: vmStateStorageClass names no StorageClass. The
	// value is written anyway: the StorageClass may come later.
	storageClassNotFound = "StorageClassNotFound"
	// exclusiveGate: a gate the ClusterConfig turns on is kept out of the CR
	// by its exclusive partner, set there by someone else.
	exclusiveGate = "ExclusiveGate"
	// settingsCheckFailed: the StorageClass or the KubeVirt CR cannot be read.
	settingsCheckFailed = "CheckFailed"
)

// KubeVirtSettingsCondition reports features.virt.kubevirt as the
// KubeVirtSettings condition: False with what the KubeVirt CR does not get as
// set (a vmStateStorageClass naming no StorageClass, a gate kept out by its
// exclusive partner), True when nothing is wrong, Unknown when it cannot
// tell. The caller sets ObservedGeneration.
func KubeVirtSettingsCondition(ctx context.Context, c client.Client, s k4allv1alpha1.KubeVirtSettings) metav1.Condition {
	cond := metav1.Condition{Type: k4allv1alpha1.ConditionKubeVirtSettings}
	var reasons, problems []string
	if s.VMStateStorageClass != "" {
		err := c.Get(ctx, client.ObjectKey{Name: s.VMStateStorageClass}, &storagev1.StorageClass{})
		switch {
		case apierrors.IsNotFound(err):
			reasons = append(reasons, storageClassNotFound)
			problems = append(problems, fmt.Sprintf("vmStateStorageClass %q: no such StorageClass; "+
				"KubeVirt has it anyway, and VMs with persistent EFI or TPM state need it to start", s.VMStateStorageClass))
		case err != nil:
			cond.Status, cond.Reason = metav1.ConditionUnknown, settingsCheckFailed
			cond.Message = fmt.Sprintf("read StorageClass %s: %v", s.VMStateStorageClass, err)
			return cond
		}
	}
	kv, found, err := getOperatorCR(ctx, c, kubeVirtAPIVersion, kubeVirtKind, kubeVirtKey)
	if err != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionUnknown, settingsCheckFailed, err.Error()
		return cond
	}
	if found {
		live, _, _ := unstructured.NestedStringSlice(kv.Object, featureGatesPath...)
		for _, g := range s.FeatureGates {
			p, exclusive := engine.ExclusivePartner("kubevirt.io", kubeVirtKind, featureGatesPath, g)
			if exclusive && !slices.Contains(live, g) && slices.Contains(live, p) {
				reasons = append(reasons, exclusiveGate)
				problems = append(problems, fmt.Sprintf("%s is not turned on: the KubeVirt CR has %s, which turns it off; "+
					"remove %s there, or list it in disabledFeatureGates", g, p, p))
			}
		}
	}
	if len(problems) == 0 {
		cond.Status, cond.Reason = metav1.ConditionTrue, kubeVirtSettingsValid
		cond.Message = "the settings are valid"
		return cond
	}
	cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, reasons[0], strings.Join(problems, "; ")
	return cond
}
