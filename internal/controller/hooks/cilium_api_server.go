package hooks

import (
	"context"
	goerrors "errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/gpillon/k4all-operator/internal/engine"
)

// The Cilium agents and operator reach the API server at the address the
// kubelet gives every pod, the "kubernetes" ClusterIP, unless the chart's
// k8sServiceHost and k8sServicePort name another. On a cluster without
// kube-proxy only Cilium serves that ClusterIP: an agent that starts cannot
// reach the API server through it, and the node's CNI stays down
// (gpillon/k4all#114). Clusters restored from k4all 1.x run so: Cilium with
// kubeProxyReplacement, no kube-proxy, and k8sServiceHost in their release.
// With kube-proxy (every cluster k4all 2.x installs) the values stay as
// they were.

// kubeSystem is where kubeadm puts kube-proxy's DaemonSet (kubeProxyName),
// its configuration and the control plane's static pods.
const (
	kubeSystem    = "kube-system"
	kubeProxyName = "kube-proxy"
)

// kubeadm's configuration of the cluster, and the annotation kubeadm puts on
// each kube-apiserver static pod: the address and port it advertises.
const (
	kubeadmConfigName           = "kubeadm-config"
	kubeadmClusterConfiguration = "ClusterConfiguration"
	apiServerEndpointAnnotation = "kubeadm.kubernetes.io/kube-apiserver.advertise-address.endpoint"
	// kubeadm's default API server port, the one k4all uses: kubeadm takes
	// it for a controlPlaneEndpoint without a port.
	kubeadmAPIServerPort = 6443
)

// The chart values that name the API server.
const (
	ciliumServiceHostValue = "k8sServiceHost"
	ciliumServicePortValue = "k8sServicePort"
)

// errNoAPIServerEndpoint says that the cluster runs no kube-proxy and that
// where the API server is cannot be read from the cluster.
var errNoAPIServerEndpoint = goerrors.New("no API server endpoint found")

// apiServerEndpoint is the host and port the API server is reached at.
type apiServerEndpoint struct {
	Host string
	Port int
}

// ciliumAPIServer returns the endpoint the Cilium agents and operator must
// be given (k8sServiceHost, k8sServicePort) on a cluster without
// kube-proxy, nil with kube-proxy. The endpoint is kubeadm's: the
// controlPlaneEndpoint of kube-system/kubeadm-config (the API VIP with
// kube-vip or keepalived, a custom endpoint), else, with a single control
// plane, the address its kube-apiserver advertises. These come with a
// cluster restored from k4all 1.x, whose etcd and static pods are its own.
// Without them errNoAPIServerEndpoint: the endpoint is never guessed. Any
// other read error is returned.
func ciliumAPIServer(ctx context.Context, c client.Reader) (*apiServerEndpoint, error) {
	err := c.Get(ctx, types.NamespacedName{Namespace: kubeSystem, Name: kubeProxyName}, &appsv1.DaemonSet{})
	switch {
	case err == nil:
		return nil, nil
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("tell whether the cluster runs kube-proxy: %w", err)
	}
	ep, err := controlPlaneEndpoint(ctx, c)
	if ep != nil || err != nil {
		return ep, err
	}
	return advertisedEndpoint(ctx, c)
}

