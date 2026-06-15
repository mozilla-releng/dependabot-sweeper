// Package models defines data types used across the orchestrator.
package models

import "time"

// BumpType classifies a semver version change.
type BumpType string

const (
	BumpPatch   BumpType = "patch"
	BumpMinor   BumpType = "minor"
	BumpMajor   BumpType = "major"
	BumpUnknown BumpType = "unknown"
)

// BumpRank orders bump severity for policy comparisons: major > minor > patch >
// unknown. A higher rank is more significant. unknown ranks lowest on purpose,
// so any min-bump-to-engage threshold (including the default `major`) skips
// PRs whose title didn't parse into a recognised bump (Q5/Q14): processing an
// unparseable-title PR would otherwise reach the expensive agent unintentionally.
func BumpRank(b BumpType) int {
	switch b {
	case BumpMajor:
		return 3
	case BumpMinor:
		return 2
	case BumpPatch:
		return 1
	default: // BumpUnknown and anything unrecognised
		return 0
	}
}

// Recommendation is the analysis agent's verdict on a dependency bump.
type Recommendation string

const (
	RecommendApprove          Recommendation = "approve"
	RecommendNeedsChanges     Recommendation = "needs_changes"
	RecommendNeedsHumanReview Recommendation = "needs_human_review"
)

// Confidence is the analysis agent's confidence level.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Action is what the orchestrator did with a PR.
type Action string

const (
	ActionApproved        Action = "approved"
	ActionClosedStale     Action = "closed_stale"
	ActionReplacementPR   Action = "replacement_pr"
	ActionFlaggedForHuman Action = "flagged_for_human"
	ActionSkippedPolicy   Action = "skipped_policy" // bump below min-bump-to-engage (Q5)
	ActionSkippedPending  Action = "skipped_ci_pending"
	ActionSkippedNoChange Action = "skipped_no_change"
	ActionError           Action = "error"
	ActionDryRun          Action = "dry_run"
)

