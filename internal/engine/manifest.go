package engine

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManifestApplier fetches YAML manifests from URLs and applies them via server-side apply.
type ManifestApplier struct {
	client     client.Client
	log        logr.Logger
	httpClient *http.Client
}

func NewManifestApplier(c client.Client, log logr.Logger) *ManifestApplier {
	return &ManifestApplier{
		client:     c,
		log:        log,
		httpClient: &http.Client{Timeout: 2 * time.Minute},
	}
}

// ObjectMutator changes objects of a manifest component before their
// server-side apply, knowing what the cluster has: a component hook folds
// ClusterConfig settings into them (VirtHook: the KubeVirt CR), so the
// settings and the manifest are one apply, by one field manager, and an
// apply of the manifest never drops them (spec R3).
type ObjectMutator interface {
	// Mutates tells whether Mutate changes obj, an object of the
	// component's manifests: only those are read from the cluster first.
	Mutates(obj *unstructured.Unstructured) bool
	// Mutate changes desired, the object about to be applied, from live,
	// the object in the cluster (nil: not there yet). spec is the component
	// being applied, its version resolved. The engine does not merge the
	// additive lists (additive_lists.go) of an object Mutate changes: Mutate
	// merges them, with MergeSharedList. It may run more than once for one
	// apply: on a conflict, with a fresh live object.
	Mutate(ctx context.Context, spec k4allv1alpha1.ComponentSpec, desired, live *unstructured.Unstructured) error
}

// Apply server-side applies the objects of the component's manifests, URL
// sources first, then inline manifests. mutator, if not nil, changes the
// objects it claims before their apply.
func (m *ManifestApplier) Apply(
	ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, mutator ObjectMutator,
) error {
	urls := spec.Sources
	if len(urls) == 0 && spec.Source != "" {
		urls = []string{spec.Source}
	}

	if len(urls) == 0 && len(spec.InlineManifests) == 0 {
		// Its hook renders its objects (k4all-metadata).
		m.log.V(1).Info("no manifests to apply", "component", name)
		return nil
	}

	// Apply URL-sourced manifests first
	for _, url := range urls {
		m.log.Info("applying manifest", "component", name, "url", url)
		objects, err := m.fetchAndDecode(ctx, url)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", url, err)
		}

		for _, obj := range objects {
			if spec.Namespace != "" && obj.GetNamespace() == "" {
				if isNamespaced(obj) {
					obj.SetNamespace(spec.Namespace)
				}
			}

			if err := m.serverSideApply(ctx, &obj, spec, mutator); err != nil {
				return fmt.Errorf("apply %s %s/%s: %w",
					obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
			}
		}
	}

	// Apply inline manifests (with retry, since CRDs from URL manifests may need time to register)
	for i, raw := range spec.InlineManifests {
		m.log.Info("applying inline manifest", "component", name, "index", i)
		objects, err := decodeMultiDoc([]byte(raw))
		if err != nil {
			return fmt.Errorf("decode inline manifest %d: %w", i, err)
		}

		for _, obj := range objects {
			if spec.Namespace != "" && obj.GetNamespace() == "" {
				if isNamespaced(obj) {
					obj.SetNamespace(spec.Namespace)
				}
			}

			if err := m.serverSideApplyWithRetry(ctx, &obj, spec, mutator); err != nil {
				return fmt.Errorf("apply inline %s %s/%s: %w",
					obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
			}
		}
	}

	return nil
}

func (m *ManifestApplier) serverSideApplyWithRetry(
	ctx context.Context, obj *unstructured.Unstructured, spec k4allv1alpha1.ComponentSpec, mutator ObjectMutator,
) error {
	retryCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		err := m.serverSideApply(retryCtx, obj, spec, mutator)
		if err == nil {
			return nil
		}

		m.log.V(1).Info("retrying inline manifest apply", "kind", obj.GetKind(), "name", obj.GetName(), "error", err)

		select {
		case <-retryCtx.Done():
			return fmt.Errorf("timed out: %w", err)
		case <-ticker.C:
		}
	}
}

func (m *ManifestApplier) Delete(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec) error {
	urls := spec.Sources
	if len(urls) == 0 && spec.Source != "" {
		urls = []string{spec.Source}
	}

	for _, url := range urls {
		objects, err := m.fetchAndDecode(ctx, url)
		if err != nil {
			m.log.V(1).Info("skipping delete fetch failure", "url", url, "error", err)
			continue
		}

		for i := len(objects) - 1; i >= 0; i-- {
			obj := &objects[i]
			if err := m.client.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
				m.log.V(1).Info("delete failed", "kind", obj.GetKind(), "name", obj.GetName(), "error", err)
			}
		}
	}

	for i, raw := range spec.InlineManifests {
		objects, err := decodeMultiDoc([]byte(raw))
		if err != nil {
			m.log.V(1).Info("skipping inline manifest decode failure on delete", "index", i, "error", err)
			continue
		}
		for j := len(objects) - 1; j >= 0; j-- {
			obj := &objects[j]
			if spec.Namespace != "" && obj.GetNamespace() == "" {
				if isNamespaced(*obj) {
					obj.SetNamespace(spec.Namespace)
				}
			}
			if err := m.client.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
				m.log.V(1).Info("inline manifest delete failed", "kind", obj.GetKind(), "name", obj.GetName(), "error", err)
			}
		}
	}

	return nil
}

