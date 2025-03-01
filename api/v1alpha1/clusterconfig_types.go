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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterConfigSpec defines the desired state of ClusterConfig
type ClusterConfigSpec struct {
	// Version of the configuration
	Version string `json:"version"`
	// Networking configuration
	Networking NetworkingConfig `json:"networking"`
	// Features configuration
	Features FeaturesConfig `json:"features"`
	// Cluster configuration
	Cluster ControlPlaneConfig `json:"cluster"`
}

// NetworkingConfig defines the networking configuration
type NetworkingConfig struct {
	// CNI configuration
	CNI CNIConfig `json:"cni"`
	// Firewalld configuration
	Firewalld FirewalldConfig `json:"firewalld"`
	// Interface configuration
	Interface InterfaceConfig `json:"iface"`
}

// CNIConfig defines the CNI configuration
type CNIConfig struct {
	// Type of CNI (calico, cilium)
	Type string `json:"type"`
}

// FirewalldConfig defines the Firewalld configuration
type FirewalldConfig struct {
	// Enabled indicates if firewalld is enabled
	Enabled string `json:"enabled"`
}

// InterfaceConfig defines the interface configuration
type InterfaceConfig struct {
	// Device name (eth card)
	Dev string `json:"dev"`
	// IP configuration (dhcp or static)
	IpConfig string `json:"ipconfig"`
	// IP address (required for static IP)
	IpAddr string `json:"ipaddr,omitempty"`
	// Gateway (required for static IP)
	Gateway string `json:"gateway,omitempty"`
	// Subnet mask (required for static IP)
	SubnetMask string `json:"subnet_mask,omitempty"`
	// DNS servers (comma-separated list)
	DNS string `json:"dns,omitempty"`
	// DNS search domains (comma-separated list)
	DNSSearch string `json:"dns_search,omitempty"`
}

// FeaturesConfig defines the features configuration
type FeaturesConfig struct {
	// Virt configuration
	Virt VirtConfig `json:"virt"`
	// ArgoCD configuration
	ArgoCD ArgoCDConfig `json:"argocd"`
}

type VirtConfig struct {
	// Enabled indicates if virtualization is enabled
	Enabled bool `json:"enabled"` // Changed to bool
	// Managed indicates if virtualization is managed by the operator
	Managed bool `json:"managed,omitempty"`
	// Emulation indicates if emulation is enabled (true, false, or auto)
	Emulation string `json:"emulation"`
}

type ArgoCDConfig struct {
	// Enabled indicates if ArgoCD is enabled
	Enabled bool `json:"enabled"` // Changed to bool
	// Managed indicates if ArgoCD is managed by the operator
	Managed bool `json:"managed,omitempty"`
}

// ControlPlaneConfig defines the cluster configuration
type ControlPlaneConfig struct {
	// ApiEndPointUseHostName indicates if the API endpoint should use the hostname
	ApiEndPointUseHostName string `json:"apiEndPointUseHostName"`
	// CustomApiEndPoint is a custom hostname for the control plane
	CustomApiEndPoint string `json:"customApiEndPoint,omitempty"`
	// HA configuration
	HA HAConfig `json:"ha"`
}

// HAConfig defines the HA configuration
type HAConfig struct {
	// Interface to use for the virtual IP
	Interface string `json:"interface"`
	// Type of HA (none, keepalived, kubevip)
	Type string `json:"type"`
	// Control plane endpoint for the API server (ignored in "none" mode)
	ApiControlEndpoint string `json:"apiControlEndpoint,omitempty"`
	// Control plane endpoint subnet size for the API server (ignored in "none" mode)
	ApiControlEndpointSubnetSize string `json:"apiControlEndpointSubnetSize,omitempty"`
}

// ClusterConfigStatus defines the observed state of ClusterConfig
type ClusterConfigStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// ClusterConfig is the Schema for the clusterconfigs API
type ClusterConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterConfigSpec   `json:"spec,omitempty"`
	Status ClusterConfigStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ClusterConfigList contains a list of ClusterConfig
type ClusterConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterConfig{}, &ClusterConfigList{})
}