// CheckDetail is a single CI check result.
type CheckDetail struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`     // "completed", "in_progress", "queued", "pending"(legacy)
	Conclusion *string   `json:"conclusion"` // "success","failure","neutral","skipped","stale","timed_out","cancelled",...
	DetailsURL string    `json:"details_url,omitempty"`
	CreatedAt  time.Time `json:"created_at"`       // staleness clock: check-run StartedAt, or legacy status CreatedAt
	Output     string    `json:"output,omitempty"` // generic failure detail (summary+text); populated for failures
}

// isTerminal reports whether a check has reached a final state (any conclusion,
// incl. GitHub's own stale/timed_out/cancelled), generically. A check run is
// terminal once its Status == "completed"; a legacy commit status is terminal
// once its state is anything other than the pending/queued/in_progress states.
func (cd CheckDetail) isTerminal() bool {
	if cd.Status == "completed" { // modern check runs
		return true
	}
	if cd.Status != "" && cd.Status != "in_progress" && cd.Status != "queued" && cd.Status != "pending" {
		return true // legacy non-pending state
	}
	return false
}

// isTerminalFailure reports whether a check has settled into a failing state.
// Generic across modern check runs (Status=="completed" with a "failure"
// conclusion) and legacy commit statuses ("failure"/"error" states).
func (cd CheckDetail) isTerminalFailure() bool {
	if cd.Status == "completed" {
		return cd.Conclusion != nil && *cd.Conclusion == "failure"
	}
	if cd.Conclusion != nil && (*cd.Conclusion == "failure" || *cd.Conclusion == "error") {
		return true
	}
	return false
}

// DeriveFailures returns the subset of checks that have settled into a failing
// state. Used to reconstruct CIStatus.Failures from a stored Checks slice (the
// store persists individual checks but not the derived Failures list).
func DeriveFailures(checks []CheckDetail) []CheckDetail {
	var failures []CheckDetail
	for _, c := range checks {
		if c.isTerminalFailure() {
			failures = append(failures, c)
		}
	}
	return failures
}

// Settled reports whether every check is terminal or stale (pending longer than
// `staleness` since CreatedAt), so the PR can be triaged now; plus the names of
// checks still pending-and-not-yet-stale (why we'd skip-and-revisit otherwise).
// Pure; clock injected. Fixes the old failure>pending precedence — an early-red
// sibling no longer implies the PR is settled while others still run. Checks in
// `ignored` are not waited on: if we won't gate on a check's result (it's in the
// operator's ignore-list), there's no reason to delay settledness for it — this
// keeps slow, bump-irrelevant checks (e.g. unrelated build matrices) from
// stretching every CI cycle. (Bug #21 follow-up.)
func Settled(checks []CheckDetail, now time.Time, staleness time.Duration, ignored map[string]bool) (settled bool, pendingNotStale []string) {
	for _, c := range checks {
		if ignored[c.Name] {
			continue // won't gate on it, so don't wait for it
		}
		if c.isTerminal() {
			continue
		}
		if !c.CreatedAt.IsZero() && now.Sub(c.CreatedAt) >= staleness {
			continue // stale → no longer blocks settledness
		}
		pendingNotStale = append(pendingNotStale, c.Name)
	}
	return len(pendingNotStale) == 0, pendingNotStale
}

// CIStatus is the aggregated CI status from check runs and legacy statuses.
type CIStatus struct {
	State    string        `json:"state"` // "success", "failure", "pending", "unknown" (diagnostics only; no longer the gate)
	Total    int           `json:"total"`
	Passed   int           `json:"passed"`
	Failed   int           `json:"failed"`
	Pending  int           `json:"pending"`
	Failures []CheckDetail `json:"failures"`         // back-compat/diagnostics: terminal-failing checks only
	Checks   []CheckDetail `json:"checks,omitempty"` // ALL checks (terminal + pending); drives Settled/AcceptableGiven
}

// Settled reports whether the PR's CI is settled enough to triage now, plus the
// names of checks still pending-and-not-yet-stale. Wraps the pure Settled over
// the full check list. Clock injected. `ignored` checks are not waited on.
func (ci CIStatus) Settled(now time.Time, staleness time.Duration, ignored map[string]bool) (bool, []string) {
	return Settled(ci.Checks, now, staleness, ignored)
}

// FailureNames returns the names of failed checks.
func (ci CIStatus) FailureNames() []string {
	names := make([]string, 0, len(ci.Failures))
	for _, f := range ci.Failures {
		names = append(names, f.Name)
	}
	return names
}

// AcceptableGiven reports whether CI is good enough to merge, given ignored
// checks + base-branch failures + the required-checks set + the staleness clock.
// It reasons over the full per-check list (ci.Checks), not the aggregate State.
//
//   - Required-checks gating (Q7): when `required` is non-empty, only checks in
//     that set can block — the repo's merge gate ignores everything else, so the
//     tool must too ("CI passing" ≡ "required checks passing"). When `required`
//     is empty (branch protection unconfigured, has no required set, or was
//     unreadable), it falls back to ALL checks (review M2) — a vacuously-true
//     "required passing" must never let an all-red PR read as acceptable.
//   - A terminal-failing check blocks unless it is named in `ignored` (an
//     operator-supplied list of known-noisy/structural checks) or `baseFailures`
//     (already failing on the base branch, so the change didn't introduce it).
//   - A *stale* pending check (pending past `staleness` since CreatedAt) blocks
//     unless it is ignored — "ignore" thus means "never blocks, failing OR
//     stuck". Stale-but-blocking names are returned suffixed " (stuck)".
//   - A still-pending-not-stale check should never reach here (the Settled()
//     gate skips the PR first); defensively it is treated as blocking.
//
// Returns the list of genuinely-blocking check names for diagnostics. Pure;
// clock injected.
//
// This is the success criterion for an implementation attempt: real repos
// routinely carry pre-existing red checks unrelated to a dependency bump, so
// requiring every check to be green would make success unreachable (Bug #7).
func (ci CIStatus) AcceptableGiven(ignored, baseFailures, required map[string]bool, now time.Time, staleness time.Duration) (bool, []string) {
	gateOnRequired := len(required) > 0
	var blocking []string
	for _, c := range ci.Checks {
		name := c.Name
		// Under required-checks gating a non-required check can never block — it
		// is not part of the repo's merge gate (Q7). Keyed on the bare name (the
		// " (stuck)" suffix is added below, after this filter).
		if gateOnRequired && !required[name] {
			continue
		}
		switch {
		case c.isTerminalFailure():
			if ignored[name] || baseFailures[name] {
				continue
			}
			blocking = append(blocking, name)
		case !c.isTerminal(): // pending
			if ignored[name] {
				continue
			}
			if !c.CreatedAt.IsZero() && now.Sub(c.CreatedAt) >= staleness {
				blocking = append(blocking, name+" (stuck)")
			} else {
				blocking = append(blocking, name) // not stale yet — defensive
			}
		}
	}
	return len(blocking) == 0, blocking
}

// DependabotPR is a dependabot PR with parsed metadata.
type DependabotPR struct {
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Author      string   `json:"author"` // GitHub login of the PR creator
	PackageName string   `json:"package_name"`
	Ecosystem   string   `json:"ecosystem"`
	OldVersion  string   `json:"old_version"`
	NewVersion  string   `json:"new_version"`
	BumpType    BumpType `json:"bump_type"`
	CI          CIStatus `json:"ci"`
	Diff        string   `json:"diff"`
	URL         string   `json:"url"`
	HeadSHA     string   `json:"head_sha"`
	HeadRef     string   `json:"head_ref"`
	BaseRef     string   `json:"base_ref"`

	// Grouped is true for a dependabot "grouped update" PR that bumps several
	// packages at once (title like "bump the X group with N updates"). Such a
	// PR has no single package/version, so PackageName holds the group name and
	// OldVersion/NewVersion are empty; the individual bumps are in GroupedUpdates.
	Grouped        bool          `json:"grouped,omitempty"`
	GroupedUpdates []PackageBump `json:"grouped_updates,omitempty"`
}

// PackageBump is a single package's version change within a grouped update.
type PackageBump struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

// UpstreamInfo holds upstream changelog and release information.
type UpstreamInfo struct {
	Releases         []Release `json:"releases"`
	ChangelogSnippet string    `json:"changelog_snippet"`
	RepoURL          string    `json:"repo_url"`
}

// Release is a single upstream GitHub release.
type Release struct {
	Tag  string `json:"tag"`
	Name string `json:"name"`
	Body string `json:"body"`
}

// CodebaseUsage describes how the target repo uses a dependency.
type CodebaseUsage struct {
	ImportFiles   []string       `json:"import_files"`
	UsageSnippets []UsageSnippet `json:"usage_snippets"`
}

// UsageSnippet is a single code location using a dependency.
type UsageSnippet struct {
	File    string `json:"file"`
	Line    string `json:"line"`
	Content string `json:"content"`
}

// AgentAnalysis is the structured output from the Claude analysis.
type AgentAnalysis struct {
	BreakingChanges []string          `json:"breaking_changes"`
	Deprecations    []string          `json:"deprecations"`
	CodebaseImpact  []CodeImpact      `json:"codebase_impact"`
	Recommendation  Recommendation    `json:"recommendation"`
	Confidence      Confidence        `json:"confidence"`
	ReviewBody      string            `json:"review_body"`
	CodeChanges     []CodeChangeEntry `json:"code_changes"`
}

// CodeImpact describes how a breaking change affects a specific file.
type CodeImpact struct {
	File     string `json:"file"`
	Usage    string `json:"usage"`
	Affected bool   `json:"affected"`
	Detail   string `json:"detail"`
}

// CodeChangeEntry describes a code change needed in a specific file.
type CodeChangeEntry struct {
	File        string `json:"file"`
	Description string `json:"description"`
}

// ReviewVerdict is the review agent's assessment of implementation quality.
type ReviewVerdict struct {
	Verdict  string   `json:"verdict"` // "approve" or "request_changes"
	Concerns []string `json:"concerns"`
}

// CommitInfo is a commit on the implementation branch, surfaced to the reviewer.
type CommitInfo struct {
	SHA      string `json:"sha"`
	Message  string `json:"message"`
	DiffStat string `json:"diff_stat"`
}

// ReviewResult is the result of processing a single dependabot PR.
type ReviewResult struct {
	PRNumber            int    `json:"pr_number"`
	PackageName         string `json:"package_name"`
	OldVersion          string `json:"old_version"`
	NewVersion          string `json:"new_version"`
	Action              Action `json:"action"`
	Detail              string `json:"detail"`
	ReplacementPRNumber *int   `json:"replacement_pr_number,omitempty"`
	Success             bool   `json:"success"`
	Error               string `json:"error,omitempty"`
}

// PRStage is a coarse pipeline stage for a single PR, surfaced live on the
// dashboard. The string values are a stable wire contract consumed by the
// dashboard's JavaScript and any SSE client — do not rename without updating
// both ends (and TestPRStageConstants will fail if you try).
type PRStage string

const (
	StagePending      PRStage = "pending"
	StageAnalysing    PRStage = "analysing"
	StageApproved     PRStage = "approved"
	StageImplStarting PRStage = "impl_starting"
	StageImplRunning  PRStage = "impl_running" // worker subprocess live
	StageWaitingCI    PRStage = "waiting_ci"
	StageResuming     PRStage = "impl_resuming" // CI failed, resuming worker
	StageReviewing    PRStage = "reviewing"
	StageFinalized    PRStage = "finalized"
	StageFlagged      PRStage = "flagged_human"
	StageGaveUp       PRStage = "gave_up"
	StageSkipped      PRStage = "skipped"
	StageSettling     PRStage = "ci_settling" // CI still in flight; will revisit next cycle
	StageError        PRStage = "error"
)

// AllPRStages is the complete, ordered list of every PRStage constant.
// When adding a new stage: add the constant above AND add it here — the
// workflow spec test (internal/workflow/spec_test.go) checks this list against
// the spec and fails the build if any stage is missing from the spec.
var AllPRStages = []PRStage{
	StagePending,
	StageSettling,
	StageAnalysing,
	StageImplStarting,
	StageImplRunning,
	StageWaitingCI,
	StageResuming,
	StageReviewing,
	StageApproved,
	StageFinalized,
	StageSkipped,
	StageFlagged,
	StageGaveUp,
	StageError,
}

// StageEvent is one entry in a PR's stage-transition timeline.
type StageEvent struct {
	Stage  PRStage   `json:"stage"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail"`
}

