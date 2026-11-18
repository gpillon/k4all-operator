package hooks

import (
	"context"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IngressNginxHook computes Helm values for the NGINX Ingress Controller
// based on ClusterConfig (dedicated IP, isDefault, HA type, host networking).
// Mirrors the logic from setup-nginx-ingress.sh.
type IngressNginxHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
	// restConfig reaches the API server directly, for the namespace check.
	restConfig *rest.Config
	// namespace of the release, ingress-nginx; set apart only by tests.
	namespace string
	// How long and how often the namespace check waits for what the
	// uninstall is still deleting; 0 means the defaults (tests shorten them).
	settle, poll time.Duration
}

func NewIngressNginxHook(c client.Client, log logr.Logger) *IngressNginxHook {
	return &IngressNginxHook{client: c, log: log}
}

func (h *IngressNginxHook) PreInstall(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec, values map[string]interface{}) (map[string]interface{}, error) {
	nginxCfg := config.Ingress.NginxSettings()
	haType := config.Cluster.HA.Type

	controller, _ := values["controller"].(map[string]interface{})
	if controller == nil {
		controller = make(map[string]interface{})
	}

	service, _ := controller["service"].(map[string]interface{})
	if service == nil {
		service = make(map[string]interface{})
	}

	labels, _ := service["labels"].(map[string]interface{})
	if labels == nil {
		labels = make(map[string]interface{})
	}
	labels["k4all-ingress"] = "nginx"
	service["labels"] = labels

	dedicatedIP := nginxCfg.DedicatedIP
	isDefault := nginxCfg.IsDefault

	if dedicatedIP != "" {
		service["type"] = serviceTypeLoadBalancer
		controller["hostNetwork"] = false
		service["loadBalancerIP"] = dedicatedIP
	} else {
		clusterIP, err := nodeInternalIP(ctx, h.client)
		if err != nil {
			h.log.V(1).Info("could not detect cluster IP, skipping externalIPs", "error", err)
		} else {
			service["externalIPs"] = []string{clusterIP}
			service["loadBalancerIP"] = clusterIP
		}
		service["type"] = serviceTypeLoadBalancer
	}

	if haType == haTypeKubeVIP && dedicatedIP == "" {
		service["loadBalancerClass"] = kubeVIPLoadBalancer
	}

	ingressClassResource, _ := controller["ingressClassResource"].(map[string]interface{})
	if ingressClassResource == nil {
		ingressClassResource = make(map[string]interface{})
	}
	ingressClassResource["default"] = isDefault
	controller["ingressClassResource"] = ingressClassResource

	// When there's no dedicated IP and no kubevip, use hostPort for direct node access
	if dedicatedIP == "" && haType != haTypeKubeVIP && isDefault {
		h.log.Info("using hostPort mode for NGINX (no dedicated IP, no kubevip)")
		hostPort := make(map[string]interface{})
		hostPort["enabled"] = true
		controller["hostPort"] = hostPort
		service["type"] = "ClusterIP"
	} else {
		hostPort := make(map[string]interface{})
		hostPort["enabled"] = false
		controller["hostPort"] = hostPort
	}

	controller["service"] = service
	values["controller"] = controller

	return values, nil
}

// ingressNginxNamespace is where the ingress-nginx release lives.
const ingressNginxNamespace = "ingress-nginx"

// What ingress-nginx leaves outside its Helm release: the certificate the
// admission certgen Job writes, and the controller's leader-election Lease.
var ingressNginxLeftovers = []client.Object{
	&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ingress-nginx-admission"}},
	&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "ingress-nginx-leader"}},
}

// Cleanup runs after helm uninstall (management Removed, k4all v2.4.0). It
// deletes what ingress-nginx left outside its release, then its namespace if
// nothing else is in it once what the uninstall deletes is gone (up to 30
// seconds). All best effort, only logged: an error here would
// leave ingress-nginx Failed and stop the host's sync.
func (h *IngressNginxHook) Cleanup(ctx context.Context, _ string, _ k4allv1alpha1.ClusterConfigSpec) error {
	ns := h.namespace
	if ns == "" {
		ns = ingressNginxNamespace
	}
	for _, o := range ingressNginxLeftovers {
		obj := o.DeepCopyObject().(client.Object)
		obj.SetNamespace(ns)
		if err := h.client.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			h.log.Info("cannot delete an ingress-nginx leftover", "namespace", ns, "name", obj.GetName(), "error", err.Error())
		}
	}
	if h.restConfig == nil {
		return nil
	}
	left, err := h.waitForLeftovers(ctx, ns)
	switch {
	case err != nil:
		h.log.Info("namespace kept: cannot tell whether it is empty", "namespace", ns, "error", err.Error())
	case len(left) > 0:
		h.log.Info("namespace kept: it holds objects ingress-nginx did not leave", "namespace", ns, "objects", left)
	default:
		err := h.client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		if err != nil && !errors.IsNotFound(err) {
			h.log.Info("cannot delete the namespace", "namespace", ns, "error", err.Error())
		} else if err == nil {
			h.log.Info("deleted the namespace ingress-nginx left", "namespace", ns)
		}
	}
	return nil
}

// Defaults for how long and how often the cleanup waits for what helm
// uninstall is still deleting. Short: removals run before the other
// components, and Traefik, which replaces ingress-nginx, comes after.
const (
	cleanupSettle = 30 * time.Second
	cleanupPoll   = 2 * time.Second
)

// waitForLeftovers checks the namespace until nothing is left in it, or
// until the settle time: helm uninstall does not wait for what it deletes,
// and right after it the namespace still holds objects on their way out
// (in an e2e run, the controller pod, listed without its owner). It returns
// the last check.
func (h *IngressNginxHook) waitForLeftovers(ctx context.Context, ns string) ([]string, error) {
	settle, poll := h.settle, h.poll
	if settle == 0 {
		settle = cleanupSettle
	}
	if poll == 0 {
		poll = cleanupPoll
	}
	deadline := time.Now().Add(settle)
	for {
		left, err := namespaceLeftovers(ctx, h.restConfig, ns)
		if (err == nil && len(left) == 0) || time.Now().After(deadline) {
			return left, err
		}
		select {
		case <-ctx.Done():
			return left, ctx.Err()
		case <-time.After(poll):
		}
	}
}
