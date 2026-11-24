package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// The policies and bindings of the k4all-metadata component.
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingadmissionpolicies;mutatingadmissionpolicybindings,verbs=get;create;patch;delete

// metadataFieldManager writes what k4all owns.
const metadataFieldManager = "k4all-operator"

// MetadataHook runs the k4all-metadata component, the metadata service on
// 169.254.169.254 (spec §11): its objects are rendered here from the
// component's image and spec.features.virt.metadata, not fetched (the
// component has no sources). The per-VM data is the metadata controller's.
// No UninstallGuard: the component holds no data, and running VMs keep
// their server until their pod goes.
type MetadataHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
	// restConfig reads the API server's issuer (optional).
	restConfig *rest.Config
}

// NewMetadataHook returns the hook of the k4all-metadata component.
func NewMetadataHook(c client.Client, log logr.Logger) *MetadataHook {
	return &MetadataHook{client: c, log: log}
}

// Sync server-side applies the two MutatingAdmissionPolicies and their
// bindings as k4all-operator, at every reconcile: a change by hand is put
// back, and a settings change reaches the pods created after it (running
// VMs keep what their pod got until they restart). A setting refused (an
// audience the API server accepts) applies nothing: what runs stays.
func (h *MetadataHook) Sync(ctx context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) (time.Duration, error) {
	s := MetadataSettingsFrom(config)
	if err := h.checkAudiences(ctx, s.Audiences); err != nil {
		return 0, err
	}
	objs, err := metadataObjects(strings.ReplaceAll(spec.Image, "{{ version }}", spec.Version), s)
	if err != nil {
		return 0, err
	}
	for _, obj := range objs {
		err := h.client.Patch(ctx, obj, client.Apply, client.FieldOwner(metadataFieldManager), client.ForceOwnership)
		if meta.IsNoMatchError(err) {
			return 0, fmt.Errorf("the API server does not serve %s %s: the metadata service needs Kubernetes 1.36 or later",
				admissionAPIVersion, obj.GetKind())
		}
		if err != nil {
			return 0, fmt.Errorf("apply %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return 0, nil
}

// Cleanup deletes the policies (metadata or virt turned off): pods created
// after get nothing.
func (h *MetadataHook) Cleanup(ctx context.Context, _ string, _ k4allv1alpha1.ClusterConfigSpec) error {
	for _, name := range []string{MetadataPolicy, MetadataVMIPolicy} {
		for _, kind := range []string{"MutatingAdmissionPolicyBinding", "MutatingAdmissionPolicy"} {
			obj := &unstructured.Unstructured{}
			obj.SetAPIVersion(admissionAPIVersion)
			obj.SetKind(kind)
			obj.SetName(name)
			err := h.client.Delete(ctx, obj)
			if err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
				return fmt.Errorf("delete %s %s: %w", kind, name, err)
			}
		}
	}
	return nil
}

// kubeadmAudiences are the audiences of a kubeadm API server: its default
// issuer, https://kubernetes.default.svc.<cluster domain>, and the names
// the API is reached by.
var kubeadmAudiences = []string{
	"https://kubernetes.default.svc.cluster.local", "https://kubernetes.default.svc", "https://kubernetes.default",
	"https://kubernetes", "kubernetes.default.svc.cluster.local", "kubernetes.default.svc", "kubernetes.default",
	"kubernetes",
}

// checkAudiences refuses an audience the API server takes as its own: a
// token for it, taken from a guest, could call the API as the VM's
// ServiceAccount.
func (h *MetadataHook) checkAudiences(ctx context.Context, audiences []string) error {
	refused := append([]string{}, kubeadmAudiences...)
	if issuer := h.issuer(ctx); issuer != "" {
		refused = append(refused, issuer)
	}
	for _, aud := range audiences {
		for _, r := range refused {
			if strings.EqualFold(strings.TrimSuffix(aud, "/"), r) {
				return fmt.Errorf("token audience %q refused: the API server accepts it, a guest could call the API "+
					"with it (spec.features.virt.metadata.tokens.audiences)", aud)
			}
		}
	}
	return nil
}

// issuer is the service account issuer the API server publishes (its
// default audience), "" when it cannot be read.
func (h *MetadataHook) issuer(ctx context.Context) string {
	if h.restConfig == nil {
		return ""
	}
	hc, err := rest.HTTPClientFor(h.restConfig)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(h.restConfig.Host, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return ""
	}
	resp, err := hc.Do(req)
	if err != nil {
		h.log.V(1).Info("the API server's issuer is unknown", "error", err.Error())
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return ""
	}
	return strings.TrimSuffix(doc.Issuer, "/")
}
