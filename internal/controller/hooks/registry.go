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
// next reconcile within that time, also with an error. Sync redoes all the
// hook's post-install work: its error goes into the component's message like
// a PostInstall one, and a success clears such a message.
type Syncer interface {
	Sync(ctx context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) (time.Duration, error)
}

// ValuesRevisioner is implemented by a hook whose PreInstall values, or the
// objects the apply writes (KubeVirt, CDI), change between operator versions
// for the same ClusterConfig. The reconciler adds the revision to the
// component's appliedHash, so a cluster re-applies the component once after
// the operator upgrade instead of keeping what the previous operator applied.
type ValuesRevisioner interface {
	ValuesRevision() string
}

// UninstallGuard is implemented by a hook whose component must not be
// uninstalled while something still needs it (Rook: a CephCluster holds
// data, and without the operator its daemons and finalizers are stranded).
// The reconciler asks before every uninstall, whichever the cause: the
// component's feature turned off, a CNI switch, management Removed. A
// non-empty reason refuses it: the component stays installed, its status
// says why, and the reconciler asks again at every reconcile. An error
// refuses it too: the hook cannot tell that it is safe.
type UninstallGuard interface {
	UninstallBlocked(ctx context.Context, name string) (reason string, err error)
}

// UninstallPreparer is implemented by a hook whose component's operator must
// uninstall what it created before k4all deletes the operator: KubeVirt and
// CDI create CRDs, webhooks and APIServices that are in no manifest, and
// only their operator removes them, when its CR is deleted. The reconciler
// calls PrepareUninstall once the UninstallGuard (if any) lets the uninstall
// start, and at every reconcile after that until it is done:
//   - "" and no error: the operator is done; the engine's uninstall and the
//     Cleanup hook follow, as for any component;
//   - a pending message: the component stays Uninstalling with it, and the
//     reconciler comes back soon; past uninstallWait it is Failed, still
//     with its operator;
//   - an error: the component is Failed, its operator kept, and the next
//     reconcile asks again.
//
// It must not block: it starts the operator's uninstall (deletes the CR) and
// reports how far it got.
type UninstallPreparer interface {
	PrepareUninstall(ctx context.Context, name string) (pending string, err error)
}

// Remover is implemented by a hook that removes its component itself: once
// the UninstallGuard and the UninstallPreparer (if any) let the uninstall go
// on, the reconciler calls Remove instead of deleting every object the
// component's manifests list (kubevirt-manager: VMs name its
// PriorityClasses, and its CRD holds the user's objects). Cleanup still runs
// after it.
type Remover interface {
	Remove(ctx context.Context, spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec) error
}

// OverrideMerger is implemented by a hook that layers its component's
// ClusterConfig componentOverride on the PreInstall values itself, instead
// of the plain deep merge, where a list of the override replaces the
// release's list (headlamp: plugin lists merge by name). The reconciler calls
// it on every apply, with an empty override when the ClusterConfig has none.
type OverrideMerger interface {
	MergeOverride(spec k4allv1alpha1.ComponentSpec, config k4allv1alpha1.ClusterConfigSpec,
		values map[string]interface{}, override k4allv1alpha1.ComponentOverride) (map[string]interface{}, error)
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
	kvm := NewKubevirtManagerHook(c, log.WithName("hook-kubevirt-manager"))
	kvm.restConfig = o.restConfig
	r.hooks["kubevirt-manager"] = kvm
	r.hooks["rook-ceph"] = NewRookHook(c, log.WithName("hook-rook"))
	r.hooks["ceph-csi-drivers"] = NewCephCSIDriversHook(c)
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
