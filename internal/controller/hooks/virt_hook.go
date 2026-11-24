package hooks

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VirtHook folds the ClusterConfig's KubeVirt settings into the KubeVirt CR
// at its apply (ManifestMutator, virt_settings.go), keeps KubeVirt's
// software emulation in line with the ClusterConfig (Sync, virt_emulation.go)
// and reports the KubeVirt version that runs.
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

// virtApplyRevision changes whenever the KubeVirt and CDI CRs are applied
// otherwise for the same ClusterConfig. 1: with spec.uninstallStrategy
// BlockUninstallIfWorkloadsExist (k4all-operator v0.5.7). CDI's CR is still
// applied as v0.5.7 applied it.
const virtApplyRevision = "1"

// kubeVirtApplyRevision is virtApplyRevision for the KubeVirt CR alone. 2:
// without upstream's empty fields, and with the features.virt.kubevirt
// settings and their gate merge (k4all-operator v0.6.0). With no settings,
// the re-apply changes no field KubeVirt reads: virt-operator does not roll
// its infra for it.
const kubeVirtApplyRevision = "2"

// ValuesRevision implements ValuesRevisioner: every cluster applies
// KubeVirt again once after the operator upgrade.
func (h *VirtHook) ValuesRevision() string { return kubeVirtApplyRevision }

const kubeVirtAPIVersion, kubeVirtKind = "kubevirt.io/v1", "KubeVirt"

// RunningVersion reads the KubeVirt CR: virt-operator reports there the
// version it runs and the one it is rolling out.
func (h *VirtHook) RunningVersion(ctx context.Context) (string, bool, error) {
	return operatorCRVersion(ctx, h.client, kubeVirtAPIVersion, kubeVirtKind, kubeVirtKey,
		"observedKubeVirtVersion", "targetKubeVirtVersion")
}
