package drain

import (
	"testing"
	"time"
)

// The k4all Headlamp addon parses these messages to tell the admin what to
// do when a drain is held (gpillon/k4all headlamp-plugin/plugin/src/model/
// drainBlock.ts, PR gpillon/k4all#106): the ClusterUpgrade's
// status.nodes[].message and the node-settings rollout (ClusterConfig
// condition NodeSettingsRollout, reason DrainBlocked) carry them as they are.
// They are a contract until ClusterUpgrade status gets structured blockers
// (ai/upgrade-readiness spec): a rewording here must change drainBlock.ts
// and its tests in the same release, or the addon silently drops its advice.
// The addon relies on:
//   - "cannot be drained: " then "PDB <pdb> allows no disruption of pod <pod>"
//     joined by "; ", up to three and "and N more", then
//     " (a single-instance CloudNativePG";
//   - "runs pods that declare no controller: " or
//     "not evicted, they declare no controller: " then the pods joined by
//     ", " (", and N more" past three), then " (";
//   - the suffix "; the node is not cordoned".

func TestBlockedMessage_WordingTheAddonParses(t *testing.T) {
	got := BlockedMessage(
		[]Blocker{{PDB: "db/pg-primary", Pod: "db/pg-1"}, {PDB: "db/r", Pod: "db/r-0"}},
		[]string{"tools/debug", "tools/shell", "web/once", "web/twice"},
	)
	want := "cannot be drained: PDB db/pg-primary allows no disruption of pod db/pg-1; " +
		"PDB db/r allows no disruption of pod db/r-0" +
		" (a single-instance CloudNativePG cluster? see docs/upgrades.md, Drains that never end); " +
		"runs pods that declare no controller: tools/debug, tools/shell, web/once, and 1 more" +
		" (a drain would delete them, and nothing recreates them: delete them, or recreate them under a" +
		" controller; see docs/upgrades.md, Pods that declare no controller)" +
		"; the node is not cordoned"
	if got != want {
		t.Errorf("BlockedMessage wording changed (update the addon's drainBlock.ts too):\n got %q\nwant %q", got, want)
	}

	four := []Blocker{{PDB: "a/1", Pod: "a/x1"}, {PDB: "a/2", Pod: "a/x2"}, {PDB: "a/3", Pod: "a/x3"}, {PDB: "a/4", Pod: "a/x4"}}
	got = BlockedMessage(four, nil)
	want = "cannot be drained: PDB a/1 allows no disruption of pod a/x1; PDB a/2 allows no disruption of pod a/x2; " +
		"PDB a/3 allows no disruption of pod a/x3; and 1 more" +
		" (a single-instance CloudNativePG cluster? see docs/upgrades.md, Drains that never end)" +
		"; the node is not cordoned"
	if got != want {
		t.Errorf("BlockedMessage wording changed (update the addon's drainBlock.ts too):\n got %q\nwant %q", got, want)
	}
}

func TestDrain_WordingTheAddonParses(t *testing.T) {
	bare := Pod{Namespace: "tools", Name: "debug", Created: start.Add(-time.Hour), NoController: true}
	moved := Pod{Namespace: "web", Name: "app", Created: start.Add(-time.Hour)}
	p := Drain([]Pod{moved, bare}, start, start.Add(time.Minute), Settings{Timeout: 30 * time.Minute}, false)
	want := "2 pods left: web/app, tools/debug" +
		"; not evicted, they declare no controller: tools/debug" +
		" (delete them, or recreate them under a controller)"
	if p.Message != want {
		t.Errorf("Drain wording changed (update the addon's drainBlock.ts too):\n got %q\nwant %q", p.Message, want)
	}
}
