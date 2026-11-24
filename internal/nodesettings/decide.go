package nodesettings

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
	"github.com/gpillon/k4all-operator/internal/drain"
	"github.com/gpillon/k4all-operator/internal/upgradegraph"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MinRelease is the first k4all release whose k4all-upgrade agent applies
// node settings. Older nodes are skipped until they are upgraded.
const MinRelease = "v2.7.0"

// Values of Step.Cordon: who cordoned the node of a step.
const (
	// CordonByK4all: the operator cordoned it, and uncordons it at the end.
	CordonByK4all = "true"
	// CordonAlready: the admin had; it stays cordoned after the step.
	CordonAlready = "already"
	// CordonNone: a restart, without a cordon.
	CordonNone = "none"
)

// Step is a node's rollout step, kept in its StepAnnotation from the start
// to the end: the settings it gets, what it does for them, who cordoned it
// and when (its drain's start).
type Step struct {
	Hash    string    `json:"hash"`
	Class   Class     `json:"class"`
	Cordon  string    `json:"cordon"`
	Started time.Time `json:"started"`
}

// Encode is s as the annotation's JSON.
func (s Step) Encode() string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// parseStep reads a StepAnnotation. One that cannot be read is taken as a
// drain the admin cordoned for: the operator never uncordons it.
func parseStep(text string) Step {
	s := Step{}
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		return Step{Class: DrainReboot, Cordon: CordonAlready}
	}
	return s
}

// Values of NodeSettingsStatus.Phase.
const (
	// PhaseDone: the node has the settings.
	PhaseDone = "Done"
	// PhasePending: the node waits for its step.
	PhasePending = "Pending"
	// PhaseDraining: cordoned, its pods being evicted.
	PhaseDraining = "Draining"
	// PhaseApplying: asked for the settings (target), waiting for applied.
	PhaseApplying = "Applying"
	// PhaseSkipped: its k4all release has no agent for them (MinRelease).
	PhaseSkipped = "Skipped"
)

// ActionKind is a change the controller makes.
type ActionKind string

const (
	// WriteConfig patches kube-system/kubelet-config: data.kubelet with
	// KubeletConfig (when not empty) and ConfigMapAnnotation with Record.
	WriteConfig ActionKind = "WriteConfig"
	// Start writes Step on the Node; with Step.Cordon "true" also cordons
	// it, and with a draining class labels it draining.
	Start ActionKind = "Start"
	// UpdateStep rewrites the Node's Step (the settings changed while it
	// drained).
	UpdateStep ActionKind = "UpdateStep"
	// Evict evicts Pods (namespace/name) from the Node.
	Evict ActionKind = "Evict"
	// EvictSelf evicts the operator's own pod (Pods) from the Node it
	// drained, so the leader moves before the node reboots (k4all#97).
	EvictSelf ActionKind = "EvictSelf"
	// SetTarget writes node-settings-target: Hash.
	SetTarget ActionKind = "SetTarget"
	// Finish ends the Node's Step: removes it, uncordons the node if the
	// operator cordoned it and removes the draining label.
	Finish ActionKind = "Finish"
	// LetGo ends the Node's Step without the settings, as Finish, and puts
	// node-settings-target back to Target (removed when empty) and removes
	// node-settings-message: the next step starts from the new settings.
	LetGo ActionKind = "LetGo"
)

// Action is one change.
type Action struct {
	Kind          ActionKind
	Node          string
	Step          *Step
	Hash          string   // SetTarget
	Target        string   // LetGo
	Pods          []string // Evict, EvictSelf
	KubeletConfig string   // WriteConfig: the new data.kubelet, "" to keep it
	Record        *Record  // WriteConfig
}

// Input is the state Decide works from.
type Input struct {
	Spec  *k4allv1alpha1.NodesConfig
	Nodes []Node
	// KubeletConfig is data.kubelet of kube-system/kubelet-config;
	// KubeletConfigMissing that the ConfigMap is not there.
	KubeletConfig        string
	KubeletConfigMissing bool
	// Record is its ConfigMapAnnotation, nil when absent.
	Record *Record
	// Pods holds the pods the drain moves off the node in its step.
	Pods map[string][]drain.Pod
	// UnhealthyPDBs are the budgets short of healthy pods, sorted.
	UnhealthyPDBs []drain.PDB
	// Blockers holds, by node, the pods its drain could never evict.
	Blockers map[string][]drain.Blocker
	// Drain is ClusterUpgrade k4all's spec.drain.
	Drain drain.Settings
	// UpgradeBusy says what of a ClusterUpgrade keeps a step from starting;
	// "" when nothing does.
	UpgradeBusy string
	// PodNetwork is for the maxPods check.
	PodNetwork PodNetwork
	// Self is the operator's pod (nil: unknown); SelfEvictTried that the
	// drain of its node evicted it once already.
	Self           *drain.Self
	SelfEvictTried bool
	Now            time.Time
	// Generation of the ClusterConfig, for the condition.
	Generation int64
}

