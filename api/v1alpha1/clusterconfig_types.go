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
}

// IngressConfig defines the desired ingress controller settings.
type IngressConfig struct {
	// +optional
	Nginx NginxIngressConfig `json:"nginx,omitempty"`
	// +optional
	Cilium CiliumIngressConfig `json:"cilium,omitempty"`
}

// NginxIngressConfig configures the NGINX ingress controller.
type NginxIngressConfig struct {
	// Dedicated LoadBalancer IP; leave empty to use the node IP directly
	// +optional
	DedicatedIP string `json:"dedicatedIP,omitempty"`
	// Whether NGINX should be the default IngressClass
	// +optional
	IsDefault bool `json:"isDefault,omitempty"`
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
}

type FirewalldConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

type FeaturesConfig struct {
	// Virtualization feature (KubeVirt + CDI + kubevirt-manager)
	// +optional
	Virt VirtConfig `json:"virt,omitempty"`
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
	// Emulation mode: "true", "false", or "auto" (detect /dev/kvm)
	// +kubebuilder:validation:Enum="true";"false";auto
	// +kubebuilder:default="auto"
	Emulation string `json:"emulation,omitempty"`
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
