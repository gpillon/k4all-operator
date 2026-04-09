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

// +kubebuilder:validation:Enum=helm;"helm-oci";manifest;binary;"static-pod"
type ComponentType string

const (
	ComponentTypeHelm      ComponentType = "helm"
	ComponentTypeHelmOCI   ComponentType = "helm-oci"
	ComponentTypeManifest  ComponentType = "manifest"
	ComponentTypeBinary    ComponentType = "binary"
	ComponentTypeStaticPod ComponentType = "static-pod"
)

// +kubebuilder:validation:Enum=merge;strategic;json
type PatchType string

const (
	PatchTypeMerge     PatchType = "merge"
	PatchTypeStrategic PatchType = "strategic"
	PatchTypeJSON      PatchType = "json"
)

type ComponentState string

const (
	ComponentStatePending      ComponentState = "Pending"
	ComponentStateInstalling   ComponentState = "Installing"
	ComponentStateInstalled    ComponentState = "Installed"
	ComponentStateUpgrading    ComponentState = "Upgrading"
	ComponentStateFailed       ComponentState = "Failed"
	ComponentStateUninstalling ComponentState = "Uninstalling"
	ComponentStateSkipped      ComponentState = "Skipped"
	ComponentStateRemoved      ComponentState = "Removed"
	ComponentStateUnmanaged    ComponentState = "Unmanaged"
)

// +kubebuilder:validation:Enum=Managed;Removed;Unmanaged
type ComponentManagement string

const (
	ComponentManagementManaged   ComponentManagement = "Managed"
	ComponentManagementRemoved   ComponentManagement = "Removed"
	ComponentManagementUnmanaged ComponentManagement = "Unmanaged"
)

// ReleaseManifestSpec mirrors the k4all-release.yaml structure
type ReleaseManifestSpec struct {
	// OS image specification
	OSImage OSImageSpec `json:"osImage,omitempty"`
	// Components to install, keyed by component name
	Components map[string]ComponentSpec `json:"components"`
}

type OSImageSpec struct {
	Name       string `json:"name,omitempty"`
	Repository string `json:"repository,omitempty"`
	ImageTag   string `json:"imageTag,omitempty"`
}

type ComponentSpec struct {
	// Type of component installation strategy
	Type ComponentType `json:"type"`
	// Lifecycle management policy: Managed (default), Removed, or Unmanaged
	// +kubebuilder:default=Managed
	// +optional
	Management ComponentManagement `json:"management,omitempty"`
	// Desired version; "latest" triggers resolution via VersionURL
	Version string `json:"version"`
	// URL to fetch the latest version string (for "latest" resolution)
	// +optional
	VersionURL string `json:"versionUrl,omitempty"`
	// Helm repository URL (for type helm) or OCI reference (for type helm-oci)
	// +optional
	Repo string `json:"repo,omitempty"`
	// Helm chart name
	// +optional
	Chart string `json:"chart,omitempty"`
	// Single manifest URL (supports {{ version }} and {{ arch }} templates)
	// +optional
	Source string `json:"source,omitempty"`
	// Multiple manifest URLs, applied in order
	// +optional
	Sources []string `json:"sources,omitempty"`
	// Target namespace for the component
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Inline Helm values (arbitrary YAML)
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
	// References to ConfigMaps containing Helm values
	// +optional
	ValuesFrom []ValuesReference `json:"valuesFrom,omitempty"`
	// Extra Helm CLI-style arguments (e.g. "--set installCRDs=true")
	// +optional
	HelmArgs []string `json:"helmArgs,omitempty"`
	// Host filesystem path for binary installation
	// +optional
	InstallPath string `json:"installPath,omitempty"`
	// Container image reference (for static-pod type, supports {{ version }})
	// +optional
	Image string `json:"image,omitempty"`
	// Components that must be installed before this one
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`
	// Post-install patches to apply to cluster resources
	// +optional
	Patches []ComponentPatch `json:"patches,omitempty"`
	// Raw YAML manifests to apply inline (for manifest-type components).
	// Applied after URL-sourced manifests, with retry for CRD availability.
	// +optional
	InlineManifests []string `json:"inlineManifests,omitempty"`
	// User-customizable key-value pairs preserved across upgrades
	// +optional
	Custom map[string]string `json:"custom,omitempty"`
	// If set, this component is only installed when the named feature is enabled in ClusterConfig
	// +optional
	FeatureGate string `json:"featureGate,omitempty"`
}

type ValuesReference struct {
	// Kind of the reference (ConfigMap or Secret)
	// +kubebuilder:validation:Enum=ConfigMap;Secret
	Kind string `json:"kind"`
	// Name of the ConfigMap or Secret
	Name string `json:"name"`
	// Namespace of the ConfigMap or Secret; defaults to the release namespace
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Key in the ConfigMap/Secret data; defaults to "values.yaml"
	// +optional
	ValuesKey string `json:"valuesKey,omitempty"`
}

type ComponentPatch struct {
	// Target resource to patch
	Target PatchTarget `json:"target"`
	// Patch strategy
	Type PatchType `json:"type"`
	// Raw patch content (JSON or YAML)
	Patch string `json:"patch"`
}

type PatchTarget struct {
	// API version of the target resource
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
	// Kind of the target resource
	Kind string `json:"kind"`
	// Name of the target resource
	Name string `json:"name"`
	// Namespace of the target resource; empty for cluster-scoped
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ReleaseManifestStatus tracks per-component installation state
type ReleaseManifestStatus struct {
	// Per-component installation status
	// +optional
	Components map[string]ComponentStatus `json:"components,omitempty"`
	// Standard conditions
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Resolved OS image tag (stamped by CI)
	// +optional
	ResolvedOSImageTag string `json:"resolvedOSImageTag,omitempty"`
}

type ComponentStatus struct {
	// Current state of the component
	State ComponentState `json:"state,omitempty"`
	// Currently installed version
	InstalledVersion string `json:"installedVersion,omitempty"`
	// Human-readable message
	Message string `json:"message,omitempty"`
	// Last time the state transitioned
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Components",type="integer",JSONPath=".status.conditions[?(@.type=='Ready')].message"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ReleaseManifest is the Schema for the releasemanifests API.
// It defines all components and their versions for a k4all cluster.
type ReleaseManifest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReleaseManifestSpec   `json:"spec,omitempty"`
	Status ReleaseManifestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReleaseManifestList contains a list of ReleaseManifest
type ReleaseManifestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReleaseManifest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReleaseManifest{}, &ReleaseManifestList{})
}
