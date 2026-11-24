/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterConfigSpec defines the desired state of the k4all cluster
type ClusterConfigSpec struct {
	// Networking configuration
	Networking NetworkingConfig `json:"networking"`
	// Features to enable
	Features FeaturesConfig `json:"features"`
	// Control plane / HA configuration
	Cluster ControlPlaneConfig `json:"cluster"`
	// Ingress controller configuration
	// +optional
	Ingress IngressConfig `json:"ingress,omitempty"`
	// Proxy configuration for cluster-wide HTTP(S) proxy settings.
	// Pre-boot scripts read these before the cluster starts; the operator
	// also exposes them for in-cluster components that need proxy awareness.
	// +optional
	Proxy *ProxyConfig `json:"proxy,omitempty"`
	// Per-component value overrides (component name -> Helm values or custom config).
	// These override values from ReleaseManifest for the named component.
	// +optional
	ComponentOverrides map[string]ComponentOverride `json:"componentOverrides,omitempty"`
	// Node settings k4all owns, the same on every node: kubelet settings,
	// hugepages, IOMMU. The operator rolls a change out one node at a time.
	// Absent, or a field of it absent: k4all does not own that setting and
	// leaves what kubeadm or the admin set. Clearing a field removes what
	// k4all had written for it, and only that.
	// +optional
	Nodes *NodesConfig `json:"nodes,omitempty"`
}

// NodesConfig is the node settings k4all owns. Every field is optional and
// absent means "not owned by k4all": k4all writes nothing for it.
type NodesConfig struct {
	// Kubelet settings, written to kube-system/kubelet-config (the
	// KubeletConfiguration kubeadm gives every node, joining nodes too).
	// Fields left out keep the value kubeadm or the admin set there.
	// +optional
	Kubelet *KubeletSettings `json:"kubelet,omitempty"`
	// Hugepages to reserve on every node, as kernel arguments: page size
	// ("1Gi" or "2Mi") to the number of pages per node; 0 reserves none. A
	// change reboots the nodes. A size left out is not owned. k4all removes
	// only the kernel arguments it added, never ones set by hand.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.all(size, size == '1Gi' || size == '2Mi')",message="hugepages page sizes are 1Gi and 2Mi"
	// +kubebuilder:validation:XValidation:rule="self.all(size, self[size] >= 0)",message="hugepage counts are 0 or more"
	Hugepages map[string]int32 `json:"hugepages,omitempty"`
	// IOMMU in passthrough mode (intel_iommu=on or amd_iommu=on, iommu=pt) as
	// kernel arguments, for PCI passthrough to VMs. A change reboots the nodes.
	// true: k4all adds them. false or absent: k4all adds none and removes the
	// ones it had added; arguments set by hand are never touched.
	// +optional
	IOMMU *bool `json:"iommu,omitempty"`
	// How the operator rolls a change of these settings out.
	// +optional
	Rollout *NodeRollout `json:"rollout,omitempty"`
}

