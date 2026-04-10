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

// NodeConfigSpec defines per-node desired state.
// Network interface configuration is now handled by Anaconda/NetworkManager
// and OVS bridge creation by the nmstate NNCP feature.
type NodeConfigSpec struct {
}

// NodeConfigStatus reflects observed node state
type NodeConfigStatus struct {
	// Current network interface status
	// +optional
	NetworkInterface NetworkInterfaceStatus `json:"networkInterface,omitempty"`
	// Interfaces discovered on the node
	// +optional
	AvailableInterfaces []AvailableInterface `json:"availableInterfaces,omitempty"`
	// Kubernetes node name
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// Node role (bootstrap, control, worker)
	// +optional
	NodeType string `json:"nodeType,omitempty"`
}

type AvailableInterface struct {
	Name string `json:"name"`
	Mac  string `json:"mac"`
}

type NetworkInterfaceStatus struct {
	// +kubebuilder:validation:Enum=Configuring;Ready;Error
	State   string `json:"state,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	// +optional
	LastUpdateTime metav1.Time `json:"lastUpdateTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".status.nodeName"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.networkInterface.state"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".status.nodeType"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NodeConfig is the Schema for the nodeconfigs API
type NodeConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeConfigSpec   `json:"spec,omitempty"`
	Status NodeConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeConfigList contains a list of NodeConfig
type NodeConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeConfig{}, &NodeConfigList{})
}
