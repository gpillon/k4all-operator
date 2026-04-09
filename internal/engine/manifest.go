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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
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

func (m *ManifestApplier) Apply(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec) error {
	urls := spec.Sources
	if len(urls) == 0 && spec.Source != "" {
		urls = []string{spec.Source}
	}

	if len(urls) == 0 && len(spec.InlineManifests) == 0 {
		return fmt.Errorf("component %s: no source URLs or inline manifests defined", name)
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

			if err := m.serverSideApply(ctx, &obj); err != nil {
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

			if err := m.serverSideApplyWithRetry(ctx, &obj); err != nil {
				return fmt.Errorf("apply inline %s %s/%s: %w",
					obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
			}
		}
	}

	return nil
}

func (m *ManifestApplier) serverSideApplyWithRetry(ctx context.Context, obj *unstructured.Unstructured) error {
	retryCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		err := m.serverSideApply(retryCtx, obj)
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
	defer resp.Body.Close()

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

func (m *ManifestApplier) serverSideApply(ctx context.Context, obj *unstructured.Unstructured) error {
	obj.SetManagedFields(nil)

	patch := client.Apply
	opts := []client.PatchOption{
		client.FieldOwner("k4all-operator"),
		client.ForceOwnership,
	}

	return m.client.Patch(ctx, obj, patch, opts...)
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
