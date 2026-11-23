package hooks

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubevirtManagerHook keeps kubevirt-manager installed while VMs exist
// (UninstallBlocked, virt_workloads.go). Its uninstall deletes the
// PriorityClasses vm-standard and vm-preemptible, and its VM form puts
// vm-standard in the VMs it makes: such a VM keeps running, but can no
// longer start or move to another host, its new pod is refused. It also
// deletes the CRD of its image catalog, the user's Images with it.
type KubevirtManagerHook struct {
	BaseHook
	client client.Client
}

func NewKubevirtManagerHook(c client.Client) *KubevirtManagerHook {
	return &KubevirtManagerHook{client: c}
}