// Result is what Decide returns. A nil Status (and Condition) means k4all
// owns no node setting and never wrote one: status.nodes stays absent.
type Result struct {
	Status    *k4allv1alpha1.NodesStatus
	Condition *metav1.Condition
	Actions   []Action
}

// emptyHash is the hash of no settings: what a node never touched has.
var emptyHash = Settings{}.Hash()

// decision is one Decide in progress.
type decision struct {
	in      Input
	desired Settings
	hash    string
	known   map[string]Settings
	nodes   []Node
	status  k4allv1alpha1.NodesStatus
	actions []Action
	reason  string
}

// Decide returns the rollout status, the NodeSettingsRollout condition and
// the actions to run now. It moves at most one node: the one in its step,
// or else the next one that lacks the settings.
func Decide(in Input) Result {
	desired := FromSpec(in.Spec)
	if desired.Empty() && in.Record == nil && !anyAnnotated(in.Nodes) {
		return Result{}
	}
	d := &decision{in: in, desired: desired, hash: desired.Hash(), known: in.Record.known()}
	d.known[emptyHash] = Settings{}
	d.known[d.hash] = desired
	self := ""
	if in.Self != nil {
		self = in.Self.Node
	}
	d.nodes = ordered(in.Nodes, self)
	d.status.SettingsHash = d.hash
	d.observe()
	d.run()
	return d.result()
}

func anyAnnotated(nodes []Node) bool {
	for _, n := range nodes {
		for _, key := range []string{TargetAnnotation, AppliedAnnotation, StepAnnotation} {
			if _, ok := n.Annotations[key]; ok {
				return true
			}
		}
	}
	return false
}

// roleRank orders the rollout: workers first, the bootstrap node last.
var roleRank = map[string]int{"worker": 0, "": 1, "control": 2, "bootstrap": 3}

