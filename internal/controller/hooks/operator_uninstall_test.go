package hooks

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// A KubeVirt CRD that does not serve spec.uninstallStrategy drops it: the
// KubeVirt CR is then not deleted, virt-operator would delete the VMs with
// it. (KubeVirt's own CRD has the field; this one prunes every unknown one.)
func TestVirtHook_KeepsAKubeVirtThatCannotBlockItsUninstall(t *testing.T) {
	c, cfg := envtestClient(t)
	ctx := context.Background()
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "kubevirts.kubevirt.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "kubevirt.io", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind: "KubeVirt", ListKind: "KubeVirtList", Plural: "kubevirts", Singular: "kubevirt",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type:       "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": {Type: "object"}},
				}},
			}},
		},
	}
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{crd}}); err != nil {
		t.Fatalf("install the KubeVirt CRD: %v", err)
	}
	if err := client.IgnoreAlreadyExists(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kubevirt"}})); err != nil {
		t.Fatal(err)
	}
	kv := &unstructured.Unstructured{}
	kv.SetAPIVersion("kubevirt.io/v1")
	kv.SetKind("KubeVirt")
	kv.SetNamespace("kubevirt")
	kv.SetName("kubevirt")
	kv.Object["spec"] = map[string]interface{}{}
	if err := c.Create(ctx, kv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), kv) })

	pending, err := NewVirtHook(c, logr.Discard()).PrepareUninstall(ctx, "kubevirt")

	if err == nil || !strings.Contains(err.Error(), "does not keep spec.uninstallStrategy BlockUninstallIfWorkloadsExist") {
		t.Fatalf("PrepareUninstall = %q, %v; want an error: the CRD drops the field", pending, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(kv), kv); err != nil || kv.GetDeletionTimestamp() != nil {
		t.Fatalf("KubeVirt: get = %v, deleting %v; want it kept", err, kv.GetDeletionTimestamp() != nil)
	}
}
