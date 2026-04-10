package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Engine orchestrates component installation using type-specific sub-engines.
type Engine struct {
	client     client.Client
	restConfig *rest.Config
	log        logr.Logger

	helm      *HelmInstaller
	manifest  *ManifestApplier
	binary    *BinaryInstaller
	staticPod *StaticPodManager
	patcher   *PatchApplier
	resolver  *VersionResolver
}

func New(c client.Client, restConfig *rest.Config, log logr.Logger, toolsImage string) *Engine {
	return &Engine{
		client:     c,
		restConfig: restConfig,
		log:        log,
		helm:       NewHelmInstaller(restConfig, log.WithName("helm")),
		manifest:   NewManifestApplier(c, log.WithName("manifest")),
		binary:     NewBinaryInstaller(c, log.WithName("binary"), toolsImage),
		staticPod:  NewStaticPodManager(c, log.WithName("static-pod"), toolsImage),
		patcher:    NewPatchApplier(c, log.WithName("patch")),
		resolver:   NewVersionResolver(log.WithName("resolver")),
	}
}

// ReconcileComponent installs or upgrades a single component.
// It resolves version templates, dispatches to the appropriate installer,
// and applies any post-install patches.
func (e *Engine) ReconcileComponent(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, extraValues map[string]interface{}) (*k4allv1alpha1.ComponentStatus, error) {
	log := e.log.WithValues("component", name, "type", spec.Type)
	st := &k4allv1alpha1.ComponentStatus{
		State:              k4allv1alpha1.ComponentStateInstalling,
		LastTransitionTime: metav1.Now(),
	}

	resolvedVersion, err := e.resolver.Resolve(ctx, spec)
	if err != nil {
		st.State = k4allv1alpha1.ComponentStateFailed
		st.Message = fmt.Sprintf("version resolution failed: %v", err)
		return st, err
	}
	spec = e.templateSpec(spec, resolvedVersion)
	log.Info("resolved component", "version", resolvedVersion)

	values, err := e.mergeValues(ctx, spec, extraValues)
	if err != nil {
		st.State = k4allv1alpha1.ComponentStateFailed
		st.Message = fmt.Sprintf("values merge failed: %v", err)
		return st, err
	}

	switch spec.Type {
	case k4allv1alpha1.ComponentTypeHelm, k4allv1alpha1.ComponentTypeHelmOCI:
		err = e.helm.Reconcile(ctx, name, spec, values)
	case k4allv1alpha1.ComponentTypeManifest:
		err = e.manifest.Apply(ctx, name, spec)
	case k4allv1alpha1.ComponentTypeBinary:
		err = e.binary.Install(ctx, name, spec)
	case k4allv1alpha1.ComponentTypeStaticPod:
		err = e.staticPod.Reconcile(ctx, name, spec)
	default:
		err = fmt.Errorf("unsupported component type: %s", spec.Type)
	}

	if err != nil {
		st.State = k4allv1alpha1.ComponentStateFailed
		st.Message = err.Error()
		return st, err
	}

	if len(spec.Patches) > 0 {
		if patchErr := e.patcher.ApplyAll(ctx, spec.Patches); patchErr != nil {
			st.State = k4allv1alpha1.ComponentStateFailed
			st.Message = fmt.Sprintf("patch failed: %v", patchErr)
			return st, patchErr
		}
	}

	st.State = k4allv1alpha1.ComponentStateInstalled
	st.InstalledVersion = resolvedVersion
	st.Message = "installed successfully"
	return st, nil
}

// UninstallComponent removes a previously installed component.
// It resolves version templates before dispatching so that manifest URLs
// containing {{ version }} are expanded correctly.
func (e *Engine) UninstallComponent(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, installedVersion string) error {
	version := installedVersion
	if version == "" {
		resolved, err := e.resolver.Resolve(ctx, spec)
		if err != nil {
			e.log.V(1).Info("version resolution failed during uninstall, using spec.Version as-is", "component", name, "error", err)
			version = spec.Version
		} else {
			version = resolved
		}
	}
	spec = e.templateSpec(spec, version)

	switch spec.Type {
	case k4allv1alpha1.ComponentTypeHelm, k4allv1alpha1.ComponentTypeHelmOCI:
		return e.helm.Uninstall(ctx, name, spec.Namespace)
	case k4allv1alpha1.ComponentTypeManifest:
		return e.manifest.Delete(ctx, name, spec)
	case k4allv1alpha1.ComponentTypeBinary:
		return nil // binaries left on disk; cleaned up on OS upgrade
	case k4allv1alpha1.ComponentTypeStaticPod:
		return e.staticPod.Remove(ctx, name, spec)
	default:
		return fmt.Errorf("unsupported component type for uninstall: %s", spec.Type)
	}
}