// ordered sorts the nodes as they get the settings: workers, then control
// planes, then the bootstrap node, by name; self, the node running the
// operator, last of all: the operator moves once (k4all#97).
func ordered(nodes []Node, self string) []Node {
	out := append([]Node(nil), nodes...)
	rank := func(n Node) int {
		if n.Name == self {
			return len(roleRank) + 1
		}
		if r, ok := roleRank[n.Annotations[k4allv1alpha1.NodeTypeAnnotation]]; ok {
			return r
		}
		return roleRank[""]
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// applied is the settings hash a node has: a node that never reported one
// has no settings.
func applied(n Node) string {
	if h := n.Annotations[AppliedAnnotation]; h != "" {
		return h
	}
	return emptyHash
}

// eligible tells whether the node runs an agent that applies node settings,
// and otherwise why not.
func eligible(n Node) (bool, string) {
	release := n.Annotations[k4allv1alpha1.NodeReleaseAnnotation]
	if release == "" {
		return false, fmt.Sprintf("no %s annotation: the node settings need k4all %s or later", k4allv1alpha1.NodeReleaseAnnotation, MinRelease)
	}
	if c, err := upgradegraph.Compare(release, MinRelease); err != nil || c < 0 {
		return false, fmt.Sprintf("runs k4all %s: it gets the node settings after its upgrade to %s or later", release, MinRelease)
	}
	return true, ""
}

// observe sets each node's entry: Done, Pending or Skipped.
func (d *decision) observe() {
	for _, n := range d.nodes {
		ps := k4allv1alpha1.NodeSettingsStatus{Name: n.Name, AppliedHash: n.Annotations[AppliedAnnotation]}
		ok, why := eligible(n)
		switch {
		case applied(n) == d.hash:
			ps.Phase = PhaseDone
		case !ok:
			ps.Phase, ps.Message = PhaseSkipped, why
		default:
			ps.Phase, ps.Message = PhasePending, "next: "+string(d.classOf(n))
		}
		d.status.PerNode = append(d.status.PerNode, ps)
	}
}

// classOf is what node n does to get the desired settings, from the ones it
// has. A hash the record no longer knows could be anything: the heaviest.
func (d *decision) classOf(n Node) Class {
	from, ok := d.known[applied(n)]
	if !ok {
		return DrainReboot
	}
	if c := ClassOf(from, d.desired); c != "" {
		return c
	}
	return Restart // the same settings under another hash: only a new applied
}

func (d *decision) run() {
	for i, n := range d.nodes {
		if text, ok := n.Annotations[StepAnnotation]; ok {
			d.moveStep(i, parseStep(text))
			return
		}
	}
	if d.in.KubeletConfigMissing {
		d.block("KubeletConfigMissing", "kube-system/kubelet-config not found: kubeadm keeps the kubelet settings of every node there")
		return
	}
	if msg := Validate(d.desired, d.in.Nodes, d.in.PodNetwork); msg != "" {
		d.block("Invalid", "the node settings do not fit: "+msg)
		return
	}
	write, err := d.configWrite()
	if err != nil {
		d.block("KubeletConfigUnreadable", "kube-system/kubelet-config: "+err.Error())
		return
	}
	var pending, skipped []int
	for i, ps := range d.status.PerNode {
		switch ps.Phase {
		case PhasePending:
			pending = append(pending, i)
		case PhaseSkipped:
			skipped = append(skipped, i)
		}
	}
	if len(pending) == 0 {
		// Owned fields edited by hand come back, but not during an upgrade:
		// its kubeadm would give them to nodes without their step.
		if write != nil && d.in.UpgradeBusy == "" {
			d.actions = append(d.actions, *write)
		}
		if len(skipped) > 0 {
			d.block("NodesSkipped", fmt.Sprintf("waiting for %s to reach k4all %s: %s", d.names(skipped), MinRelease,
				d.status.PerNode[skipped[0]].Message))
			return
		}
		d.idle()
		return
	}
	if d.in.UpgradeBusy != "" {
		d.block("UpgradeInProgress", "waits for the ClusterUpgrade: "+d.in.UpgradeBusy)
		return
	}
	if d.in.Spec != nil && d.in.Spec.Rollout != nil && d.in.Spec.Rollout.Paused {
		d.setPhase(k4allv1alpha1.NodeRolloutPaused, "Paused",
			fmt.Sprintf("paused by spec.nodes.rollout.paused: %d nodes left (%s)", len(pending), d.names(pending)))
		return
	}
	for _, n := range d.nodes {
		if !n.Ready {
			d.rolling("waiting for " + n.Name + " to be Ready")
			return
		}
	}
	if wait := drain.WaitForPDBs(d.in.UnhealthyPDBs, d.lastDone(), d.in.Now, d.in.Drain.Settle); wait != "" {
		d.rolling(wait)
		return
	}
	d.startStep(pending[0], write)
}

// startStep starts node i: kubelet-config first (write), then its step.
func (d *decision) startStep(i int, write *Action) {
	n, ps := d.nodes[i], &d.status.PerNode[i]
	class := d.classOf(n)
	single := len(d.nodes) == 1
	if class.Drains() && !single {
		if blockers := d.in.Blockers[n.Name]; len(blockers) > 0 {
			ps.Message = drain.BlockedMessage(blockers)
			d.block("DrainBlocked", n.Name+" "+ps.Message)
			return
		}
	}
	if write != nil {
		d.actions = append(d.actions, *write)
	}
	step := Step{Hash: d.hash, Class: class, Cordon: CordonNone, Started: d.in.Now}
	if class.Drains() {
		step.Cordon = CordonByK4all
		if n.Unschedulable {
			step.Cordon = CordonAlready
		}
	}
	d.actions = append(d.actions, Action{Kind: Start, Node: n.Name, Step: &step})
	d.status.Rollout.CurrentNode = n.Name
	if class.Drains() && !single {
		ps.Phase, ps.Message = PhaseDraining, "draining"
		d.rolling(fmt.Sprintf("%s: draining (%s)", n.Name, class))
		return
	}
	d.actions = append(d.actions, Action{Kind: SetTarget, Node: n.Name, Hash: d.hash})
	ps.Phase, ps.Message = PhaseApplying, ""
	d.rolling(fmt.Sprintf("%s: applying the settings (%s)", n.Name, class))
}

// moveStep moves on node i, in its step.
func (d *decision) moveStep(i int, step Step) {
	n, ps := d.nodes[i], &d.status.PerNode[i]
	d.status.Rollout.CurrentNode = n.Name
	if target := n.Annotations[TargetAnnotation]; target != "" && target == step.Hash {
		d.waitForNode(i, step)
		return
	}
	if d.hash != step.Hash {
		// The settings changed before the target: the node gets the new ones
		// if it still needs a drain, else it is let go and starts again.
		class := d.classOf(n)
		if !class.Drains() {
			d.letGo(i, step)
			return
		}
		if !d.in.KubeletConfigMissing && Validate(d.desired, d.in.Nodes, d.in.PodNetwork) == "" {
			if write, err := d.configWrite(); err == nil {
				if write != nil {
					d.actions = append(d.actions, *write)
				}
				step.Hash, step.Class = d.hash, class
				updated := step
				d.actions = append(d.actions, Action{Kind: UpdateStep, Node: n.Name, Step: &updated})
			}
		}
		// Else the new settings do not fit: the drain goes on for the old ones.
	}
	single := len(d.nodes) == 1
	p := drain.Drain(d.in.Pods[n.Name], step.Started, d.in.Now, d.in.Drain, single)
	switch p.Outcome {
	case drain.Evicting:
		d.actions = append(d.actions, Action{Kind: Evict, Node: n.Name, Pods: p.Evict})
		ps.Phase, ps.Message = PhaseDraining, p.Message
		d.rolling(n.Name + ": draining, " + p.Message)
		return
	case drain.TimedOut:
		var evict []string
		for _, pod := range p.Left {
			if !pod.Terminating {
				evict = append(evict, pod.String())
			}
		}
		if len(evict) > 0 {
			d.actions = append(d.actions, Action{Kind: Evict, Node: n.Name, Pods: evict})
		}
		ps.Phase, ps.Message = PhaseDraining, p.Message
		d.block("DrainTimeout", n.Name+": "+p.Message+"; the node stays cordoned and the drain goes on: let the pods go, "+
			"or set spec.drain.continueOnTimeout of ClusterUpgrade k4all")
		return
	}
	if (drain.SelfEviction{Node: n.Name, Self: d.in.Self, Started: step.Started, Single: single,
		Elsewhere: elsewhere(d.nodes, n.Name), Tried: d.in.SelfEvictTried}).Now() {
		d.actions = append(d.actions, Action{Kind: EvictSelf, Node: n.Name, Pods: []string{d.in.Self.String()}})
		ps.Phase, ps.Message = PhaseDraining, "moving the k4all-operator off the node"
		d.rolling(n.Name + ": moving the k4all-operator off the node before its settings")
		return
	}
	d.actions = append(d.actions, Action{Kind: SetTarget, Node: n.Name, Hash: step.Hash})
	ps.Phase, ps.Message = PhaseApplying, ""
	if len(p.Back) > 0 {
		ps.Message = "back on the node after the drain started: " + drain.Names(p.Back)
	}
	d.rolling(fmt.Sprintf("%s: applying the settings (%s)", n.Name, step.Class))
}

// waitForNode waits for node i to apply its target, then finishes it.
func (d *decision) waitForNode(i int, step Step) {
	n, ps := d.nodes[i], &d.status.PerNode[i]
	msg := n.Annotations[MessageAnnotation]
	if applied(n) == step.Hash {
		if !n.Ready {
			ps.Phase, ps.Message = PhaseApplying, "applied; waiting for the node to be Ready"
			d.rolling(n.Name + ": waiting for it to be Ready")
			return
		}
		d.actions = append(d.actions, Action{Kind: Finish, Node: n.Name, Step: &step})
		if d.in.Record != nil {
			rec := *d.in.Record
			done := d.in.Now
			rec.LastDone = &done
			d.actions = append(d.actions, Action{Kind: WriteConfig, Record: &rec})
		}
		ps.Phase, ps.Message = PhaseDone, ""
		if step.Hash != d.hash {
			ps.Phase, ps.Message = PhasePending, "next: "+string(d.classOf(n))
		}
		d.status.Rollout.CurrentNode = ""
		d.rolling(n.Name + " has the settings " + step.Hash)
		return
	}
	ps.Phase, ps.Message = PhaseApplying, msg
	if msg != "" && !strings.HasPrefix(msg, waitingPrefix) {
		if d.hash != step.Hash {
			d.letGo(i, step)
			return
		}
		d.block("NodeRefused", fmt.Sprintf("%s refuses the settings: %s", n.Name, msg))
		return
	}
	what := "applying the settings"
	if msg != "" {
		what = msg
	}
	d.rolling(fmt.Sprintf("%s: %s (%s)", n.Name, what, step.Class))
}

// letGo ends the step of node i without its settings.
func (d *decision) letGo(i int, step Step) {
	n, ps := d.nodes[i], &d.status.PerNode[i]
	d.actions = append(d.actions, Action{Kind: LetGo, Node: n.Name, Step: &step, Target: n.Annotations[AppliedAnnotation]})
	ps.Phase, ps.Message = PhasePending, "the settings changed: its step starts again"
	if applied(n) == d.hash {
		ps.Phase, ps.Message = PhaseDone, ""
	}
	d.status.Rollout.CurrentNode = ""
	d.rolling(n.Name + ": the settings changed; its step starts again")
}

// elsewhere tells whether a node other than name is Ready and schedulable.
func elsewhere(nodes []Node, name string) bool {
	for _, n := range nodes {
		if n.Name != name && n.Ready && !n.Unschedulable {
			return true
		}
	}
	return false
}

// configWrite is the WriteConfig that brings kubelet-config to the desired
// settings, nil when it has them already.
func (d *decision) configWrite() (*Action, error) {
	var prior Prior
	var lastDone *time.Time
	if d.in.Record != nil {
		prior, lastDone = d.in.Record.Prior, d.in.Record.LastDone
	}
	doc, prior, changed, err := MergeKubelet(d.in.KubeletConfig, d.desired.Kubelet, prior)
	if err != nil {
		return nil, err
	}
	if !changed && d.in.Record != nil && d.in.Record.Hash == d.hash {
		return nil, nil
	}
	rec := Record{Hash: d.hash, Settings: d.desired, Prior: prior, History: d.history(), LastDone: lastDone}
	a := &Action{Kind: WriteConfig, Record: &rec}
	if changed {
		a.KubeletConfig = doc
	}
	return a, nil
}

// history keeps the settings of the hashes the nodes still have or were
// asked for, but the desired ones.
func (d *decision) history() map[string]Settings {
	out := map[string]Settings{}
	for _, n := range d.nodes {
		for _, h := range []string{n.Annotations[AppliedAnnotation], n.Annotations[TargetAnnotation]} {
			if s, ok := d.known[h]; ok && h != "" && h != d.hash && h != emptyHash {
				out[h] = s
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (d *decision) lastDone() time.Time {
	if d.in.Record == nil || d.in.Record.LastDone == nil {
		return time.Time{}
	}
	return *d.in.Record.LastDone
}

// names names the nodes at indices, up to three.
func (d *decision) names(indices []int) string {
	names := make([]string, 0, 4)
	for j, i := range indices {
		if j == 3 {
			names = append(names, fmt.Sprintf("and %d more", len(indices)-3))
			break
		}
		names = append(names, d.nodes[i].Name)
	}
	return strings.Join(names, ", ")
}

func (d *decision) setPhase(phase, reason, message string) {
	d.status.Rollout.Phase, d.status.Rollout.Message, d.reason = phase, message, reason
}

func (d *decision) rolling(message string) {
	d.setPhase(k4allv1alpha1.NodeRolloutRolling, "Rolling", message)
}

func (d *decision) block(reason, message string) {
	d.setPhase(k4allv1alpha1.NodeRolloutBlocked, reason, message)
}

func (d *decision) idle() {
	msg := "every node has the settings " + d.hash
	if d.desired.Empty() {
		msg = "k4all owns no node setting: every node is back to what kubeadm and the admin set"
	}
	d.setPhase(k4allv1alpha1.NodeRolloutIdle, "RolledOut", msg)
}

func (d *decision) result() Result {
	status := d.status
	c := metav1.Condition{
		Type:               k4allv1alpha1.ConditionNodeSettingsRollout,
		Status:             metav1.ConditionFalse,
		Reason:             d.reason,
		Message:            status.Rollout.Message,
		ObservedGeneration: d.in.Generation,
	}
	if status.Rollout.Phase == k4allv1alpha1.NodeRolloutIdle {
		c.Status = metav1.ConditionTrue
	}
	return Result{Status: &status, Condition: &c, Actions: d.actions}
}
