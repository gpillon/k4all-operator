package hooks

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Hook is called around component installation and removal.
// PreInstall can modify Helm values or prepare the cluster.
// PostInstall runs after the component has been installed/upgraded.
// Cleanup runs when a component is being uninstalled (CNI switch, feature disabled, management=Removed).
type Hook interface {
	PreInstall(ctx context.Context, name string, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec, values map[string]interface{}) (map[string]interface{}, error)
	PostInstall(ctx context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) error
	Cleanup(ctx context.Context, name string, config k4allv1alpha1.ClusterConfigSpec) error
}

// BaseHook provides no-op defaults so hooks only override what they need.
// A hook may also implement engine.VersionReporter (KubeVirt, CDI): the
// reconciler then follows the component's upgrade path from the version it
// reports, and waits on it between steps.
type BaseHook struct{}

func (BaseHook) PreInstall(_ context.Context, _ string, _ k4allv1alpha1.ComponentSpec, _ k4allv1alpha1.ClusterConfigSpec, values map[string]interface{}) (map[string]interface{}, error) {
	return values, nil
}

func (BaseHook) PostInstall(_ context.Context, _ k4allv1alpha1.ComponentSpec, _ k4allv1alpha1.ClusterConfigSpec) error {
	return nil
}

func (BaseHook) Cleanup(_ context.Context, _ string, _ k4allv1alpha1.ClusterConfigSpec) error {
	return nil
}

// Syncer is implemented by a hook that keeps its component in line with the
// cluster between installs (KubeVirt: emulation "auto" follows the nodes).
// The reconciler calls Sync after PostInstall and on every reconcile while
// the component is installed and up to date. A positive duration asks for the
// next reconcile within that time, also with an error (which is logged).
type Syncer interface {
	Sync(ctx context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) (time.Duration, error)
}

// ValuesRevisioner is implemented by a hook whose PreInstall values change
// between operator versions for the same ClusterConfig. The reconciler adds
// the revision to the component's appliedHash, so a cluster re-applies the
// component once after the operator upgrade instead of keeping the values
// the previous operator rendered.
type ValuesRevisioner interface {
	ValuesRevision() string
}

// Registry maps component names to their hooks.
type Registry struct {
	hooks map[string]Hook
}

// Option configures a Registry.
type Option func(*options)

type options struct{ restConfig *rest.Config }

// WithRestConfig lets hooks read the API server directly, e.g. to tell
// whether a namespace a removed component left is empty.
func WithRestConfig(cfg *rest.Config) Option {
	return func(o *options) { o.restConfig = cfg }
}

func NewRegistry(c client.Client, log logr.Logger, opts ...Option) *Registry {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	r := &Registry{
		hooks: make(map[string]Hook),
	}
	r.hooks["kubevirt"] = NewVirtHook(c, log.WithName("hook-virt"))
	r.hooks["cdi"] = NewCDIHook(c)
	r.hooks["cilium"] = NewCiliumHook(c, log.WithName("hook-cilium"))
	r.hooks["headlamp"] = NewHeadlampHook(c, log.WithName("hook-headlamp"))
	nginx := NewIngressNginxHook(c, log.WithName("hook-ingress-nginx"))
	nginx.restConfig = o.restConfig
	r.hooks["ingress-nginx"] = nginx
	r.hooks["traefik"] = NewTraefikHook(c, log.WithName("hook-traefik"))
	r.hooks["metallb"] = NewMetalLBHook(c, log.WithName("hook-metallb"))
	r.hooks["topolvm"] = NewTopolvmHook(c, log.WithName("hook-topolvm"))
	r.hooks["calico"] = NewCalicoHook(c, log.WithName("hook-calico"))
	r.hooks["nmstate"] = NewNMStateHook(c, log.WithName("hook-nmstate"))
	return r
}

// Register sets the hook of a component, replacing the built-in one.
func (r *Registry) Register(componentName string, h Hook) {
	r.hooks[componentName] = h
}

// Get returns the hook for a component, or nil if none exists.
func (r *Registry) Get(componentName string) Hook {
	return r.hooks[componentName]
}