// PRProgress is the persisted view of one PR moving through the pipeline.
// Callers receive value copies from Store.Get/Store.All, so PRProgress itself
// carries no synchronisation.
type PRProgress struct {
	PRNumber    int     `json:"pr_number"`
	PackageName string  `json:"package_name"`
	BumpType    string  `json:"bump_type"`
	Stage       PRStage `json:"stage"`
	// SessionID and WorktreePath are internal (Claude session UUID and a local
	// server filesystem path). They are deliberately NOT serialised: the
	// dashboard is public, and exposing a session ID or an absolute server path
	// is needless disclosure. Used in-process via the Go fields only.
	SessionID     string       `json:"-"`
	WorktreePath  string       `json:"-"`
	ImplBranch    string       `json:"impl_branch,omitempty"`
	ReplacementPR *int         `json:"replacement_pr,omitempty"`
	LastUpdated   time.Time    `json:"last_updated"`
	History       []StageEvent `json:"history"`

	// Version metadata captured at scan time from DependabotPR.
	OldVersion string `json:"old_version,omitempty"`
	NewVersion string `json:"new_version,omitempty"`
	Ecosystem  string `json:"ecosystem,omitempty"`

	// CI is the latest CI snapshot. nil until SetCI is called (i.e. not yet
	// populated for PRs that skip before CI is checked). Pointer so "not yet
	// computed" is distinguishable from "computed but empty".
	CI *CIStatus `json:"ci,omitempty"`

	// Analysis is the analyser verdict. nil until SetAnalysis is called (i.e.
	// not yet populated for PRs that are skipped without being analysed).
	Analysis *AgentAnalysis `json:"analysis,omitempty"`

	// HeadSHA and Outcome record the last terminal outcome for this PR (Bug #23).
	// When non-empty, the next scan at the same HeadSHA skips re-processing via
	// a DB lookup instead of reading back a PR comment.
	HeadSHA string `json:"head_sha,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}
