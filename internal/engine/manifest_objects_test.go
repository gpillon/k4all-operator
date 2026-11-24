package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// served answers path with body, as GitHub serves a release asset.
func served(t *testing.T, path, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const sourceManifest = `apiVersion: v1
kind: ServiceAccount
metadata:
  name: from-source
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: from-source
`

// ManifestObjects lists what an apply of the component writes, in the
// order it writes it: the sources' objects, then those of its inline
// manifests, each namespaced object without a namespace in the component's
// namespace. A Remover that deletes "what the manifest has" deletes the
// inline objects too.
func TestManifestObjects_SourcesThenInlineManifests(t *testing.T) {
	arch := goArchToK8s(runtime.GOARCH)
	url := served(t, "/v1.2.3/"+arch+"/objects.yaml", sourceManifest)
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1.2.3", Namespace: "probe-system",
		Source: url + "/{{ version }}/{{ arch }}/objects.yaml",
		InlineManifests: []string{
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: inline-one\n",
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: inline-two\n  namespace: elsewhere\n" +
				"---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: inline-three\n",
		},
	}

	objects, err := ManifestObjects(context.Background(), spec)
	if err != nil {
		t.Fatalf("ManifestObjects: %v", err)
	}

	got := make([]string, 0, len(objects))
	for _, o := range objects {
		got = append(got, o.GetKind()+" "+o.GetNamespace()+"/"+o.GetName())
	}
	want := []string{
		"ServiceAccount probe-system/from-source",
		"ClusterRole /from-source",
		"Deployment probe-system/inline-one",
		"ConfigMap elsewhere/inline-two",
		"Namespace /inline-three",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("objects\n  %v\nwant\n  %v", got, want)
	}
}

// A component with inline manifests only (no source) has objects too.
func TestManifestObjects_InlineManifestsOnly(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1",
		InlineManifests: []string{"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: only\n  namespace: default\n"},
	}

	objects, err := ManifestObjects(context.Background(), spec)
	if err != nil {
		t.Fatalf("ManifestObjects: %v", err)
	}
	if len(objects) != 1 || objects[0].GetName() != "only" {
		t.Errorf("objects %v, want the inline ConfigMap", objects)
	}
}

// An inline manifest that does not decode fails, as its apply would: a
// Remover must not take a partial list for the whole.
func TestManifestObjects_ABrokenInlineManifestFails(t *testing.T) {
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Version: "v1",
		InlineManifests: []string{"apiVersion: v1\nkind: ConfigMap\nmetadata: [not, a, map\n"},
	}

	if _, err := ManifestObjects(context.Background(), spec); err == nil ||
		!strings.Contains(err.Error(), "inline manifest 0") {
		t.Errorf("ManifestObjects = %v, want the inline manifest's decode error", err)
	}
}
