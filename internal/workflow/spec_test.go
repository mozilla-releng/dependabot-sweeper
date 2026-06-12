package workflow_test

import (
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/workflow"
)

// allPRStages derives the authoritative stage list from models.AllPRStages so
// that adding a constant to models (and AllPRStages) automatically triggers a
// test failure here until the workflow spec is also updated.
func allPRStages() []string {
	out := make([]string, len(models.AllPRStages))
	for i, s := range models.AllPRStages {
		out[i] = string(s)
	}
	return out
}

// terminalStages are the stages from which there are no further transitions.
var terminalStages = []string{
	string(models.StageApproved),
	string(models.StageFinalized),
	string(models.StageSkipped),
	string(models.StageFlagged),
	string(models.StageGaveUp),
	string(models.StageError),
}

// TestSpecStageCoverage asserts:
//  1. Every models.PRStage constant appears as a non-decision node in the spec.
//  2. No extra stage nodes exist in the spec that are not a PRStage constant.
func TestSpecStageCoverage(t *testing.T) {
	g := workflow.Spec()

	specStageIDs := make(map[string]bool)
	for _, n := range g.Nodes {
		if n.Kind != workflow.NodeKindDecision {
			specStageIDs[n.ID] = true
		}
	}

	want := make(map[string]bool)
	for _, s := range allPRStages() {
		want[s] = true
		if !specStageIDs[s] {
			t.Errorf("PRStage %q is defined in models but missing from the workflow spec", s)
		}
	}
	for id := range specStageIDs {
		if !want[id] {
			t.Errorf("workflow spec has stage node %q which is not a known models.PRStage constant", id)
		}
	}
}

// TestSpecEdgeIntegrity asserts that every edge references real node IDs.
func TestSpecEdgeIntegrity(t *testing.T) {
	g := workflow.Spec()

	nodeIDs := make(map[string]bool)
	for _, n := range g.Nodes {
		nodeIDs[n.ID] = true
	}

	for _, e := range g.Edges {
		if !nodeIDs[e.From] {
			t.Errorf("edge From references unknown node %q (To: %q)", e.From, e.To)
		}
		if !nodeIDs[e.To] {
			t.Errorf("edge To references unknown node %q (From: %q)", e.To, e.From)
		}
	}
}

// TestSpecTerminalSet asserts that terminal-kind nodes match the expected set.
func TestSpecTerminalSet(t *testing.T) {
	g := workflow.Spec()

	specTerminals := make(map[string]bool)
	for _, n := range g.Nodes {
		if n.Kind == workflow.NodeKindTerminal {
			specTerminals[n.ID] = true
		}
	}

	want := make(map[string]bool)
	for _, s := range terminalStages {
		want[s] = true
		if !specTerminals[s] {
			t.Errorf("expected %q to be a terminal node in the spec", s)
		}
	}
	for id := range specTerminals {
		if !want[id] {
			t.Errorf("unexpected terminal node %q in spec", id)
		}
	}
}

// TestSpecReachability asserts that every terminal stage is reachable from the
// entry node (via BFS), and that the entry itself is found.
func TestSpecReachability(t *testing.T) {
	g := workflow.Spec()

	adj := make(map[string][]string)
	for _, e := range g.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	visited := make(map[string]bool)
	queue := []string{g.EntryID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		queue = append(queue, adj[cur]...)
	}

	if !visited[g.EntryID] {
		t.Errorf("entry node %q was not reached during BFS (graph is empty?)", g.EntryID)
	}
	for _, s := range terminalStages {
		if !visited[s] {
			t.Errorf("terminal node %q is not reachable from entry %q", s, g.EntryID)
		}
	}
}

// TestSpecEntryNotTerminal asserts that the entry node is not a terminal.
func TestSpecEntryNotTerminal(t *testing.T) {
	g := workflow.Spec()

	nodeKinds := make(map[string]workflow.NodeKind)
	for _, n := range g.Nodes {
		nodeKinds[n.ID] = n.Kind
	}

	if k := nodeKinds[g.EntryID]; k == workflow.NodeKindTerminal {
		t.Errorf("entry node %q must not be a terminal node", g.EntryID)
	}
}

// TestSpecNodeLabelsNonEmpty asserts every node has a non-empty label and summary.
func TestSpecNodeLabelsNonEmpty(t *testing.T) {
	g := workflow.Spec()
	for _, n := range g.Nodes {
		if n.Label == "" {
			t.Errorf("node %q has empty Label", n.ID)
		}
		if n.Summary == "" {
			t.Errorf("node %q has empty Summary", n.ID)
		}
	}
}
