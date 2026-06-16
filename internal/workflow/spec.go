// Package workflow defines the canonical workflow graph — the single source of
// truth for what the tool does. The spec_test.go verifies the graph stays
// consistent with the real models.PRStage constants, so adding a new stage to
// the code without updating this file fails the build.
//
// Post-Q10 state machine: the separate analyser is eliminated. Every engaged
// PR goes through a single combined agentic step (analyse + decide + implement
// if needed). The old two-step flow (analyse then route to impl) is replaced by
// one flow where the agent itself decides the outcome.
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
	Phase   string   `json:"phase,omitempty"`  // Queued|Agent|Implementing|CI+Review|Done+Flagged
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

			// Stage nodes (one per models.PRStage constant)

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
				Label:   "Agent running",
				Phase:   "Agent",
				Summary: "The combined agent is analysing the upstream changes and codebase impact, then deciding what to do.",
				Detail:  "A single agentic step (Q10): the agent has a live repo checkout and full tool access. It fetches upstream data, searches the codebase, and ends in: recommend (comment with WHY, required-CI mechanically re-gated), finalized (replacement PR with justification), flagged_human (concise reason), or gave_up (silent draft). Green required-CI alone is not sufficient to recommend — the agent must reason about upstream changes.",
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
				Summary: "The combined agent is making code changes (first turn).",
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
				Summary: "The independent reviewer agent is judging the code changes and the justification (Q15).",
				Where:   "implementation.go:Pipeline.Run:reviewGate",
			},

			// Terminal stages
			{
				ID:      string(models.StageApproved),
				Kind:    NodeKindTerminal,
				Label:   "Recommended",
				Phase:   "Done+Flagged",
				Summary: "The bump looks correct as-is; the tool posts a recommendation (with a concrete WHY) after re-verifying required-CI is green.",
				Detail:  "The tool proposes, it does not approve: a comment with the agent's WHY is posted and a human decides whether to merge. It never submits a native GitHub APPROVE review. Idempotent — the sticky comment is edited in place, not reposted. The recommend outcome is re-gated by a fresh mechanical required-CI read in the orchestrator — never by the agent's self-report (Q4/C3).",
				Where:   "orchestrator.go:actOnAgentVerdict:recommend",
			},
			{
				ID:      string(models.StageFinalized),
				Kind:    NodeKindTerminal,
				Label:   "Finalized",
				Phase:   "Done+Flagged",
				Summary: "A replacement PR has been opened (draft to ready) and the original closed. Justification posted to PR body.",
				Detail:  "The agent curates its work into logical commits on top of the bump commit (Q13). The reviewer approves both code and justification (Q15). The PR is un-drafted and the original closed. The justification is posted to the PR body only on final approval — private through the impl/reviewer loop.",
				Where:   "orchestrator.go:actOnAgentVerdict + implementation.go:curateBranch",
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
				Summary: "Needs human attention — the agent has a concise, purpose-built reason it cannot resolve autonomously.",
				Detail:  "Every human-attention flag must carry a concise, purpose-built explanation — never a review-body dump. This is a last resort, emitted only when the agent has a specific insight it cannot handle itself.",
				Where:   "orchestrator.go:actOnAgentVerdict + implementation.go:Pipeline.Run",
			},
			{
				ID:      string(models.StageGaveUp),
				Kind:    NodeKindTerminal,
				Label:   "Gave up",
				Phase:   "Done+Flagged",
				Summary: "Gave up trying to fix CI. A silent draft is left open; no comment is posted.",
				Detail:  "Reached when the CI-fix loop exhausts its iteration/time cap, trips the no-progress guard, or post-curate CI fails. A sticky gave_up outcome is recorded at the post-rebase tip SHA (not the scan-time SHA, which drifts after rebase), so the next scan skips this PR until dependabot pushes a new commit. The replacement stays a silent draft — no noise.",
				Where:   "implementation.go:decideCIFixLoop -> ciFixGiveUp",
			},
			{
				ID:      string(models.StageError),
				Kind:    NodeKindTerminal,
				Label:   "Error",
				Phase:   "Done+Flagged",
				Summary: "Unexpected error; the PR will be retried next cycle.",
				Where:   "orchestrator.go:processPR (error path)",
			},

			// Decision nodes (rendered as diamonds)
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
				ID:      "dec_agent_verdict",
				Kind:    NodeKindDecision,
				Label:   "Agent verdict?",
				Summary: "What did the combined agent decide? Recommend, replacement PR, flag, or gave up?",
				Detail:  "The combined agent analyses upstream changes, searches the codebase, and ends in one of four outcomes. Recommend: required-CI green + agent judged no change needed (re-gated mechanically by orchestrator). Replacement PR: changes needed. Flag: specific insight it cannot resolve. Gave-up/silent-draft: could not reach a confident verdict.",
				Where:   "orchestrator.go:actOnAgentVerdict",
			},
			{
				ID:      "dec_replacement_exists",
				Kind:    NodeKindDecision,
				Label:   "Replacement exists?",
				Summary: "Does a replacement PR already exist for this branch (idempotency guard)?",
				Where:   "orchestrator.go:actOnAgentVerdict (FindPRByBranch)",
			},
			{
				ID:      "dec_ci_gate",
				Kind:    NodeKindDecision,
				Label:   "CI ok?",
				Summary: "After the agent commits: are required CI checks acceptable, still running, or has the loop hit its cap?",
				Where:   "implementation.go:Pipeline.Run CI-fix loop",
			},
			{
				ID:      "dec_review_gate",
				Kind:    NodeKindDecision,
				Label:   "Review?",
				Summary: "Reviewer verdict on code + justification: approve, or request changes (retries remaining)?",
				Where:   "implementation.go:Pipeline.Run review gate",
			},
		},
		Edges: []Edge{
			// Entry to early-exit check
			{From: "pending", To: "dec_early_exit", Kind: EdgeKindNormal},

			// Early-exit outcomes
			{From: "dec_early_exit", To: "skipped", Label: "stale / already-processed / dry-run", Kind: EdgeKindDecision},
			{From: "dec_early_exit", To: "dec_ci_settled", Label: "none of the above", Kind: EdgeKindDecision},

			// CI-settled gate
			{From: "dec_ci_settled", To: "ci_settling", Label: "CI still running", Kind: EdgeKindDecision},
			{From: "dec_ci_settled", To: "analysing", Label: "CI settled", Kind: EdgeKindDecision},

			// Combined agent
			{From: "analysing", To: "error", Label: "agent error", Kind: EdgeKindNormal},
			{From: "analysing", To: "dec_agent_verdict", Kind: EdgeKindNormal},

			// Agent verdict routing
			{From: "dec_agent_verdict", To: "flagged_human", Label: "flag for human (concise reason)", Kind: EdgeKindDecision},
			{From: "dec_agent_verdict", To: "gave_up", Label: "gave up / silent draft", Kind: EdgeKindDecision},
			{From: "dec_agent_verdict", To: "approved", Label: "recommend merge (required-CI re-gated by orchestrator)", Kind: EdgeKindDecision},
			{From: "dec_agent_verdict", To: "dec_replacement_exists", Label: "needs changes -> impl path", Kind: EdgeKindDecision},

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
			{From: "dec_review_gate", To: "finalized", Label: "reviewer approves code + justification", Kind: EdgeKindDecision},
			{From: "dec_review_gate", To: "impl_resuming", Label: "reviewer requests changes, retries remain", Kind: EdgeKindBack},
			{From: "dec_review_gate", To: "flagged_human", Label: "reviewer requests changes, retries exhausted", Kind: EdgeKindDecision},
		},
	}
}
