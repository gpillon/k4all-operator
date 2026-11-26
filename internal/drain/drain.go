// Package drain holds the node drain the operator runs before a node is
// changed: a ClusterUpgrade hop and a node-settings rollout step. Like
// kubectl drain, it evicts the pods a node had when its drain started, and
// neither evicts nor waits for the ones put back on it since. It does no
// I/O: the controllers list the pods and run the evictions.
package drain

import (
	"fmt"
	"strings"
	"time"
)

// Label is on a Node from the operator's cordon to its uncordon. The
// operator's pod tolerates the cordon, which a single node needs, and prefers
// the nodes without this label (config/manager/manager.yaml): a drain that
// evicts it does not put it back on the node about to reboot (k4all#86).
// k4all-upgrade sets it too when it drains a node itself.
const Label = "k4all.magesgate.com/draining"

// Pod is a pod the drain moves off its node: not a DaemonSet, static or
// finished pod, nor the operator itself.
type Pod struct {
	Namespace, Name string
	Created         time.Time
	Terminating     bool
	// NoController: no ownerReference is its controller, so nothing
	// recreates it once evicted. The drain leaves it to the admin, as
	// `kubectl drain` without --force does (k4all#99).
	NoController bool
}

func (p Pod) String() string { return p.Namespace + "/" + p.Name }

// PDB is a PodDisruptionBudget with fewer healthy pods than it wants.
type PDB struct {
	Namespace, Name  string
	Healthy, Desired int32
}

// Blocker is a pod a drain cannot move: its PodDisruptionBudget allows no
// disruption although every pod it expects is healthy, so waiting changes
// nothing (the primary of a single-instance CloudNativePG cluster).
type Blocker struct {
	PDB, Pod string // namespace/name
}

// Settings are the drain's limits: ClusterUpgrade spec.drain.
type Settings struct {
	// How long the drain may take.
	Timeout time.Duration
	// How long the next node waits for the PodDisruptionBudgets to have
	// their healthy pods back after a node finished.
	Settle time.Duration
	// Go on with the node when the drain times out.
	ContinueOnTimeout bool
}

// SettingsFrom is spec.drain of the ClusterUpgrade: timeoutMinutes (30 when
// 0), settleMinutes (10 when unset) and continueOnTimeout.
func SettingsFrom(timeoutMinutes int32, settleMinutes *int32, continueOnTimeout bool) Settings {
	s := Settings{Timeout: 30 * time.Minute, Settle: 10 * time.Minute, ContinueOnTimeout: continueOnTimeout}
	if timeoutMinutes > 0 {
		s.Timeout = time.Duration(timeoutMinutes) * time.Minute
	}
	if settleMinutes != nil {
		s.Settle = time.Duration(*settleMinutes) * time.Minute
	}
	return s
}

// Outcome is where a drain is.
type Outcome int

const (
	// Evicting: pods are left; evict them and look again.
	Evicting Outcome = iota
	// TimedOut: pods are still left after the timeout.
	TimedOut
	// Drained: nothing left to move (or the timeout and continueOnTimeout).
	Drained
)

// Progress is one look at a drain.
type Progress struct {
	Outcome Outcome
	// The pods the node had when the drain started, still there.
	Left []Pod
	// The pods put back on the node since the drain started.
	Back []Pod
	// The pods to evict now (namespace/name): Left, less the terminating
	// and those that declare no controller (Pod.NoController).
	Evict []string
	// What is left, or why the drain timed out; empty once Drained.
	Message string
}

// Drain looks at the drain of a node that started at started: pods are the
// pods the drain moves that are on the node now. A single node is not
// drained: its workloads stop with its reboot.
func Drain(pods []Pod, started, now time.Time, s Settings, single bool) Progress {
	left, back := Split(pods, started)
	p := Progress{Outcome: Drained, Left: left, Back: back}
	if len(left) == 0 || single {
		return p
	}
	if now.Sub(started) < s.Timeout {
		p.Outcome = Evicting
		p.Message = fmt.Sprintf("%d pods left: %s", len(left), Names(left))
		var bare []string
		for _, pod := range left {
			switch {
			case pod.NoController:
				bare = append(bare, pod.String())
			case !pod.Terminating:
				p.Evict = append(p.Evict, pod.String())
			}
		}
		if len(bare) > 0 {
			p.Message += "; not evicted, they declare no controller: " + nameSome(bare) +
				" (delete them, or recreate them under a controller)"
		}
		return p
	}
	if s.ContinueOnTimeout {
		return p
	}
	p.Outcome = TimedOut
	p.Message = fmt.Sprintf("drain timed out after %d minutes, %d pods left: %s",
		int(s.Timeout/time.Minute), len(left), Names(left))
	return p
}

