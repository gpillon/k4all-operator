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

// ClusterUpgradeName is the name of the only ClusterUpgrade. The operator
// creates it, so its status always shows the cluster's release.
const ClusterUpgradeName = "k4all"

// DefaultUpgradeGraphURL is the graph k4all-upgrade reads on the nodes.
const DefaultUpgradeGraphURL = "https://github.com/gpillon/k4all/releases/latest/download/upgrade-graph.json"

// Node annotations of the upgrade protocol between the operator and
// k4all-upgrade on each node (fcos-k8s ai/migrazioni/contratto-host-operator.md).
const (
	// Written by k4all-upgrade.
	NodeReleaseAnnotation        = "k4all.magesgate.com/release"         // release the node runs, set at each COMMIT and at boot
	NodeTypeAnnotation           = "k4all.magesgate.com/node-type"       // bootstrap | control | worker
	NodeUpgradePhaseAnnotation   = "k4all.magesgate.com/upgrade-phase"   // hop phase, e.g. STAGE, REBOOT, HOLD
	NodeUpgradeMessageAnnotation = "k4all.magesgate.com/upgrade-message" // why the hop is in HOLD
	NodeUpgradeDoneAnnotation    = "k4all.magesgate.com/upgrade-done"    // release of the hop just finished
	// Optional, next to HOLD: why, as a NodeUpgradeReason (TargetRejected,
	// RollbackRebooted). Without it a HOLD is HostHold.
	NodeUpgradeReasonAnnotation = "k4all.magesgate.com/upgrade-reason"

	// Written by the operator.
	NodeUpgradeTargetAnnotation   = "k4all.magesgate.com/upgrade-target"   // release of the hop the node must run
	NodeUpgradeCordonedAnnotation = "k4all.magesgate.com/upgrade-cordoned" // the operator cordoned the node, so it uncordons it

	// Set by the user (or the UI) on the ClusterUpgrade, with any value: the
	// operator runs again the node that failed with reason DrainTimeout or
	// Abandoned (cordoned, with no upgrade-target), starting its drain over
	// with a full drain timeout, then asking for the hop again. It does not
	// touch a node in HOLD, which k4all-upgrade resumes on the node. The
	// operator removes the annotation after the reconcile that read it.
	ClusterUpgradeRetryAnnotation = "k4all.magesgate.com/retry"
)

// ClusterUpgradeSpec is the release the cluster should reach.
type ClusterUpgradeSpec struct {
	// Release to bring every node to, e.g. v2.7.0. Empty means no upgrade is
	// requested. Lowering it, or emptying it, stops the upgrade after the
	// node in progress.
	// +optional
	// +kubebuilder:validation:Pattern=`^(v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)?$`
	TargetRelease string `json:"targetRelease,omitempty"`

	// Upgrade graph the path is planned on. It must be the graph the nodes'
	// k4all-upgrade reads. Empty means the k4all release graph on GitHub.
	// +optional
	GraphURL string `json:"graphUrl,omitempty"`

	// Paused stops the upgrade before the next node. A node already
	// upgrading finishes its hop.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// +optional
	// +kubebuilder:default={}
	Drain DrainSpec `json:"drain,omitempty"`
}

// DrainSpec sets how a node is drained before its hop. A cluster with a
// single node is only cordoned: its pods have nowhere to go.
type DrainSpec struct {
	// Minutes to wait for the evictions, which PodDisruptionBudgets can hold
	// back. Then the node fails, unless continueOnTimeout is set.
	// +optional
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	TimeoutMinutes int32 `json:"timeoutMinutes,omitempty"`

	// Run the hop anyway when the drain times out. The pods left on the node
	// stop at its reboot.
	// +optional
	ContinueOnTimeout bool `json:"continueOnTimeout,omitempty"`
}

// ClusterUpgradePhase is where the whole upgrade stands.
// +kubebuilder:validation:Enum=Idle;Paused;Progressing;Succeeded;Failed
type ClusterUpgradePhase string

