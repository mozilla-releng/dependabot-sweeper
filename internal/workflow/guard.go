package workflow

import (
	"fmt"
	"log/slog"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

// allowedTransitions is the pre-computed set of legal stage->stage transitions
// derived from Spec() by collapsing decision and back edges. Populated by
// init(). The key is "from->to".
var allowedTransitions map[string]bool

func init() {
	allowedTransitions = buildAllowedTransitions(Spec())
}

// buildAllowedTransitions derives the set of legal stage->stage transitions from
// the workflow graph by collapsing decision nodes (Q8). A stage->stage transition
// S1->S2 is legal if there exists a path from S1 to S2 in the full graph that
// starts at S1, follows any edges (normal, decision, or back), and reaches S2
// at some point — with S1 appearing only at the start of the path (no re-entry
// of S1 mid-path, but other stage nodes may appear in between).
//
// This correctly handles:
//   - Direct stage->stage edges (the few that exist)
//   - Stage -> decision node -> ... -> stage paths (the common case)
//   - Back-edge loops (e.g. impl_resuming -> dec_ci_gate -> ... -> reviewing)
//
// The algorithm is: for each origin stage S1, BFS from S1 through the entire
// graph (following all edge types). Every stage node S2 encountered (other than
// S1 at any point after the start) is added as S1->S2.
func buildAllowedTransitions(g Graph) map[string]bool {
	// Build full adjacency list (all edge types).
	adjacency := make(map[string][]string, len(g.Nodes))
	for _, e := range g.Edges {
		adjacency[e.From] = append(adjacency[e.From], e.To)
	}

	// Identify stage nodes (non-decision).
	isStage := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.Kind != NodeKindDecision {
			isStage[n.ID] = true
		}
	}

	allowed := make(map[string]bool)

	for _, origin := range g.Nodes {
		if origin.Kind == NodeKindDecision {
			continue
		}
		// BFS from origin, following all edges. Record every stage node we
		// reach (other than the origin itself as a re-entry).
		visited := make(map[string]bool)
		visited[origin.ID] = true
		queue := []string{origin.ID}

		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]

			for _, next := range adjacency[cur] {
				if isStage[next] {
					// Found a reachable stage — record the transition from origin.
					if next != origin.ID {
						allowed[origin.ID+"->"+next] = true
					}
					// Continue through this stage node too (BFS doesn't stop at
					// stage nodes — the path can continue through them). But only
					// if not visited to avoid infinite loops.
					if !visited[next] {
						visited[next] = true
						queue = append(queue, next)
					}
				} else {
					// Decision node — traverse through it.
					if !visited[next] {
						visited[next] = true
						queue = append(queue, next)
					}
				}
			}
		}
	}
	return allowed
}

// ValidateTransition checks whether a stage transition from -> to is legal
// according to the post-Q10 workflow graph.
//
// If the transition is illegal, it logs a loud error and returns an error
// describing the violation. Callers MUST honour this error — an illegal
// transition (e.g. finalized -> pending) is a cost-safety incident because
// it can cause a processed PR to re-enter the expensive agentic step.
//
// Special case: the zero-value transition (from == "") is allowed for initial
// population (the first Report on a new PR row).
func ValidateTransition(from, to models.PRStage) error {
	if from == "" {
		return nil // initial population
	}
	if from == to {
		// Self-loops: allowed for idempotent re-stamps of active stages (e.g.
		// waiting_ci -> waiting_ci as the CI poll advances). Terminal self-loops
		// should not occur but are benign (no-op cycles emit nothing per T6a).
		return nil
	}
	key := string(from) + "->" + string(to)
	if !allowedTransitions[key] {
		err := fmt.Errorf("illegal stage transition %q -> %q: not in the workflow graph (cost-safety: re-check PR entry conditions)", from, to)
		slog.Error("ILLEGAL STAGE TRANSITION — possible cost-safety violation",
			"from", from, "to", to,
			"hint", "a PR in a terminal stage must never re-enter the agentic pipeline")
		return err
	}
	return nil
}

// AllowedFrom returns the set of stages reachable from `from` in one step
// through the collapsed transition graph. Used in tests.
func AllowedFrom(from models.PRStage) []models.PRStage {
	prefix := string(from) + "->"
	var out []models.PRStage
	for k := range allowedTransitions {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, models.PRStage(k[len(prefix):]))
		}
	}
	return out
}