// Split sorts the pods of a node being drained: the ones it had when the
// drain started, which the drain moves, and the ones put back on it since,
// which tolerate the cordon or were bound to the node. Like kubectl drain,
// the drain neither evicts nor waits for the second ones.
func Split(pods []Pod, started time.Time) (left, back []Pod) {
	for _, p := range pods {
		if p.Created.After(started) {
			back = append(back, p)
		} else {
			left = append(left, p)
		}
	}
	return left, back
}

// Names names up to three pods.
func Names(pods []Pod) string {
	names := make([]string, 0, 3)
	for i, p := range pods {
		if i == 3 {
			names = append(names, fmt.Sprintf("and %d more", len(pods)-3))
			break
		}
		names = append(names, p.String())
	}
	return strings.Join(names, ", ")
}

// WaitForPDBs names the PodDisruptionBudget the next node waits for. After a
// node finishes its pods start again elsewhere; until every budget has its
// healthy pods back, the next drain would be refused. The wait ends settle
// after lastDone, when the last node finished (zero: none yet).
func WaitForPDBs(unhealthy []PDB, lastDone, now time.Time, settle time.Duration) string {
	if len(unhealthy) == 0 || lastDone.IsZero() || now.Sub(lastDone) >= settle {
		return ""
	}
	p := unhealthy[0]
	return fmt.Sprintf("waiting for PDB %s/%s: %d/%d healthy", p.Namespace, p.Name, p.Healthy, p.Desired)
}

// BlockedMessage says why a node is not cordoned: pods on it that its drain
// could never evict (blockers, k4all#87), or would delete for good because
// they declare no controller (bare, namespace/name, k4all#99). Changing a
// budget or deleting a pod is the admin's call. "" when neither.
func BlockedMessage(blockers []Blocker, bare []string) string {
	var why []string
	if len(blockers) > 0 {
		named := make([]string, 0, 4)
		for j, b := range blockers {
			if j == 3 {
				named = append(named, fmt.Sprintf("and %d more", len(blockers)-3))
				break
			}
			named = append(named, fmt.Sprintf("PDB %s allows no disruption of pod %s", b.PDB, b.Pod))
		}
		why = append(why, "cannot be drained: "+strings.Join(named, "; ")+
			" (a single-instance CloudNativePG cluster? see docs/upgrades.md, Drains that never end)")
	}
	if len(bare) > 0 {
		why = append(why, "runs pods that declare no controller: "+nameSome(bare)+
			" (a drain would delete them, and nothing recreates them: delete them, or recreate them under a"+
			" controller; see docs/upgrades.md, Pods that declare no controller)")
	}
	if len(why) == 0 {
		return ""
	}
	return strings.Join(why, "; ") + "; the node is not cordoned"
}

// nameSome names up to three of names and counts the rest.
func nameSome(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:3], ", "), len(names)-3)
}

// Self is the operator's own pod and the node it runs on.
type Self struct {
	Pod
	Node string
}

// SelfEviction is what decides whether a drain evicts the operator's own pod
// (k4all#97). The drain never counts that pod: it would stop the drain. So
// once every other pod has left the node that runs it, the operator evicts
// itself, and the Deployment's next pod, the next leader, starts on another
// node and goes on; otherwise the leader would go down with the node's reboot
// and nothing would drive the rollout until the node is back.
type SelfEviction struct {
	// The node drained, every other pod gone.
	Node string
	// The operator's pod; nil when it could not be read.
	Self *Self
	// When the drain of Node started.
	Started time.Time
	// The cluster has a single node: it is not drained.
	Single bool
	// Another node is Ready and schedulable: somewhere for the next pod.
	Elsewhere bool
	// This drain evicted the operator once already: a pod back on the node
	// after that stays, as before k4all#97, and goes down with the reboot.
	Tried bool
}

// Now tells whether to evict the operator's own pod now. A pod created after
// the drain started is one put back on the node: it is never evicted again.
func (s SelfEviction) Now() bool {
	return s.Self != nil && s.Self.Node == s.Node && !s.Single && s.Elsewhere && !s.Tried &&
		!s.Self.Created.After(s.Started)
}
