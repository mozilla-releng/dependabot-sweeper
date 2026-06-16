// Package workflow defines the canonical workflow graph — the single source of
// truth for what the tool does. The spec_test.go verifies the graph stays
// consistent with the real models.PRStage constants, so adding a new stage to
// the code without updating this file fails the build.
package workflow

import "github.com/mozilla-releng/dependabot-sweeper/internal/models"

// NodeKind describes the role of a node in the workflow graph.
type NodeKind string

const (
	// NodeKindEntry is the single starting point (the pending stage).
	NodeKindEntry NodeKind = "entry"
	// NodeKindTransient is a stage that passes through quickly each cycle.
	NodeKindTransient NodeKind = "transient"
	// NodeKindActive is a stage where agent work or waiting is happening.
	NodeKindActive NodeKind = "active"
	// NodeKindDecision is rendered as a diamond; it has no corresponding PRStage.
	NodeKindDecision NodeKind = "decision"
	// NodeKindTerminal is a stage with no further transitions.
	NodeKindTerminal NodeKind = "terminal"
)

// Node is one vertex in the workflow graph.
type Node struct {
	ID      string   `json:"id"`
	Kind    NodeKind `json:"kind"`
	Label   string   `json:"label"`
	Phase   string   `json:"phase,omitempty"`  // Queued|Analysing|Implementing|CI+Review|Done+Flagged
	Summary string   `json:"summary"`          // one sentence — what this node means
	Detail  string   `json:"detail,omitempty"` // optional short paragraph for the explainer
	Where   string   `json:"where,omitempty"`  // code anchor, e.g. "orchestrator.go:processPR"
}

// EdgeKind describes how an edge should be rendered.
type EdgeKind string

const (
	// EdgeKindNormal is a standard forward transition.
	EdgeKindNormal EdgeKind = "normal"
	// EdgeKindDecision is the result of a yes/no or multi-way branch.
	EdgeKindDecision EdgeKind = "decision"
	// EdgeKindBack is a loop-back edge (rendered dashed/orange).
	EdgeKindBack EdgeKind = "back"
)

// Edge is a directed transition between two nodes.
type Edge struct {
	From  string   `json:"from"`
	To    string   `json:"to"`
	Label string   `json:"label,omitempty"` // the condition, phrased as the answer
	Kind  EdgeKind `json:"kind"`
}

// Graph is the complete workflow description.
type Graph struct {
	Nodes   []Node `json:"nodes"`
	Edges   []Edge `json:"edges"`
	EntryID string `json:"entryId"`
}