const (
	// No targetRelease.
	ClusterUpgradeIdle ClusterUpgradePhase = "Idle"
	// spec.paused, with no node in the middle of a hop.
	ClusterUpgradePaused ClusterUpgradePhase = "Paused"
	// Nodes are being upgraded, or wait for their turn.
	ClusterUpgradeProgressing ClusterUpgradePhase = "Progressing"
	// Every node runs targetRelease.
	ClusterUpgradeSucceeded ClusterUpgradePhase = "Succeeded"
	// Stopped: see message. A node in HOLD goes on by itself once
	// k4all-upgrade resumes it; a failed drain needs the retry annotation.
	ClusterUpgradeFailed ClusterUpgradePhase = "Failed"
)

// HopPhase is where one hop of the path stands.
// +kubebuilder:validation:Enum=Done;InProgress;Pending
type HopPhase string

const (
	HopDone       HopPhase = "Done"
	HopInProgress HopPhase = "InProgress"
	HopPending    HopPhase = "Pending"
)

// NodeUpgradePhase is where one node stands in the current hop.
// +kubebuilder:validation:Enum=Pending;Draining;Upgrading;Done;Failed
type NodeUpgradePhase string

const (
	// Waits for its turn in the current hop.
	NodeUpgradePending NodeUpgradePhase = "Pending"
	// Cordoned, its pods being evicted.
	NodeUpgradeDraining NodeUpgradePhase = "Draining"
	// upgrade-target is set: k4all-upgrade runs the hop (see hostPhase).
	NodeUpgradeUpgrading NodeUpgradePhase = "Upgrading"
	// Runs the current hop's release, or a newer one.
	NodeUpgradeDone NodeUpgradePhase = "Done"
	// HOLD on the node, or the drain timed out: see message.
	NodeUpgradeFailed NodeUpgradePhase = "Failed"
)

// NodeUpgradeReason says, in a machine-readable way, why a node failed or
// what the upgrade waits for on it.
// +kubebuilder:validation:Enum=DrainTimeout;HostHold;Abandoned;TargetRejected;RollbackRebooted;NodeNotReady
type NodeUpgradeReason string

const (
	// Failed: the drain timed out (PodDisruptionBudgets). Retry annotation.
	NodeReasonDrainTimeout NodeUpgradeReason = "DrainTimeout"
	// Failed: k4all-upgrade put the hop in HOLD; it goes on once resumed on the node.
	NodeReasonHostHold NodeUpgradeReason = "HostHold"
	// Failed: the hop was abandoned on the node (clear-hold --abandon). Retry annotation.
	NodeReasonAbandoned NodeUpgradeReason = "Abandoned"
	// Failed: k4all-upgrade refused the upgrade-target (upgrade-reason annotation).
	NodeReasonTargetRejected NodeUpgradeReason = "TargetRejected"
	// Failed: k4all-upgrade rolled the node back and rebooted it (upgrade-reason annotation).
	NodeReasonRollbackRebooted NodeUpgradeReason = "RollbackRebooted"
	// Not failed: the upgrade waits for this node to be Ready.
	NodeReasonNotReady NodeUpgradeReason = "NodeNotReady"
)

