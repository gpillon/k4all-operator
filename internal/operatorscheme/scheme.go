// Package operatorscheme builds the scheme of the operator's manager: the
// types its client maps to API resources when it reads or writes a typed
// object. cmd/main.go and the tests build it here alike, so a type the code
// writes and the manager lacks fails a test: the kubevirt-manager removal
// deleted its image catalog CRD as a typed CustomResourceDefinition, which
// only the hook's test scheme knew (gpillon/k4all-operator#45).
package operatorscheme

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// New returns the manager's scheme: Kubernetes' built-in types,
// apiextensions/v1 (CRDs) and k4all's API.
func New() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	utilruntime.Must(k4allv1alpha1.AddToScheme(s))
	return s
}