func (m *ManifestApplier) fetchAndDecode(ctx context.Context, url string) ([]unstructured.Unstructured, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return nil, err
	}

	return decodeMultiDoc(body)
}

func decodeMultiDoc(data []byte) ([]unstructured.Unstructured, error) {
	var result []unstructured.Unstructured
	reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))

	for {
		doc, err := reader.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 || string(doc) == "---" {
			continue
		}

		obj := &unstructured.Unstructured{}
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(obj); err != nil {
			if err == io.EOF {
				continue
			}
			return nil, fmt.Errorf("decode YAML document: %w", err)
		}

		if obj.GetKind() == "" {
			continue
		}

		result = append(result, *obj)
	}
	return result, nil
}

func (m *ManifestApplier) serverSideApply(
	ctx context.Context, obj *unstructured.Unstructured, spec k4allv1alpha1.ComponentSpec, mutator ObjectMutator,
) error {
	obj.SetManagedFields(nil)
	if err := blockUninstallIfWorkloadsExist(obj); err != nil {
		return fmt.Errorf("%s %s: set spec.uninstallStrategy: %w", obj.GetKind(), obj.GetName(), err)
	}
	stripEmptyFields(obj)
	mutates := mutator != nil && mutator.Mutates(obj)
	if !mutates && len(additiveListsOf(obj)) == 0 {
		return m.apply(ctx, obj)
	}

	// Lists the admins add to (additive_lists.go), and the settings a hook
	// folds in, are merged with the object as read, in the one apply. The
	// apply is conditional on that resourceVersion: a change made in between
	// makes it fail with a conflict, and the merge starts again from a fresh
	// read.
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		desired := obj.DeepCopy()
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		switch err := m.client.Get(ctx, client.ObjectKeyFromObject(obj), live); {
		case errors.IsNotFound(err), meta.IsNoMatchError(err):
			live = nil // a fresh install: upstream's lists as they are
		case err != nil:
			return fmt.Errorf("read the live %s: %w", obj.GetKind(), err)
		default:
			desired.SetResourceVersion(live.GetResourceVersion())
		}
		if mutates {
			if err := mutator.Mutate(ctx, spec, desired, live); err != nil {
				return err
			}
		} else if err := keepLiveEntries(desired, live); err != nil {
			return err
		}
		return m.apply(ctx, desired)
	})
}

// FieldManager is the field manager of every write of the operator: its
// server-side applies, and its creates, updates and patches through the
// manager's client (spec R3, §16.2). Consoles tell what k4all owns from it.
const FieldManager = "k4all-operator"

// apply server-side applies obj as the k4all-operator, taking over the fields
// it sets from any other manager.
func (m *ManifestApplier) apply(ctx context.Context, obj *unstructured.Unstructured) error {
	return m.client.Patch(ctx, obj, client.Apply,
		client.FieldOwner(FieldManager),
		client.ForceOwnership,
	)
}

// isNamespaced returns true for resources that are typically namespaced.
// For a fully correct check, we'd need a RESTMapper, but this heuristic
// covers the common cluster-scoped kinds.
func isNamespaced(obj unstructured.Unstructured) bool {
	kind := obj.GetKind()
	clusterScoped := map[string]bool{
		"Namespace":                      true,
		"Node":                           true,
		"PersistentVolume":               true,
		"ClusterRole":                    true,
		"ClusterRoleBinding":             true,
		"CustomResourceDefinition":       true,
		"MutatingWebhookConfiguration":   true,
		"ValidatingWebhookConfiguration": true,
		"APIService":                     true,
		"PriorityClass":                  true,
		"StorageClass":                   true,
		"CSIDriver":                      true,
		"CSINode":                        true,
		"VolumeAttachment":               true,
		"IngressClass":                   true,
	}
	return !clusterScoped[kind]
}

// WaitForResource polls until a resource exists or context is cancelled.
func WaitForResource(ctx context.Context, c client.Client, gvk, namespacedName types.NamespacedName, obj *unstructured.Unstructured) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			err := c.Get(ctx, namespacedName, obj)
			if err == nil {
				return nil
			}
			if !errors.IsNotFound(err) {
				return err
			}
		}
	}
}