// Hop is one release of the path.
type Hop struct {
	Release string   `json:"release"`
	Kind    string   `json:"kind,omitempty"` // minor | patch
	Phase   HopPhase `json:"phase"`
	// When the hop became InProgress.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// When every node had run it.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// ReleaseInfo describes a release of the upgrade graph.
type ReleaseInfo struct {
	Version    string `json:"version"`
	Kind       string `json:"kind,omitempty"`       // minor | patch
	MinFrom    string `json:"minFrom,omitempty"`    // oldest release that may switch straight to it
	Kubernetes string `json:"kubernetes,omitempty"` // kubelet version in the image
	Crio       string `json:"crio,omitempty"`       // CRI-O minor in the image
	Fedora     int32  `json:"fedora,omitempty"`
	Notes      string `json:"notes,omitempty"`
	// Hops from currentRelease to this release, as the operator would plan
	// them; empty when the graph has no way there. Only in availableReleases.
	// +optional
	Path []string `json:"path,omitempty"`
}

// NodeUpgradeStatus is one node's part in the upgrade.
type NodeUpgradeStatus struct {
	Name string `json:"name"`
	// bootstrap, control or worker (the node-type annotation). Nodes are
	// upgraded in this order, one at a time.
	Role string `json:"role,omitempty"`
	// Release the node runs (the release annotation).
	Release string           `json:"release,omitempty"`
	Phase   NodeUpgradePhase `json:"phase"`
	// k4all-upgrade's phase of the hop (the upgrade-phase annotation).
	// +optional
	HostPhase string `json:"hostPhase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// Why the node failed, or what the upgrade waits for on it.
	// +optional
	Reason NodeUpgradeReason `json:"reason,omitempty"`
	// When the operator cordoned the node for the current hop. A retry
	// starts it over.
	// +optional
	DrainStartedAt *metav1.Time `json:"drainStartedAt,omitempty"`
	// When the operator started the node's latest hop (its cordon).
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// When the node finished its latest hop.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// ClusterUpgradeStatus is the observed state of the upgrade.
type ClusterUpgradeStatus struct {
	// +optional
	Phase ClusterUpgradePhase `json:"phase,omitempty"`
	// What the upgrade waits for, or why it stopped.
	// +optional
	Message string `json:"message,omitempty"`
	// The oldest release among the nodes: the one the whole cluster runs.
	// +optional
	CurrentRelease string `json:"currentRelease,omitempty"`
	// The graph's entry for currentRelease (Kubernetes, CRI-O, Fedora).
	// +optional
	CurrentReleaseInfo *ReleaseInfo `json:"currentReleaseInfo,omitempty"`
	// Release of the hop in progress.
	// +optional
	CurrentHop string `json:"currentHop,omitempty"`
	// Hops from the release the upgrade started at to targetRelease, in
	// order. All nodes finish a hop before any node starts the next.
	// +optional
	Path []Hop `json:"path,omitempty"`
	// Releases newer than currentRelease that targetRelease can name, oldest
	// first. Blocked releases are left out.
	// +optional
	AvailableReleases []ReleaseInfo `json:"availableReleases,omitempty"`
	// +optional
	Nodes []NodeUpgradeStatus `json:"nodes,omitempty"`
	// Graph the path was planned on.
	// +optional
	GraphURL string `json:"graphUrl,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// GraphAvailable (the graph was read; False with the error), and
	// Progressing, Succeeded, Failed, True in the phase of the same name.
	// Their reason is machine-readable: the phase, a NodeUpgradeReason, or
	// for Failed without a node NoPath, Downgrade, NodeNotReported.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterUpgrade condition types.
const (
	ConditionGraphAvailable = "GraphAvailable"
	ConditionProgressing    = "Progressing"
	ConditionSucceeded      = "Succeeded"
	ConditionFailed         = "Failed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'k4all'",message="there is one ClusterUpgrade, named k4all"
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRelease`
// +kubebuilder:printcolumn:name="Current",type=string,JSONPath=`.status.currentRelease`
// +kubebuilder:printcolumn:name="Hop",type=string,JSONPath=`.status.currentHop`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterUpgrade moves every node of the cluster to a k4all release, one hop
// of the upgrade graph at a time and one node at a time. The operator cordons
// and drains a node, then asks k4all-upgrade on it for the hop through the
// node's annotations.
type ClusterUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterUpgradeSpec   `json:"spec,omitempty"`
	Status ClusterUpgradeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterUpgradeList contains a list of ClusterUpgrade.
type ClusterUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterUpgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterUpgrade{}, &ClusterUpgradeList{})
}
