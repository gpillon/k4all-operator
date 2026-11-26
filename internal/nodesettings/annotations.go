package nodesettings

// Node annotations of the node-settings rollout (ADR-0006: the operator and
// the nodes talk through Node annotations). k4all-upgrade has the same names
// (ANNOTATION_NS_*); every published tool keeps speaking the protocol it
// shipped with, so a change has to stay compatible with all of them.
const (
	// TargetAnnotation is written by the operator: the settings hash the
	// node must apply. Set once the node is drained (or at once for a
	// restart), never changed while the node applies it.
	TargetAnnotation = "k4all.magesgate.com/node-settings-target"
	// AppliedAnnotation is written by the node's k4all-upgrade agent: the
	// settings hash it applied, once kubelet is Ready with it.
	AppliedAnnotation = "k4all.magesgate.com/node-settings-applied"
	// MessageAnnotation is written by the agent: why it waits ("waiting: …")
	// or why it refuses the settings (any other text). It goes with applied.
	MessageAnnotation = "k4all.magesgate.com/node-settings-message"
	// StepAnnotation is the operator's own: the node is in its rollout step,
	// from the cordon to the uncordon. A JSON Step. The agent ignores it.
	StepAnnotation = "k4all.magesgate.com/node-settings-step"
	// ConfigMapAnnotation, on kube-system/kubelet-config, is the settings
	// the operator rolls out: a JSON Record. The agent reads the hash and
	// the kernel arguments from it; kubeadm pulls the kubelet fields.
	ConfigMapAnnotation = "k4all.magesgate.com/node-settings"
)

// waitingPrefix starts a message of the agent that is not a refusal.
const waitingPrefix = "waiting"

// StepRunning tells whether a Node, by its annotations, is in a
// node-settings step: cordoned for it, or applying a target. A ClusterUpgrade
// starts no node meanwhile.
func StepRunning(annotations map[string]string) bool {
	if _, ok := annotations[StepAnnotation]; ok {
		return true
	}
	target := annotations[TargetAnnotation]
	return target != "" && target != annotations[AppliedAnnotation]
}
