package engine

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// ManifestObjects lists the objects an apply of a manifest component writes,
// as ManifestApplier.Apply reads them: the objects of its sources (Sources,
// or Source) at its version, then those of its inline manifests, each
// namespaced object without a namespace in the component's namespace. For a
// hook that removes its component itself and deletes only some of them (a
// Remover). Any source or inline manifest that cannot be read fails the
// whole list: a partial one must not pass for what the component has.
func ManifestObjects(ctx context.Context, spec k4allv1alpha1.ComponentSpec) ([]unstructured.Unstructured, error) {
	urls := spec.Sources
	if len(urls) == 0 && spec.Source != "" {
		urls = []string{spec.Source}
	}
	// The URL templating of Engine.templateSpec, which runs before Apply.
	template := strings.NewReplacer("{{ version }}", spec.Version, "{{ arch }}", goArchToK8s(runtime.GOARCH))
	m := &ManifestApplier{httpClient: &http.Client{Timeout: 2 * time.Minute}}
	var objects []unstructured.Unstructured
	add := func(objs []unstructured.Unstructured) {
		for _, obj := range objs {
			if spec.Namespace != "" && obj.GetNamespace() == "" && isNamespaced(obj) {
				obj.SetNamespace(spec.Namespace)
			}
			objects = append(objects, obj)
		}
	}
	for _, url := range urls {
		objs, err := m.fetchAndDecode(ctx, template.Replace(url))
		if err != nil {
			return nil, err
		}
		add(objs)
	}
	for i, raw := range spec.InlineManifests {
		objs, err := decodeMultiDoc([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("decode inline manifest %d: %w", i, err)
		}
		add(objs)
	}
	return objects, nil
}