// KubeletSettings is the part of the KubeletConfiguration k4all owns. A field
// left out is not owned: k4all does not write it, and removes it from
// kubelet-config only if k4all had written it.
// +kubebuilder:validation:XValidation:rule="!has(self.cpuManagerPolicy) || self.cpuManagerPolicy != 'static' || (has(self.reservedSystemCPUs) && size(self.reservedSystemCPUs) > 0)",message="the static CPU manager needs reservedSystemCPUs"
type KubeletSettings struct {
	// Maximum number of pods per node. It must fit the pod CIDR of each node
	// (at most 250 with Cilium's /24). Changing only this restarts kubelet,
	// without a drain.
	// +optional
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=1000
	MaxPods *int32 `json:"maxPods,omitempty"`
	// Kubelet CPU manager policy. static gives Guaranteed pods with integer
	// CPU requests exclusive CPUs, and needs reservedSystemCPUs. A change
	// drains each node and resets its CPU manager state.
	// +optional
	// +kubebuilder:validation:Enum=none;static
	CPUManagerPolicy string `json:"cpuManagerPolicy,omitempty"`
	// CPUs kept for the system and kubelet, as a cpuset ("0-1", "0,2"): no pod
	// gets them exclusively. Required with the static CPU manager. Every node
	// must have them. A change drains each node and resets its CPU manager
	// state.
	// +optional
	ReservedSystemCPUs string `json:"reservedSystemCPUs,omitempty"`
	// Kubelet topology manager policy: how CPU, memory and device allocations
	// are aligned on NUMA nodes.
	// +optional
	// +kubebuilder:validation:Enum=none;best-effort;restricted;single-numa-node
	TopologyManagerPolicy string `json:"topologyManagerPolicy,omitempty"`
	// Kubelet topology manager scope: align each container, or the whole pod.
	// +optional
	// +kubebuilder:validation:Enum=container;pod
	TopologyManagerScope string `json:"topologyManagerScope,omitempty"`
}

