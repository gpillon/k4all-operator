package engine

import (
	"context"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// ManifestObjects fetches and decodes the objects of a manifest component's
// sources at its version, in the namespace an apply puts them: for a hook
// that removes its component itself and deletes only some of them (a
// Remover).
func ManifestObjects(ctx context.Context, spec k4allv1alpha1.ComponentSpec) ([]unstructured.Unstructured, error) {
	urls := spec.Sources
	if len(urls) == 0 && spec.Source != "" {
		urls = []string{spec.Source}
	}
	m := &ManifestApplier{httpClient: &http.Client{Timeout: 2 * time.Minute}}
	var objects []unstructured.Unstructured
	for _, url := range urls {
		objs, err := m.fetchAndDecode(ctx, strings.ReplaceAll(url, "{{ version }}", spec.Version))
		if err != nil {
			return nil, err
		}
		for _, obj := range objs {
			if spec.Namespace != "" && obj.GetNamespace() == "" && isNamespaced(obj) {
				obj.SetNamespace(spec.Namespace)
			}
			objects = append(objects, obj)
		}
	}
	return objects, nil
}