// controlPlaneEndpoint is kubeadm's controlPlaneEndpoint, nil when it sets
// none (or the cluster has no kubeadm-config).
func controlPlaneEndpoint(ctx context.Context, c client.Reader) (*apiServerEndpoint, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: kubeSystem, Name: kubeadmConfigName}, cm)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %s/%s: %w", kubeSystem, kubeadmConfigName, err)
	}
	var cluster struct {
		ControlPlaneEndpoint string `json:"controlPlaneEndpoint"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data[kubeadmClusterConfiguration]), &cluster); err != nil {
		return nil, fmt.Errorf("%s/%s %s: %w", kubeSystem, kubeadmConfigName, kubeadmClusterConfiguration, err)
	}
	if cluster.ControlPlaneEndpoint == "" {
		return nil, nil
	}
	ep, err := parseEndpoint(cluster.ControlPlaneEndpoint)
	if err != nil {
		return nil, fmt.Errorf("%s/%s controlPlaneEndpoint: %w", kubeSystem, kubeadmConfigName, err)
	}
	return ep, nil
}

// advertisedEndpoint is the address the single kube-apiserver advertises
// (kubeadm's annotation on its static pod). Several control planes without
// a controlPlaneEndpoint (kubeadm joins none without one) are not guessed
// among.
func advertisedEndpoint(ctx context.Context, c client.Reader) (*apiServerEndpoint, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(kubeSystem),
		client.MatchingLabels{"component": "kube-apiserver", "tier": "control-plane"}); err != nil {
		return nil, fmt.Errorf("list the kube-apiserver pods: %w", err)
	}
	seen := map[string]bool{}
	for _, p := range pods.Items {
		if ep := p.Annotations[apiServerEndpointAnnotation]; ep != "" {
			seen[ep] = true
		}
	}
	endpoints := make([]string, 0, len(seen))
	for ep := range seen {
		endpoints = append(endpoints, ep)
	}
	sort.Strings(endpoints)
	switch len(endpoints) {
	case 0:
		return nil, fmt.Errorf("%w: kube-system/kubeadm-config has no controlPlaneEndpoint "+
			"and no kube-apiserver pod has the %s annotation", errNoAPIServerEndpoint, apiServerEndpointAnnotation)
	case 1:
		ep, err := parseEndpoint(endpoints[0])
		if err != nil {
			return nil, fmt.Errorf("kube-apiserver %s: %w", apiServerEndpointAnnotation, err)
		}
		return ep, nil
	}
	return nil, fmt.Errorf("%w: kube-system/kubeadm-config has no controlPlaneEndpoint "+
		"and the kube-apiservers advertise several (%s)", errNoAPIServerEndpoint, strings.Join(endpoints, ", "))
}

// parseEndpoint reads host[:port]; kubeadm's port when it has none.
func parseEndpoint(endpoint string) (*apiServerEndpoint, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		// No port: a name, an IPv4 address, a bare or bracketed IPv6 one.
		host = strings.TrimSuffix(strings.TrimPrefix(endpoint, "["), "]")
		if host == "" || (strings.Contains(host, ":") && net.ParseIP(host) == nil) {
			return nil, fmt.Errorf("%q is no host[:port]", endpoint)
		}
		return &apiServerEndpoint{Host: host, Port: kubeadmAPIServerPort}, nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 || host == "" {
		return nil, fmt.Errorf("%q is no host:port", endpoint)
	}
	return &apiServerEndpoint{Host: host, Port: n}, nil
}

// ciliumAPIServerValues sets k8sServiceHost and k8sServicePort in values
// on a cluster without kube-proxy (ciliumAPIServer). When the endpoint
// cannot be found, Cilium is not applied (an error), unless the
// component's componentOverride sets k8sServiceHost itself.
func ciliumAPIServerValues(
	ctx context.Context, c client.Reader, name string, config k4allv1alpha1.ClusterConfigSpec, values map[string]interface{},
) error {
	ep, err := ciliumAPIServer(ctx, c)
	switch {
	case goerrors.Is(err, errNoAPIServerEndpoint):
		if overrideSets(config, name, ciliumServiceHostValue) {
			return nil
		}
		return fmt.Errorf("the cluster runs no kube-proxy, so the Cilium agents need the API server's address "+
			"(%s, %s), and %v: not applying Cilium without it (set both in the ClusterConfig's "+
			"componentOverrides.%s values)", ciliumServiceHostValue, ciliumServicePortValue, err, name)
	case err != nil:
		return err
	case ep != nil:
		values[ciliumServiceHostValue] = ep.Host
		values[ciliumServicePortValue] = ep.Port
	}
	return nil
}

// overrideSets tells whether the ClusterConfig's componentOverride of the
// component sets the top-level value key.
func overrideSets(config k4allv1alpha1.ClusterConfigSpec, name, key string) bool {
	override, ok := config.ComponentOverrides[name]
	if !ok {
		return false
	}
	values, err := engine.ApplyOverride(nil, override)
	if err != nil {
		return false
	}
	_, set := values[key]
	return set
}