// IsInstalled checks whether a component is already installed at the given version.
func (e *Engine) IsInstalled(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, currentStatus *k4allv1alpha1.ComponentStatus) bool {
	if currentStatus == nil {
		return false
	}
	if currentStatus.State != k4allv1alpha1.ComponentStateInstalled {
		return false
	}
	resolvedVersion, err := e.resolver.Resolve(ctx, spec)
	if err != nil {
		return false
	}
	return currentStatus.InstalledVersion == resolvedVersion
}

func (e *Engine) templateSpec(spec k4allv1alpha1.ComponentSpec, version string) k4allv1alpha1.ComponentSpec {
	arch := goArchToK8s(runtime.GOARCH)
	replacer := strings.NewReplacer("{{ version }}", version, "{{ arch }}", arch)

	spec.Version = version
	spec.Source = replacer.Replace(spec.Source)
	spec.Image = replacer.Replace(spec.Image)
	for i, s := range spec.Sources {
		spec.Sources[i] = replacer.Replace(s)
	}
	return spec
}

func (e *Engine) mergeValues(ctx context.Context, spec k4allv1alpha1.ComponentSpec, extra map[string]interface{}) (map[string]interface{}, error) {
	merged := make(map[string]interface{})

	if spec.Values != nil && spec.Values.Raw != nil {
		if err := json.Unmarshal(spec.Values.Raw, &merged); err != nil {
			return nil, fmt.Errorf("unmarshal inline values: %w", err)
		}
	}

	for _, ref := range spec.ValuesFrom {
		refValues, err := e.loadValuesFrom(ctx, ref, spec.Namespace)
		if err != nil {
			return nil, fmt.Errorf("load values from %s/%s: %w", ref.Kind, ref.Name, err)
		}
		merged = mergeMaps(merged, refValues)
	}

	for _, arg := range spec.HelmArgs {
		if err := applySetArg(merged, arg); err != nil {
			e.log.V(1).Info("skipping unparseable helmArg", "arg", arg, "err", err)
		}
	}

	if extra != nil {
		merged = mergeMaps(merged, extra)
	}
	return merged, nil
}

func (e *Engine) loadValuesFrom(ctx context.Context, ref k4allv1alpha1.ValuesReference, defaultNs string) (map[string]interface{}, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = defaultNs
	}
	key := ref.ValuesKey
	if key == "" {
		key = "values.yaml"
	}

	switch ref.Kind {
	case "ConfigMap":
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion("v1")
		obj.SetKind("ConfigMap")
		if err := e.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, obj); err != nil {
			return nil, err
		}
		data, ok := obj.Object["data"].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("ConfigMap %s/%s has no data", ns, ref.Name)
		}
		raw, ok := data[key].(string)
		if !ok {
			return nil, fmt.Errorf("ConfigMap %s/%s missing key %s", ns, ref.Name, key)
		}
		values := make(map[string]interface{})
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return values, nil // try as-is if not JSON (could be YAML handled elsewhere)
		}
		return values, nil
	case "Secret":
		return nil, fmt.Errorf("Secret values loading not yet implemented")
	}
	return nil, fmt.Errorf("unsupported ValuesFrom kind: %s", ref.Kind)
}

func goArchToK8s(goarch string) string {
	switch goarch {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	default:
		return goarch
	}
}

func mergeMaps(base, overlay map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(base))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overlay {
		if baseMap, ok := result[k].(map[string]interface{}); ok {
			if overlayMap, ok := v.(map[string]interface{}); ok {
				result[k] = mergeMaps(baseMap, overlayMap)
				continue
			}
		}
		result[k] = v
	}
	return result
}

func applySetArg(values map[string]interface{}, arg string) error {
	arg = strings.TrimPrefix(arg, "--set ")
	arg = strings.TrimPrefix(arg, "--set=")
	parts := strings.SplitN(arg, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid --set arg: %s", arg)
	}
	setNestedValue(values, strings.Split(parts[0], "."), parts[1])
	return nil
}

func setNestedValue(m map[string]interface{}, keys []string, value interface{}) {
	for i, k := range keys {
		if i == len(keys)-1 {
			m[k] = value
			return
		}
		next, ok := m[k].(map[string]interface{})
		if !ok {
			next = make(map[string]interface{})
			m[k] = next
		}
		m = next
	}
}

// SetReadyCondition sets or updates the Ready condition on a ReleaseManifest.
func SetReadyCondition(rm *k4allv1alpha1.ReleaseManifest, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&rm.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		ObservedGeneration: rm.Generation,
		Reason:             reason,
		Message:            message,
	})
}