// Spec returns the canonical workflow graph.
//
// The graph is pure data — it describes what the tool does and why. It does
// not drive the orchestrator; spec_test.go verifies it stays consistent with
// the code (every models.PRStage constant must appear as a non-decision node).
//
// Decision nodes (kind="decision") are logical branch points rendered as
// diamonds; they have no corresponding models.PRStage value.
func Spec() Graph {
	return Graph{
		EntryID: string(models.StagePending),
		Nodes: []Node{

			// ── Stage nodes (one per models.PRStage constant) ──────────────

			// Entry
			{
				ID:      string(models.StagePending),
				Kind:    NodeKindEntry,
				Label:   "Pending",
				Phase:   "Queued",
				Summary: "Starting point — each scan cycle begins here; version metadata and initial CI are recorded.",
				Where:   "orchestrator.go:processPR",
			},

			// Transient stages
			{
				ID:      string(models.StageSettling),
				Kind:    NodeKindTransient,
				Label:   "CI settling",
				Phase:   "Queued",
				Summary: "CI is still running; the PR will be revisited next scan cycle.",
				Detail:  "A check that has been pending past the staleness threshold no longer blocks — it cannot hide the PR forever. Ignored checks are not waited on.",
				Where:   "orchestrator.go:processPR step 3 (pr.CI.Settled)",
			},
			{
				ID:      string(models.StageAnalysing),
				Kind:    NodeKindTransient,
				Label:   "Analysing",
				Phase:   "Analysing",
				Summary: "The analyser is fetching upstream changelog/release notes and assessing the bump.",
				Detail:  "For non-grouped PRs: fetch package metadata, check codebase usage, then call the Claude analyser. For grouped PRs: analyse the combined diff and member list.",
				Where:   "orchestrator.go:processPR step 6",
			},
			{
				ID:      string(models.StageImplStarting),
				Kind:    NodeKindTransient,
				Label:   "Impl start",
				Phase:   "Implementing",
				Summary: "Cloning the repository and creating a working branch for the implementation agent.",
				Where:   "implementation.go:Pipeline.Run",
			},
			{
				ID:      string(models.StageWaitingCI),
				Kind:    NodeKindTransient,
				Label:   "Waiting CI",
				Phase:   "CI+Review",
				Summary: "Waiting for CI checks to complete after the agent committed.",
				Detail:  "The orchestrator polls until CI settles. Unsettled CI does not consume a fix iteration — the worker is not resumed until there are real failures to report.",
				Where:   "implementation.go:Pipeline.Run (verifyCI loop)",
			},

			// Active stages
			{
				ID:      string(models.StageImplRunning),
				Kind:    NodeKindActive,
				Label:   "Impl running",
				Phase:   "Implementing",
				Summary: "The implementation agent is making the code changes (first turn).",
				Where:   "implementation.go:Pipeline.Run:turn1",
			},
			{
				ID:      string(models.StageResuming),
				Kind:    NodeKindActive,
				Label:   "Impl resuming",
				Phase:   "Implementing",
				Summary: "The agent is resuming its session with CI failure logs or reviewer feedback.",
				Detail:  "The same Claude session is resumed rather than started fresh — context is preserved across iterations. Bounded by MaxImplIterations and MaxImplTime.",
				Where:   "implementation.go:Pipeline.Run:resumeTurn",
			},
			{
				ID:      string(models.StageReviewing),
				Kind:    NodeKindActive,
				Label:   "Reviewing",
				Phase:   "CI+Review",
				Summary: "The reviewer agent is judging the quality and correctness of the agent's changes.",
				Where:   "implementation.go:Pipeline.Run:reviewGate",
			},

			// Terminal stages
			{
				ID:      string(models.StageApproved),
				Kind:    NodeKindTerminal,
				Label:   "Recommended",
				Phase:   "Done+Flagged",
				Summary: "The bump looks correct as-is; the tool posts a recommendation for a maintainer to merge.",
				Detail:  "The tool proposes, it does not approve: a comment with the assessment is posted and a human decides whether to merge. It never submits a native GitHub APPROVE review (which could trigger auto-merge with no human in the loop). Idempotent — the sticky comment is edited in place, not reposted.",
				Where:   "orchestrator.go:actOnAnalysis:approve",
			},
			{
				ID:      string(models.StageFinalized),
				Kind:    NodeKindTerminal,
				Label:   "Finalized",
				Phase:   "Done+Flagged",
				Summary: "A replacement PR containing the code fix has been opened and the original was closed.",
				Detail:  "The agent's work is squashed into a single 'fix:' commit on top of the dependabot bump commit (two-commit structure), pushed, and a new PR is opened. The original dependabot PR is closed with a reference to the replacement.",
				Where:   "orchestrator.go:actOnAnalysis + implementation.go:squashBranch",
			},
			{
				ID:      string(models.StageSkipped),
				Kind:    NodeKindTerminal,
				Label:   "Skipped",
				Phase:   "Done+Flagged",
				Summary: "No action taken — stale superseded PR, already-processed SHA, or dry run.",
				Where:   "orchestrator.go:processPR",
			},
			{
				ID:      string(models.StageFlagged),
				Kind:    NodeKindTerminal,
				Label:   "Flagged",
				Phase:   "Done+Flagged",
				Summary: "Needs human attention — pre-existing CI failures, low confidence, implementation failure, or review exhausted.",
				Detail:  "The Principle: if there's no high-confidence insight, say nothing. Flagging posts a concise one-line note only when confidence is high enough to be useful.",
				Where:   "orchestrator.go:actOnAnalysis + implementation.go:Pipeline.Run",
			},
			{
				ID:      string(models.StageGaveUp),
				Kind:    NodeKindTerminal,
				Label:   "Gave up",
				Phase:   "Done+Flagged",
				Summary: "Gave up trying to fix CI — the same failures persisted beyond the iteration or time cap.",
				Detail:  "Reached when the CI-fix loop exhausts its iteration/time cap, trips the no-progress guard, or post-squash CI fails to settle. A concise one-line reason is posted and the outcome is recorded sticky at the head SHA, so the next scan skips the PR until dependabot pushes a new commit.",
				Where:   "implementation.go:decideCIFixLoop → ciFixGiveUp",
			},
			{
				ID:      string(models.StageError),
				Kind:    NodeKindTerminal,
				Label:   "Error",
				Phase:   "Done+Flagged",
				Summary: "Unexpected error (e.g. analysis API failure); the PR will be retried next cycle.",
				Where:   "orchestrator.go:processPR (analysis error path)",
			},

			// ── Decision nodes (rendered as diamonds) ──────────────────────
			{
				ID:      "dec_early_exit",
				Kind:    NodeKindDecision,
				Label:   "Skip?",
				Summary: "Is this a stale superseded PR, an already-processed SHA, or a dry run?",
				Where:   "orchestrator.go:processPR",
			},
			{
				ID:      "dec_ci_settled",
				Kind:    NodeKindDecision,
				Label:   "CI settled?",
				Summary: "Are all non-ignored CI checks in a terminal state (not still running)?",
				Where:   "orchestrator.go:processPR step 3 (pr.CI.Settled)",
			},
			{
				ID:      "dec_analysis_routing",
				Kind:    NodeKindDecision,
				Label:   "Verdict?",
				Summary: "Based on CI state, confidence, and analyser verdict: approve, fix, flag, or error?",
				Detail:  "Routing priority: (1) CI not acceptable + verdict≠needs_changes → flag. (2) Low confidence → flag. (3) needs_human_review → flag. (4) approve → recommend merge (comment, not an approval). (5) needs_changes → impl path.",
				Where:   "orchestrator.go:actOnAnalysis",
			},
			{
				ID:      "dec_replacement_exists",
				Kind:    NodeKindDecision,
				Label:   "Replacement exists?",
				Summary: "Does a replacement PR already exist for this branch (idempotency guard)?",
				Where:   "orchestrator.go:actOnAnalysis (FindPRByBranch)",
			},
			{
				ID:      "dec_ci_gate",
				Kind:    NodeKindDecision,
				Label:   "CI ok?",
				Summary: "After the agent commits: is CI acceptable, still running, or has the loop hit its cap?",
				Where:   "implementation.go:Pipeline.Run CI-fix loop",
			},
			{
				ID:      "dec_review_gate",
				Kind:    NodeKindDecision,
				Label:   "Review?",
				Summary: "Reviewer verdict: approve the changes, or request changes (with retries remaining)?",
				Where:   "implementation.go:Pipeline.Run review gate",
			},
		},
		Edges: []Edge{
			// Entry → early-exit check
			{From: "pending", To: "dec_early_exit", Kind: EdgeKindNormal},

			// Early-exit outcomes
			{From: "dec_early_exit", To: "skipped", Label: "stale / already-processed / dry-run", Kind: EdgeKindDecision},
			{From: "dec_early_exit", To: "dec_ci_settled", Label: "none of the above", Kind: EdgeKindDecision},

			// CI-settled gate
			{From: "dec_ci_settled", To: "ci_settling", Label: "CI still running", Kind: EdgeKindDecision},
			{From: "dec_ci_settled", To: "analysing", Label: "CI settled", Kind: EdgeKindDecision},

			// Analysis
			{From: "analysing", To: "error", Label: "analysis error", Kind: EdgeKindNormal},
			{From: "analysing", To: "dec_analysis_routing", Kind: EdgeKindNormal},

			// Analysis routing
			{From: "dec_analysis_routing", To: "flagged_human", Label: "CI failing (pre-existing) / low confidence / needs_human_review", Kind: EdgeKindDecision},
			{From: "dec_analysis_routing", To: "approved", Label: "recommend merge", Kind: EdgeKindDecision},
			{From: "dec_analysis_routing", To: "dec_replacement_exists", Label: "needs_changes", Kind: EdgeKindDecision},

			// Replacement idempotency
			{From: "dec_replacement_exists", To: "finalized", Label: "replacement already exists", Kind: EdgeKindDecision},
			{From: "dec_replacement_exists", To: "impl_starting", Label: "no replacement yet", Kind: EdgeKindDecision},

			// Impl pipeline
			{From: "impl_starting", To: "impl_running", Kind: EdgeKindNormal},
			{From: "impl_running", To: "dec_ci_gate", Kind: EdgeKindNormal},

			// CI gate: three outcomes
			{From: "dec_ci_gate", To: "waiting_ci", Label: "CI still running", Kind: EdgeKindDecision},
			{From: "waiting_ci", To: "dec_ci_gate", Label: "poll", Kind: EdgeKindBack},
			{From: "dec_ci_gate", To: "impl_resuming", Label: "CI settled, not acceptable, retries remain", Kind: EdgeKindDecision},
			{From: "impl_resuming", To: "dec_ci_gate", Label: "resume turn complete", Kind: EdgeKindBack},
			{From: "dec_ci_gate", To: "gave_up", Label: "iterations / time cap reached", Kind: EdgeKindDecision},
			{From: "dec_ci_gate", To: "dec_review_gate", Label: "CI acceptable", Kind: EdgeKindDecision},

			// Review gate
			{From: "dec_review_gate", To: "finalized", Label: "reviewer approves", Kind: EdgeKindDecision},
			{From: "dec_review_gate", To: "impl_resuming", Label: "reviewer requests changes, retries remain", Kind: EdgeKindBack},
			{From: "dec_review_gate", To: "flagged_human", Label: "reviewer requests changes, retries exhausted", Kind: EdgeKindDecision},
		},
	}
}
