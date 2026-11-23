package hooks

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VirtHook keeps KubeVirt's software emulation in line with the ClusterConfig
// (Sync, virt_emulation.go) and reports the KubeVirt version that runs.
type VirtHook struct {
	BaseHook
	client client.Client
	log    logr.Logger
	now    func() time.Time

	// streak is what the nodes have reported about KVM over consecutive
	// Syncs, and since when. It lives in memory: an operator restart starts
	// it again, which only delays a change.
	mu     sync.Mutex
	streak kvmStreak
}

func NewVirtHook(c client.Client, log logr.Logger) *VirtHook {
	return &VirtHook{client: c, log: log, now: time.Now}
}

var kubeVirtKey = types.NamespacedName{Name: "kubevirt", Namespace: "kubevirt"}

const kubeVirtAPIVersion, kubeVirtKind = "kubevirt.io/v1", "KubeVirt"

// RunningVersion reads the KubeVirt CR: virt-operator reports there the
// version it runs and the one it is rolling out.
func (h *VirtHook) RunningVersion(ctx context.Context) (string, bool, error) {
	return operatorCRVersion(ctx, h.client, kubeVirtAPIVersion, kubeVirtKind, kubeVirtKey,
		"observedKubeVirtVersion", "targetKubeVirtVersion")
}