// NodeRollout controls the rollout of node settings.
type NodeRollout struct {
	// Stop the rollout before the next node; the node in progress finishes.
	// false or absent: the rollout goes on, one node at a time.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// IngressConfig defines the desired ingress controller settings.
type IngressConfig struct {
	// Deprecated since k4all v2.6.0: use ingress.traefik. From v2.6.0 the
	// operator moves these settings to ingress.traefik and removes this key.
	// +optional
	Nginx *NginxIngressConfig `json:"nginx,omitempty"`
	// +optional
	Cilium CiliumIngressConfig `json:"cilium,omitempty"`
	// Traefik replaces NGINX from k4all v2.6.0. When absent, Traefik takes the
	// nginx settings, so clusters installed earlier keep their IP and default.
	// +optional
	Traefik *TraefikIngressConfig `json:"traefik,omitempty"`
}

// NginxIngressConfig configures the NGINX ingress controller (k4all before v2.6.0).
type NginxIngressConfig struct {
	// Dedicated LoadBalancer IP; leave empty to use the node IP directly
	// +optional
	DedicatedIP string `json:"dedicatedIP,omitempty"`
	// Whether NGINX should be the default IngressClass. Left out, it is.
	// +optional
	// +kubebuilder:default=true
	IsDefault bool `json:"isDefault"` // no omitempty: a Go client must send false, or the default turns it true
}

// TraefikIngressConfig configures the Traefik ingress controller.
type TraefikIngressConfig struct {
	// Dedicated LoadBalancer IP; leave empty to use the node IP directly
	// +optional
	DedicatedIP string `json:"dedicatedIP,omitempty"`
	// Whether Traefik should be the default IngressClass. Left out, it is.
	// +optional
	// +kubebuilder:default=true
	IsDefault bool `json:"isDefault"` // no omitempty: a Go client must send false, or the default turns it true
}

// CiliumIngressConfig configures the Cilium ingress.
type CiliumIngressConfig struct {
	// Dedicated LoadBalancer IP for Cilium ingress
	// +optional
	DedicatedIP string `json:"dedicatedIP,omitempty"`
}

// ProxyConfig holds cluster-wide HTTP(S) proxy settings.
type ProxyConfig struct {
	// +optional
	HTTPProxy string `json:"httpProxy,omitempty"`
	// +optional
	HTTPSProxy string `json:"httpsProxy,omitempty"`
	// +optional
	NoProxy string `json:"noProxy,omitempty"`
}

// ComponentOverride allows per-component value overrides in ClusterConfig
type ComponentOverride struct {
	// Inline Helm values to merge on top of ReleaseManifest values
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
	// Extra Helm arguments to append
	// +optional
	HelmArgs []string `json:"helmArgs,omitempty"`
}

type NetworkingConfig struct {
	// CNI plugin selection
	CNI CNIConfig `json:"cni"`
	// Firewalld settings
	// +optional
	Firewalld FirewalldConfig `json:"firewalld,omitempty"`
}

type CNIConfig struct {
	// +kubebuilder:validation:Enum=calico;cilium
	Type string `json:"type"`
	// Cilium options, used when type is cilium. The operator translates them
	// into Cilium Helm values; componentOverrides.cilium can still override any value.
	// +optional
	Cilium CiliumConfig `json:"cilium,omitempty"`
}

// CiliumConfig holds the Cilium options exposed by the k4all installer.
type CiliumConfig struct {
	// Interfaces Cilium attaches to, in addition to ovs-bridge when the OVS
	// bridge feature is enabled.
	// +optional
	AdditionalDevices []string `json:"additionalDevices,omitempty"`
	// Announce LoadBalancer IPs on the local network (L2 announcements).
	// +optional
	L2Announcements bool `json:"l2announcements,omitempty"`
	// Deploy Hubble Relay and the Hubble UI.
	// +optional
	Hubble bool `json:"hubble,omitempty"`
	// Enable Cilium's Gateway API implementation.
	// +optional
	GatewayAPI bool `json:"gatewayAPI,omitempty"`
}

type FirewalldConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

type FeaturesConfig struct {
	// Virtualization feature (KubeVirt + CDI + the Hangar console)
	// +optional
	Virt VirtConfig `json:"virt,omitempty"`
	// Rook/Ceph storage, with the Cairn console. Optional: absent or not
	// enabled, k4all installs no Rook.
	// +optional
	Rook RookConfig `json:"rook,omitempty"`
	// ArgoCD GitOps feature
	// +optional
	ArgoCD ArgoCDConfig `json:"argocd,omitempty"`
	// OVS networking with Multus + OVS-CNI
	// +optional
	OVSCNI OVSCNIConfig `json:"ovsCni,omitempty"`
	// OVS bridge creation via nmstate NNCP on every node
	// +optional
	OVSBridge OVSBridgeConfig `json:"ovsBridge,omitempty"`
}

type VirtConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Emulation mode, set in the KubeVirt CR as useEmulation (the operator puts
	// back a value changed there by hand): "true", "false", or "auto". With
	// "auto" the nodes decide, from the devices.kubevirt.io/kvm that
	// virt-handler offers where /dev/kvm exists: emulation is off once a node
	// offers KVM, on once virt-handler runs and none does.
	// +kubebuilder:validation:Enum="true";"false";auto
	// +kubebuilder:default="auto"
	Emulation string `json:"emulation,omitempty"`
	// KubeVirt settings k4all owns in the KubeVirt CR. Absent: k4all owns none
	// of them; Hangar or the admin set them in the KubeVirt CR.
	// +optional
	KubeVirt *KubeVirtSettings `json:"kubevirt,omitempty"`
	// Settings of the Hangar console, which k4all installs with virt.
	// Absent: the release's defaults.
	// +optional
	Hangar *HangarSettings `json:"hangar,omitempty"`
	// The metadata service VMs reach on 169.254.169.254 (OpenStack and EC2
	// formats, workload-identity tokens). Absent: off, as enabled false.
	// +optional
	Metadata *MetadataSettings `json:"metadata,omitempty"`
}

// KubeVirtSettings is what k4all owns in the KubeVirt CR's
// developerConfiguration and spec. A field left out is not owned: k4all
// writes nothing for it, and removes only what it had written itself.
// +kubebuilder:validation:XValidation:rule="!has(self.featureGates) || !has(self.disabledFeatureGates) || !self.featureGates.exists(g, g in self.disabledFeatureGates)",message="a gate cannot be both on and off"
// +kubebuilder:validation:XValidation:rule="!has(self.featureGates) || !('HotplugVolumes' in self.featureGates && 'DeclarativeHotplugVolumes' in self.featureGates)",message="HotplugVolumes turns DeclarativeHotplugVolumes off: set one"
type KubeVirtSettings struct {
	// Feature gates k4all turns on. They are added to the gates already in the
	// KubeVirt CR (set by Hangar or the admin), which stay; a gate removed
	// from this list is removed from the CR only if k4all had added it.
	// HotplugVolumes turns DeclarativeHotplugVolumes off: list one of them.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	FeatureGates []string `json:"featureGates,omitempty"`
	// Feature gates k4all turns off: removed from featureGates and, on
	// KubeVirt 1.8 and later, written to disabledFeatureGates. A gate cannot
	// be in both lists. KubeVirt ignores a GA gate here; the operator reports
	// it in the KubeVirtSettings condition where KubeVirt does.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	DisabledFeatureGates []string `json:"disabledFeatureGates,omitempty"`
	// StorageClass of the persistent VM state (EFI variables, TPM), as the
	// KubeVirt CR's vmStateStorageClass. Absent: not owned; the value set in
	// the KubeVirt CR applies, and without one the default StorageClass. A
	// StorageClass that does not exist is still written, and reported in the
	// KubeVirtSettings condition.
	// +optional
	VMStateStorageClass string `json:"vmStateStorageClass,omitempty"`
}

// HangarSettings configures the Hangar console k4all installs with virt.
type HangarSettings struct {
	// Install Hangar's privileged node agent (host inventory, device
	// passthrough), needed by Hangar's Hardware tab and PCI passthrough.
	// Absent: the release's default, which installs it with virt.
	// +optional
	NodeAgent *bool `json:"nodeAgent,omitempty"`
}

// Values of MetadataSettings.Scope.
const (
	// MetadataScopeLabelled serves the VMs whose template has the label
	// k4all.magesgate.com/metadata: enabled. An empty scope means this.
	MetadataScopeLabelled = "Labelled"
	// MetadataScopeAll serves every VM in the namespaces whose PodSecurity
	// level allows it.
	MetadataScopeAll = "All"
)

// MetadataSettings configures the metadata service on 169.254.169.254: one
// server in each VM's virt-launcher pod, from the VM's own spec.
type MetadataSettings struct {
	// Serve metadata to the VMs of the scope. false or absent: off.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Which VMs get the service: Labelled (the VM template has the label
	// k4all.magesgate.com/metadata: enabled) or All (every VM in the
	// namespaces whose PodSecurity level allows it). Empty means Labelled.
	// +optional
	// +kubebuilder:validation:Enum=Labelled;All
	Scope string `json:"scope,omitempty"`
	// The workload-identity tokens (service account tokens) served to the
	// guest. Absent: the operator's defaults.
	// +optional
	Tokens *MetadataTokens `json:"tokens,omitempty"`
	// Name of a ConfigMap in each VM's namespace with the cloud-init
	// vendor-data served to its VMs; a namespace without it serves none.
	// Absent: no vendor-data.
	// +optional
	VendorDataConfigMap string `json:"vendorDataConfigMap,omitempty"`
}

// MetadataTokens configures the service account tokens served to the guest.
type MetadataTokens struct {
	// Audiences of the tokens. Never the API server's own audience: a token
	// taken from a guest must not be able to call the API server. Absent: the
	// operator's default.
	// +optional
	// +listType=set
	Audiences []string `json:"audiences,omitempty"`
	// Lifetime of a token, in seconds; at least 600, kubelet's minimum for a
	// projected token. Absent: the operator's default.
	// +optional
	// +kubebuilder:validation:Minimum=600
	ExpirationSeconds *int64 `json:"expirationSeconds,omitempty"`
}

// Values of RookConfig.SmallCluster.
const (
	// RookSmallClusterRefuse creates nothing below 3 nodes, and the status
	// says why. An empty smallCluster means this.
	RookSmallClusterRefuse = "Refuse"
	// RookSmallClusterAllow creates the cluster below 3 nodes: one mon, pool
	// size the node count, no redundancy.
	RookSmallClusterAllow = "Allow"
)

// RookConfig configures the optional Rook/Ceph storage. k4all creates the
// CephCluster, its pool and StorageClass once; from then on they belong to
// the Cairn console, and a change to storage here does not reach them.
type RookConfig struct {
	// Install Rook and create the Ceph cluster. Turning it off does not remove
	// Rook while a CephCluster exists, and k4all never wipes disks.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Which disks become OSDs, read once, when k4all creates the CephCluster.
	// Absent: every blank device of every node (a device with a filesystem
	// or an LVM signature, as the OS disk and TopoLVM's, is skipped).
	// +optional
	Storage *RookStorage `json:"storage,omitempty"`
	// What to do below 3 nodes: Refuse (nothing is created, the status says
	// why) or Allow (one mon, pool size the node count, no redundancy).
	// Empty means Refuse.
	// +optional
	// +kubebuilder:validation:Enum=Refuse;Allow
	SmallCluster string `json:"smallCluster,omitempty"`
}

// RookStorage selects the disks of the CephCluster k4all creates.
type RookStorage struct {
	// Regular expression on device names ("^sdb$", "^nvme[01]n1$"), as
	// Rook's storage.deviceFilter. Absent: every blank device.
	// +optional
	DeviceFilter string `json:"deviceFilter,omitempty"`
}

type ArgoCDConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

type OVSCNIConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

type OVSBridgeConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

type ControlPlaneConfig struct {
	// Use hostname as API endpoint instead of IP
	ApiEndPointUseHostName bool `json:"apiEndPointUseHostName,omitempty"`
	// Custom hostname for the control plane API endpoint
	// +optional
	CustomApiEndPoint string `json:"customApiEndPoint,omitempty"`
	// HA configuration
	// +optional
	HA HAConfig `json:"ha,omitempty"`
	// Pod network CIDR (default: "10.100.0.1/18")
	// +optional
	PodNetwork string `json:"podNetwork,omitempty"`
	// Service network CIDR (default: "10.96.0.0/16")
	// +optional
	ServiceNetwork string `json:"serviceNetwork,omitempty"`
}

type HAConfig struct {
	// Network interface for the virtual IP
	// +optional
	Interface string `json:"interface,omitempty"`
	// +kubebuilder:validation:Enum=none;keepalived;kubevip
	// +kubebuilder:default=none
	Type string `json:"type,omitempty"`
	// Virtual IP address for the control plane
	// +optional
	ApiControlEndpoint string `json:"apiControlEndpoint,omitempty"`
	// Subnet size for the virtual IP (e.g. "24")
	// +optional
	ApiControlEndpointSubnetSize string `json:"apiControlEndpointSubnetSize,omitempty"`
}

// ClusterConfigStatus reflects the observed cluster state
type ClusterConfigStatus struct {
	// Standard conditions (Reconciled, Degraded)
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Most recently processed generation of the spec
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Currently active CNI
	// +optional
	ActiveCNI string `json:"activeCNI,omitempty"`
	// Per-feature observed state
	// +optional
	Features FeaturesStatus `json:"features,omitempty"`
	// Last time the config was reconciled
	// +optional
	LastReconcileTime metav1.Time `json:"lastReconcileTime,omitempty"`
	// Rollout of spec.nodes, written by the operator. Absent until spec.nodes
	// sets something.
	// +optional
	Nodes *NodesStatus `json:"nodes,omitempty"`
}

// ClusterConfig condition types, besides Reconciled and Degraded.
const (
	// ConditionPodNetworkOverlap is True when Cilium's pod addresses (its
	// pool, the nodes' pod CIDRs) overlap a network they must stay clear of;
	// the message lists what overlaps. It only reports.
	ConditionPodNetworkOverlap = "PodNetworkOverlap"
	// ConditionKubeVirtSettings reports spec.features.virt.kubevirt in the
	// KubeVirt CR: refused gates, a vmStateStorageClass that does not exist.
	ConditionKubeVirtSettings = "KubeVirtSettings"
	// ConditionNodeSettingsRollout reports the rollout of spec.nodes.
	ConditionNodeSettingsRollout = "NodeSettingsRollout"
	// ConditionRookReady reports the Rook/Ceph cluster of spec.features.rook,
	// or why k4all did not create it.
	ConditionRookReady = "RookReady"
	// ConditionMetadataService reports the metadata service of
	// spec.features.virt.metadata.
	ConditionMetadataService = "MetadataService"
)

// Values of NodeRolloutStatus.Phase.
const (
	// NodeRolloutIdle: every node has the settings, nothing to roll out.
	NodeRolloutIdle = "Idle"
	// NodeRolloutRolling: a node is being given the settings.
	NodeRolloutRolling = "Rolling"
	// NodeRolloutPaused: spec.nodes.rollout.paused stopped it between nodes.
	NodeRolloutPaused = "Paused"
	// NodeRolloutBlocked: the rollout cannot go on (a ClusterUpgrade runs, a
	// node refuses the settings); the message says why.
	NodeRolloutBlocked = "Blocked"
)

// NodesStatus is the rollout state of spec.nodes.
type NodesStatus struct {
	// Hash of the node settings the nodes must have (spec.nodes and the
	// action they take: restart, drain and restart, drain and reboot).
	// +optional
	SettingsHash string `json:"settingsHash,omitempty"`
	// The rollout across the nodes.
	// +optional
	Rollout NodeRolloutStatus `json:"rollout,omitempty"`
	// One entry per node.
	// +optional
	// +listType=map
	// +listMapKey=name
	PerNode []NodeSettingsStatus `json:"perNode,omitempty"`
}

// NodeRolloutStatus is where the rollout of the node settings is.
type NodeRolloutStatus struct {
	// Idle, Rolling, Paused or Blocked.
	// +optional
	Phase string `json:"phase,omitempty"`
	// The node being given the settings, while Rolling.
	// +optional
	CurrentNode string `json:"currentNode,omitempty"`
	// What the rollout does or waits for, in words.
	// +optional
	Message string `json:"message,omitempty"`
}

// NodeSettingsStatus is the node settings state of one node.
type NodeSettingsStatus struct {
	// Node name.
	Name string `json:"name"`
	// Settings hash the node last reported applied (its
	// k4all.magesgate.com/node-settings-applied annotation).
	// +optional
	AppliedHash string `json:"appliedHash,omitempty"`
	// Where the node is in its rollout step.
	// +optional
	Phase string `json:"phase,omitempty"`
	// What the node does or waits for, or why it refuses the settings.
	// +optional
	Message string `json:"message,omitempty"`
}

// FeaturesStatus reports the observed state of each feature toggle.
type FeaturesStatus struct {
	// +optional
	VirtReady bool `json:"virtReady,omitempty"`
	// +optional
	ArgoCDReady bool `json:"argocdReady,omitempty"`
	// +optional
	OVSCNIReady bool `json:"ovsCniReady,omitempty"`
	// +optional
	OVSBridgeReady bool `json:"ovsBridgeReady,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="CNI",type="string",JSONPath=".spec.networking.cni.type"
// +kubebuilder:printcolumn:name="Virt",type="boolean",JSONPath=".spec.features.virt.enabled"
// +kubebuilder:printcolumn:name="ArgoCD",type="boolean",JSONPath=".spec.features.argocd.enabled"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ClusterConfig is the Schema for the clusterconfigs API.
// It defines user intent for which components and features to enable.
type ClusterConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterConfigSpec   `json:"spec,omitempty"`
	Status ClusterConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterConfigList contains a list of ClusterConfig
type ClusterConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterConfig{}, &ClusterConfigList{})
}
