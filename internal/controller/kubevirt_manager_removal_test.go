package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// kubevirt-manager removed (management: Removed, k4all v2.7.0) by the
// operator's own client, whose scheme is the manager's: up to v0.6.0 it
// lacked apiextensions/v1, and the empty image catalog CRD stayed (#45).

const imageCatalogCRD = "images.kubevirt-manager.io"

// kubevirtManagerRemoved is kubevirt-manager with management Removed, in
// status, with its image catalog CRD served. The reconciler writes with the
// operator's client.
func kubevirtManagerRemoved(t *testing.T, status k4allv1alpha1.ComponentStatus) (client.Client, *ReleaseReconciler) {
	t.Helper()
	spec := k4allv1alpha1.ComponentSpec{
		Type: k4allv1alpha1.ComponentTypeManifest, Management: k4allv1alpha1.ComponentManagementRemoved,
		Version: "v1.5.4", FeatureGate: "virt", Namespace: "kubevirt-manager",
	}
	c, r := componentOnEnvtest(t, "kubevirt-manager", spec, status)
	withTestCRD(t, c, "kubevirt-manager.io_images.yaml", imageCatalogCRD)
	r.Client = operatorClient(t)
	return c, r
}

func imageCatalogExists(t *testing.T, c client.Client) bool {
	t.Helper()
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	err := c.Get(context.Background(), client.ObjectKey{Name: imageCatalogCRD}, crd)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	// Deleted, its finalizer still removing what it holds: gone.
	return crd.GetDeletionTimestamp() == nil
}

// image is an Image of kubevirt-manager's catalog, the user's.
func image(t *testing.T, c client.Client) {
	t.Helper()
	img := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubevirt-manager.io/v1",
		"kind":       "Image",
		"metadata":   map[string]interface{}{"name": "fedora-cloud", "namespace": metav1.NamespaceDefault},
		"spec": map[string]interface{}{
			"type": "http", "readableName": "Fedora Cloud",
			"http": map[string]interface{}{"url": "https://example.com/fedora.qcow2"},
		},
	}}
	if err := c.Create(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), img) })
}

// The v2.6.x → v2.7.0 hop: kubevirt-manager Installed, then Removed; its
// empty image catalog goes with it.
func TestReconcile_KubevirtManagerRemovalDeletesAnEmptyImageCatalog(t *testing.T) {
	c, r := kubevirtManagerRemoved(t, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateInstalled, InstalledVersion: "v1.5.4", LastTransitionTime: metav1.Now(),
	})

	reconcileProbe(t, r)

	if st := componentStatus(t, c, "kubevirt-manager"); st.State != k4allv1alpha1.ComponentStateRemoved {
		t.Fatalf("kubevirt-manager %s (%s), want Removed", st.State, st.Message)
	}
	if imageCatalogExists(t, c) {
		t.Errorf("CRD %s kept with no image in it", imageCatalogCRD)
	}
}

// A cluster that v0.6.0 moved to v2.7.0: kubevirt-manager Removed, its
// empty image catalog CRD left. The first reconcile of the new operator
// deletes it, and the next ones do not look again.
func TestReconcile_TheImageCatalogV060LeftIsDeleted(t *testing.T) {
	c, r := kubevirtManagerRemoved(t, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateRemoved, Message: "removed by management policy", LastTransitionTime: metav1.Now(),
	})
	base, err := client.NewWithWatch(envtestConfig(t), client.Options{Scheme: gateScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	lists := 0
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Group == "kubevirt-manager.io" {
				lists++
			}
			return cl.List(ctx, list, opts...)
		},
	})

	reconcileProbe(t, r)

	if imageCatalogExists(t, c) {
		t.Errorf("CRD %s left by v0.6.0 kept with no image in it", imageCatalogCRD)
	}
	reconcileProbe(t, r)
	if lists != 1 {
		t.Errorf("the image catalog listed %d times in two reconciles, want once per operator start", lists)
	}
	if st := componentStatus(t, c, "kubevirt-manager"); st.State != k4allv1alpha1.ComponentStateRemoved {
		t.Errorf("kubevirt-manager %s (%s), want Removed", st.State, st.Message)
	}
}

// The catalog's Images are the user's: the CRD stays while one is in it.
func TestReconcile_TheImageCatalogV060LeftStaysWithImages(t *testing.T) {
	c, r := kubevirtManagerRemoved(t, k4allv1alpha1.ComponentStatus{
		State: k4allv1alpha1.ComponentStateRemoved, Message: "removed by management policy", LastTransitionTime: metav1.Now(),
	})
	image(t, c)

	reconcileProbe(t, r)

	if !imageCatalogExists(t, c) {
		t.Errorf("CRD %s deleted with the image default/fedora-cloud in it", imageCatalogCRD)
	}
}
