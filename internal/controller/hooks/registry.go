package hooks

import (
	"context"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
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

// Registry maps component names to their hooks.
type Registry struct {
	hooks map[string]Hook
}

func NewRegistry(c client.Client, log logr.Logger) *Registry {
	r := &Registry{
		hooks: make(map[string]Hook),
	}
	r.hooks["kubevirt"] = NewVirtHook(c, log.WithName("hook-virt"))
	r.hooks["cilium"] = NewCiliumHook(c, log.WithName("hook-cilium"))
	r.hooks["headlamp"] = NewHeadlampHook(c, log.WithName("hook-headlamp"))
	r.hooks["ingress-nginx"] = NewIngressNginxHook(c, log.WithName("hook-ingress-nginx"))
	r.hooks["metallb"] = NewMetalLBHook(c, log.WithName("hook-metallb"))
	r.hooks["topolvm"] = NewTopolvmHook(c, log.WithName("hook-topolvm"))
	r.hooks["calico"] = NewCalicoHook(c, log.WithName("hook-calico"))
	return r
}

// Get returns the hook for a component, or nil if none exists.
func (r *Registry) Get(componentName string) Hook {
	return r.hooks[componentName]
}
