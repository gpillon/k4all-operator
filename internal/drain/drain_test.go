package drain

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var start = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func pod(name string, created time.Time) Pod {
	return Pod{Namespace: "ns", Name: name, Created: created}
}

func TestDrain_EvictsThePodsTheNodeHadWhenItStarted(t *testing.T) {
	before, after := pod("a", start.Add(-time.Hour)), pod("b", start.Add(time.Minute))
	terminating := pod("c", start.Add(-time.Hour))
	terminating.Terminating = true
	p := Drain([]Pod{before, after, terminating}, start, start.Add(2*time.Minute), Settings{Timeout: 30 * time.Minute}, false)
	if p.Outcome != Evicting {
		t.Fatalf("outcome = %v, want Evicting", p.Outcome)
	}
	if want := []string{"ns/a"}; !reflect.DeepEqual(p.Evict, want) {
		t.Errorf("evict = %v, want %v (not the terminating one, not the one back since)", p.Evict, want)
	}
	if p.Message != "2 pods left: ns/a, ns/c" {
		t.Errorf("message = %q", p.Message)
	}
}

func TestDrain_IsDrainedWhenOnlyPodsPutBackAreLeft(t *testing.T) {
	p := Drain([]Pod{pod("b", start.Add(time.Minute))}, start, start.Add(2*time.Minute), Settings{Timeout: time.Minute}, false)
	if p.Outcome != Drained || len(p.Back) != 1 {
		t.Fatalf("outcome = %v back = %v, want Drained with one pod back", p.Outcome, p.Back)
	}
}

func TestDrain_TimesOutUnlessToldToGoOn(t *testing.T) {
	pods := []Pod{pod("a", start.Add(-time.Hour))}
	late := start.Add(31 * time.Minute)
	p := Drain(pods, start, late, Settings{Timeout: 30 * time.Minute}, false)
	if p.Outcome != TimedOut || !strings.Contains(p.Message, "drain timed out after 30 minutes, 1 pods left: ns/a") {
		t.Errorf("outcome = %v message = %q, want TimedOut", p.Outcome, p.Message)
	}
	if p := Drain(pods, start, late, Settings{Timeout: 30 * time.Minute, ContinueOnTimeout: true}, false); p.Outcome != Drained {
		t.Errorf("outcome = %v with continueOnTimeout, want Drained", p.Outcome)
	}
}

func TestDrain_SingleNodeIsNotDrained(t *testing.T) {
	p := Drain([]Pod{pod("a", start.Add(-time.Hour))}, start, start, Settings{Timeout: time.Minute}, true)
	if p.Outcome != Drained || len(p.Evict) != 0 {
		t.Errorf("outcome = %v evict = %v, want Drained without evictions", p.Outcome, p.Evict)
	}
}

func TestWaitForPDBs_WaitsForTheSettleTimeAfterTheLastNode(t *testing.T) {
	pdbs := []PDB{{Namespace: "db", Name: "pg", Healthy: 1, Desired: 2}}
	if got := WaitForPDBs(pdbs, start, start.Add(time.Minute), 10*time.Minute); got != "waiting for PDB db/pg: 1/2 healthy" {
		t.Errorf("wait = %q", got)
	}
	if got := WaitForPDBs(pdbs, start, start.Add(11*time.Minute), 10*time.Minute); got != "" {
		t.Errorf("wait = %q after the settle time, want none", got)
	}
	if got := WaitForPDBs(pdbs, time.Time{}, start, 10*time.Minute); got != "" {
		t.Errorf("wait = %q before any node finished, want none", got)
	}
	if got := WaitForPDBs(nil, start, start, 10*time.Minute); got != "" {
		t.Errorf("wait = %q without unhealthy PDBs", got)
	}
}

func TestBlockedMessage_NamesUpToThreeBlockers(t *testing.T) {
	b := []Blocker{{PDB: "a/p1", Pod: "a/x"}, {PDB: "a/p2", Pod: "a/y"}, {PDB: "a/p3", Pod: "a/z"}, {PDB: "a/p4", Pod: "a/w"}}
	got := BlockedMessage(b)
	if !strings.HasPrefix(got, "cannot be drained: PDB a/p1 allows no disruption of pod a/x; ") ||
		!strings.Contains(got, "and 1 more") || !strings.HasSuffix(got, "the node is not cordoned") {
		t.Errorf("message = %q", got)
	}
}

// k4all#97: the drain of the node that runs the operator moves the operator
// off it, so the leader is elsewhere when the node reboots.
func TestEvictSelf(t *testing.T) {
	self := &Self{Node: "w-1", Pod: pod("k4all-operator", start.Add(-time.Hour))}
	back := &Self{Node: "w-1", Pod: pod("k4all-operator-2", start.Add(time.Minute))}
	cases := []struct {
		name string
		in   SelfEviction
		want bool
	}{
		{"its own node, drained, another node free", SelfEviction{Node: "w-1", Self: self, Started: start, Elsewhere: true}, true},
		{"another node", SelfEviction{Node: "w-0", Self: self, Started: start, Elsewhere: true}, false},
		{"single node", SelfEviction{Node: "w-1", Self: self, Started: start, Elsewhere: true, Single: true}, false},
		{"no other node to go to", SelfEviction{Node: "w-1", Self: self, Started: start}, false},
		{"tried already", SelfEviction{Node: "w-1", Self: self, Started: start, Elsewhere: true, Tried: true}, false},
		{"came back after the drain started", SelfEviction{Node: "w-1", Self: back, Started: start, Elsewhere: true}, false},
		{"own pod unknown", SelfEviction{Node: "w-1", Started: start, Elsewhere: true}, false},
	}
	for _, tc := range cases {
		if got := tc.in.Now(); got != tc.want {
			t.Errorf("%s: EvictSelf = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSettingsFrom_DefaultsAsClusterUpgrade(t *testing.T) {
	s := SettingsFrom(0, nil, false)
	if s.Timeout != 30*time.Minute || s.Settle != 10*time.Minute || s.ContinueOnTimeout {
		t.Errorf("settings = %+v, want 30m timeout, 10m settle", s)
	}
	five := int32(5)
	if s := SettingsFrom(12, &five, true); s.Timeout != 12*time.Minute || s.Settle != 5*time.Minute || !s.ContinueOnTimeout {
		t.Errorf("settings = %+v", s)
	}
}
