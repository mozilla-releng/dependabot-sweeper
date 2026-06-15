// Package orchestrator provides the main processing loop for dependabot PRs.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/analyser"
	"github.com/mozilla-releng/dependabot-sweeper/internal/codebase"
	"github.com/mozilla-releng/dependabot-sweeper/internal/config"
	ghclient "github.com/mozilla-releng/dependabot-sweeper/internal/github"
	"github.com/mozilla-releng/dependabot-sweeper/internal/implementation"
	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/progress"
)

// Orchestrator processes dependabot PRs with bounded concurrency (config.Concurrency).
type Orchestrator struct {
	config    *config.Config
	repo      string
	dryRun    bool
	verbose   bool
	reviewers []string
	onlyPR    int // 0 means process all
	github    *ghclient.Client
	analyser  *analyser.Analyser
	store     progress.ReadWriter // optional; nil for the one-shot `review` command
	logDir    string              // forwarded to each implementation pipeline
}

// New creates an Orchestrator for the given repository. acceptAuthors are
// extra PR author logins to process alongside dependabot[bot] / renovate[bot].
func New(
	ctx context.Context,
	cfg *config.Config,
	repo string,
	dryRun, verbose bool,
	reviewers []string,
	acceptAuthors []string,
	onlyPR int,
) (*Orchestrator, error) {
	gh, err := ghclient.NewClient(ctx, cfg.GitHubToken, repo, acceptAuthors)
	if err != nil {
		return nil, fmt.Errorf("creating GitHub client: %w", err)
	}

	return &Orchestrator{
		config:    cfg,
		repo:      repo,
		dryRun:    dryRun,
		verbose:   verbose,
		reviewers: reviewers,
		onlyPR:    onlyPR,
		github:    gh,
		analyser:  analyser.NewAnalyser(cfg.AnthropicAPIKey, cfg.AnalyserModel, cfg.AnalyserThinkingBudget, verbose),
	}, nil
}

// WithStore attaches a live progress store. Used by the daemon subcommands; the
// one-shot `review` command leaves it nil. Returns the receiver for chaining.
func (o *Orchestrator) WithStore(s progress.ReadWriter) *Orchestrator {
	o.store = s
	return o
}

// WithLogDir sets the directory where implementation pipelines write per-PR
// agent JSONL logs. Passed through to every pipeline.WithLogDir call.
// Defaults to $TMPDIR/sweeper-agent-logs when not set.
func (o *Orchestrator) WithLogDir(dir string) *Orchestrator {
	o.logDir = dir
	return o
}

// reportStage is the nil-safe shim every progress update goes through.
func (o *Orchestrator) reportStage(prNumber int, pkg, bump string, stage models.PRStage, detail string) {
	if o.store != nil {
		o.store.Report(prNumber, pkg, bump, stage, detail)
	}
}

// reapClosed deletes store rows for PRs that are no longer in open. It is keyed
// off allPRs (the full open set from GitHub), not toProcess, so --pr N mode
// never prunes rows for other PRs. No-op when no store is attached.
func (o *Orchestrator) reapClosed(open []models.DependabotPR) {
	if o.store == nil {
		return
	}
	nums := make([]int, len(open))
	for i, pr := range open {
		nums[i] = pr.Number
	}
	o.store.Reap(nums)
}

// prepopulate stamps `pending` (the graph entry) for every PR the store has not
// seen before, so its row exists for the per-PR metadata writes (SetVersions /
// SetCI no-op on an unknown row). PRs already tracked are left untouched: a
// no-op cycle must record no transition (T6a) — re-stamping an already-tracked
// (often terminal) PR every cycle was the reporting-noise bug. The dashboard
// reads each PR's last real stage straight from the row. No-op when the store
// is nil (one-shot `review` mode).
func (o *Orchestrator) prepopulate(prs []models.DependabotPR) {
	if o.store == nil {
		return
	}
	for _, pr := range prs {
		if _, ok := o.store.Get(pr.Number); ok {
			continue
		}
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StagePending, "")
	}
}

// reportReplacement is the nil-safe shim for recording a replacement PR number.
func (o *Orchestrator) reportReplacement(prNumber, replacementN int) {
	if o.store != nil {
		o.store.SetReplacementPR(prNumber, replacementN)
	}
}

// setVersions is the nil-safe shim for recording version metadata.
func (o *Orchestrator) setVersions(prNumber int, oldVer, newVer, ecosystem string) {
	if o.store != nil {
		o.store.SetVersions(prNumber, oldVer, newVer, ecosystem)
	}
}

// setCI is the nil-safe shim for recording the latest CI snapshot.
func (o *Orchestrator) setCI(prNumber int, ci models.CIStatus) {
	if o.store != nil {
		o.store.SetCI(prNumber, ci)
	}
}

// setAnalysis is the nil-safe shim for recording the analyser verdict.
func (o *Orchestrator) setAnalysis(prNumber int, a models.AgentAnalysis) {
	if o.store != nil {
		o.store.SetAnalysis(prNumber, a)
	}
}

// recordOutcome records the terminal head SHA and outcome so the next scan at
// the same SHA can skip re-processing via a DB lookup (Bug #23). No-op when
// the store is nil (one-shot `review` mode — the PR-comment fallback is used
// there instead) or when headSHA is empty (retriable outcomes).
func (o *Orchestrator) recordOutcome(prNumber int, headSHA string, outcome models.PRStage) {
	if o.store != nil && headSHA != "" {
		o.store.SetOutcome(prNumber, headSHA, string(outcome))
	}
}

// Run is the main loop: fetch PRs, process each serially.
func (o *Orchestrator) Run(ctx context.Context) []models.ReviewResult {
	slog.Info("Fetching dependabot PRs", "repo", o.repo)
	allPRs, err := o.github.GetDependabotPRs(ctx)
	if err != nil {
		slog.Error("Failed to fetch dependabot PRs", "error", err)
		return nil
	}
	slog.Info("Found dependabot PRs", "count", len(allPRs))

	// `--pr N` narrows which PRs we PROCESS, but the staleness check
	// still needs to see every PR — otherwise a PR that's superseded by
	// another open one in the repo would silently be treated as live.
	toProcess := allPRs
	if o.onlyPR > 0 {
		var filtered []models.DependabotPR
		for _, pr := range allPRs {
			if pr.Number == o.onlyPR {
				filtered = append(filtered, pr)
			}
		}
		if len(filtered) == 0 {
			slog.Error("PR not found among dependabot PRs", "pr", o.onlyPR)
			return nil
		}
		toProcess = filtered
	}

	// Process PRs with bounded concurrency. Each PR is independent — its own
	// temp clone, branch, replacement PR, and implementation pipeline — and the
	// shared resources (the go-github client, the analyser, and the read-only
	// allPRs slice used for the staleness check) are all safe for concurrent
	// use. Results are written into a pre-sized slice indexed by position, so
	// the summary keeps a deterministic (input) order without a mutex.
	concurrency := max(o.config.Concurrency, 1)
	slog.Info("Processing PRs", "count", len(toProcess), "concurrency", concurrency)

	// Reap rows for PRs that are no longer open. Must run before pre-populate so
	// the dashboard never briefly shows a stale PR being re-added.
	o.reapClosed(allPRs)

	// Pre-populate the dashboard so all PRs are visible as soon as the fetch
	// phase completes, rather than appearing one-by-one as goroutines start.
	// Individual goroutines overwrite these with finer-grained stages.
	o.prepopulate(toProcess)

	results := make([]models.ReviewResult, len(toProcess))
	processed := make([]bool, len(toProcess))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, pr := range toProcess {
		if err := ctx.Err(); err != nil {
			slog.Warn("Context cancelled, not launching further PRs", "launched", i, "total", len(toProcess))
			break
		}
		sem <- struct{}{} // acquire a worker slot (blocks at capacity)
		wg.Add(1)
		go func(i int, pr models.DependabotPR) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = o.processPR(ctx, pr, allPRs)
			processed[i] = true
		}(i, pr)
	}
	wg.Wait()

	// Keep only PRs that were actually processed (a cancelled context can leave
	// trailing PRs unlaunched), preserving input order.
	out := make([]models.ReviewResult, 0, len(results))
	for i := range results {
		if processed[i] {
			out = append(out, results[i])
		}
	}
	return out
}

// belowMinBump reports whether a bump is below the configured engage threshold
// and should be skipped out of policy (Q5). unknown ranks lowest, so the default
// `major` threshold skips it — a non-bump PR from the trusted author never
// reaches the agent. Pure.
func belowMinBump(bump, min models.BumpType) bool {
	return models.BumpRank(bump) < models.BumpRank(min)
}

func (o *Orchestrator) processPR(ctx context.Context, pr models.DependabotPR, allPRs []models.DependabotPR) models.ReviewResult {
	slog.Info("Processing PR",
		"number", pr.Number,
		"package", pr.PackageName,
		"old", pr.OldVersion,
		"new", pr.NewVersion,
		"bump", pr.BumpType,
		"ci", pr.CI.State,
		"passed", pr.CI.Passed,
		"total", pr.CI.Total)
	// No `pending` re-stamp here: prepopulate() already created the row on first
	// sight, and re-stamping it every cycle was the reporting-noise bug (T6a).
	// Populate version metadata and initial CI snapshot immediately so the
	// dashboard can show old→new and CI state even for PRs that are skipped
	// early (patch, stale, not-settled, already-processed) without waiting
	// for analysis to complete.
	o.setVersions(pr.Number, pr.OldVersion, pr.NewVersion, pr.Ecosystem)
	o.setCI(pr.Number, pr.CI)

	// Step 1: Skip bumps below the per-repo engage threshold (Q5). The default
	// `major` skips passing patch/minor (dependabot auto-merges those) AND
	// `unknown`-titled PRs (BumpRank ranks unknown lowest) — so a non-bump PR
	// from the trusted author never reaches the agent. A grouped PR is ranked by
	// its highest member bump (maxGroupedBump), so a group is engaged iff that
	// max meets the threshold. The reason is recorded on the dashboard.
	if belowMinBump(pr.BumpType, o.config.MinBumpToEngage) {
		detail := fmt.Sprintf("skipped: %s bump — below the configured minimum to engage (%s)",
			pr.BumpType, o.config.MinBumpToEngage)
		slog.Info("Bump below engage threshold — skipping", "pr", pr.Number,
			"bump", pr.BumpType, "min", o.config.MinBumpToEngage)
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageSkipped, detail)
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      models.ActionSkippedPolicy,
			Detail:      detail,
			Success:     true,
		}
	}

	// Step 2: Staleness. A PR is superseded if a newer INDIVIDUAL PR bumps the
	// same package higher (existing behaviour), or (Q6) a grouped PR already
	// covers this package at >= this version. A grouped PR is never closed as
	// stale — it bumps other members too — and an individual never closes a
	// whole group; hence the !pr.Grouped guard and the directional group check.
	if !pr.Grouped {
		var detail, comment string
		if newer := ghclient.FindNewerPRForPackage(pr, allPRs); newer != nil {
			detail = fmt.Sprintf("Superseded by #%d (%s → %s)", newer.Number, newer.OldVersion, newer.NewVersion)
			comment = fmt.Sprintf("Closing as stale — this PR is superseded by #%d "+
				"which bumps %s to a newer version (%s).\n\n_Automated review._",
				newer.Number, pr.PackageName, newer.NewVersion)
		} else if group := ghclient.FindSupersedingGroup(pr, allPRs); group != nil {
			detail = fmt.Sprintf("Superseded by group #%d", group.Number)
			comment = fmt.Sprintf("Closing as stale — the grouped update #%d already includes %s "+
				"at this version or newer.\n\n_Automated review._",
				group.Number, pr.PackageName)
		}
		if detail != "" {
			slog.Info("Stale PR", "pr", pr.Number, "detail", detail)
			if !o.dryRun {
				if err := o.github.ClosePRWithComment(ctx, pr.Number, comment); err != nil {
					slog.Warn("failed to close stale PR", "pr", pr.Number, "error", err)
				}
			}
			action := models.ActionClosedStale
			if o.dryRun {
				action = models.ActionDryRun
			}
			o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageSkipped, detail)
			return models.ReviewResult{
				PRNumber:    pr.Number,
				PackageName: pr.PackageName,
				OldVersion:  pr.OldVersion,
				NewVersion:  pr.NewVersion,
				Action:      action,
				Detail:      detail,
				Success:     true,
			}
		}
	}

	// CI indeterminate: getCIStatus returns State "unknown" only when the check
	// runs couldn't be fetched (API error). Do NOT proceed — an empty/partial
	// check set is vacuously "settled" and would read as green, which on this
	// path can lead to an erroneous approve. Skip and revisit next cycle.
	if pr.CI.State == "unknown" {
		slog.Warn("CI status indeterminate (fetch failed) — skipping, will revisit", "pr", pr.Number)
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageSettling, "CI status could not be fetched — will retry next cycle")
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      models.ActionSkippedPending,
			Detail:      "CI status could not be fetched — will retry next cycle",
			Success:     true,
		}
	}

	// Step 3: CI settledness gate. As a level-triggered cron we skip a PR whose
	// CI is still running and revisit next cycle — but a check that has been
	// pending past the staleness threshold no longer blocks (it can't hide the
	// PR forever), and ignored checks aren't waited on at all. Settled() also
	// fixes the old failure>pending precedence: an early-red check no longer
	// makes a mid-flight PR look settled (Bug #18).
	ignoredForSettle := make(map[string]bool, len(o.config.IgnoreChecks))
	for _, name := range o.config.IgnoreChecks {
		ignoredForSettle[name] = true
	}
	if settled, pendingNames := pr.CI.Settled(time.Now(), o.config.CIStaleness, ignoredForSettle); !settled {
		slog.Info("CI not settled, skipping", "pr", pr.Number, "pending", strings.Join(pendingNames, ", "))
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageSettling, "CI not settled — still pending: "+strings.Join(pendingNames, ", "))
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      models.ActionSkippedPending,
			Detail:      fmt.Sprintf("CI not settled — still pending: %s", strings.Join(pendingNames, ", ")),
			Success:     true,
		}
	}

	if pr.CI.State == "failure" {
		slog.Info("CI failing", "pr", pr.Number,
			"failed", pr.CI.Failed,
			"failures", strings.Join(pr.CI.FailureNames(), ", "))
	}

	// Idempotency: if we have already recorded a terminal outcome for this exact
	// head SHA, nothing has changed — skip to avoid re-running analysis and
	// re-touching the comment when the PR hasn't moved (Bug #23).
	//
	// Daemon path: read from the DB store (authoritative, not forge-able).
	// One-shot `review` path (store==nil): fall back to reading the SHA marker
	// from the bot's sticky PR comment (legacy, kept for nil-store compatibility).
	if !o.dryRun && pr.HeadSHA != "" {
		var alreadyProcessed bool
		if o.store != nil {
			if stored, ok := o.store.Get(pr.Number); ok && stored.Outcome != "" && stored.HeadSHA == pr.HeadSHA {
				alreadyProcessed = true
			}
		} else {
			if already, err := o.github.IsAlreadyProcessedAtSHA(ctx, pr.Number, pr.HeadSHA); err != nil {
				slog.Warn("could not check prior processing state", "pr", pr.Number, "error", err)
			} else {
				alreadyProcessed = already
			}
		}
		if alreadyProcessed {
			slog.Info("PR already processed at this head SHA — skipping", "pr", pr.Number)
			// T6a: a no-op cycle records NO transition. The row's stored stage and
			// outcome already reflect the terminal result, which the dashboard reads
			// directly — re-stamping it every cycle was the reporting-noise bug.
			return models.ReviewResult{
				PRNumber:    pr.Number,
				PackageName: pr.PackageName,
				OldVersion:  pr.OldVersion,
				NewVersion:  pr.NewVersion,
				Action:      models.ActionSkippedNoChange,
				Detail:      "Already processed at this head SHA — no change since last review",
				Success:     true,
			}
		}
	}

	// Dry run: report without calling Claude
	if o.dryRun {
		ciNote := ""
		if pr.CI.State == "failure" {
			ciNote = fmt.Sprintf(" (CI FAILING: %s)", strings.Join(pr.CI.FailureNames(), ", "))
		}
		slog.Info("Dry run — would analyse", "pr", pr.Number, "bump", pr.BumpType, "grouped", pr.Grouped)
		detail := fmt.Sprintf("%s bump — would fetch upstream info, analyse codebase, and submit review%s", pr.BumpType, ciNote)
		if pr.Grouped {
			detail = fmt.Sprintf("grouped update (%d packages) — would analyse the combined diff and submit review%s", len(pr.GroupedUpdates), ciNote)
		}
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageSkipped, "dry run")
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      models.ActionDryRun,
			Detail:      detail,
			Success:     true,
		}
	}

	// Steps 4-5: per-package upstream info + codebase usage. These don't apply
	// to a grouped update (no single package), so skip them for groups — the
	// analyser works off the combined diff and the member list instead.
	var upstream models.UpstreamInfo
	var usage models.CodebaseUsage
	if !pr.Grouped {
		slog.Info("Fetching upstream info", "pr", pr.Number)
		upstream = o.github.GetUpstreamInfo(ctx, pr.PackageName, pr.Ecosystem, pr.OldVersion, pr.NewVersion)

		slog.Info("Analysing codebase usage", "pr", pr.Number)
		u, uerr := codebase.AnalyseCodebaseUsage(ctx, o.repo, pr.PackageName, pr.Ecosystem, "")
		if uerr != nil {
			slog.Warn("Codebase analysis failed", "pr", pr.Number, "error", uerr)
		}
		usage = u
	}

	// Step 6: Fetch failing-CI logs (best-effort) so the analyser sees
	// actual error output rather than just check names. Gate on the presence of
	// failing checks, not the aggregate CI.State (which is diagnostics-only): a
	// settled PR can carry failures while State is not "failure".
	var failureLogs map[string]string
	if len(pr.CI.Failures) > 0 {
		slog.Info("Fetching failing-CI logs", "pr", pr.Number, "checks", len(pr.CI.Failures))
		failureLogs = o.github.FetchFailureLogs(ctx, pr)
	}

	// Step 7: Claude analysis
	slog.Info("Running Claude analysis", "pr", pr.Number)
	o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageAnalysing, "")
	analysis, err := o.analyser.Analyse(ctx, pr, upstream, usage, failureLogs)
	if err != nil {
		slog.Error("Analysis failed", "pr", pr.Number, "error", err)
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageError, fmt.Sprintf("analysis failed: %s", err))
		// Flag rather than drop: a PR that errors during analysis should appear
		// in the summary as flagged-for-human so it is visible, not silently
		// lost (Bug #16 fallback).
		if !o.dryRun {
			if cerr := o.github.UpsertStatusComment(ctx, pr.Number, pr.HeadSHA,
				fmt.Sprintf("Automated analysis could not complete.\n\n**Reason:** %s\n\n_Automated review._", err)); cerr != nil {
				slog.Warn("failed to post analysis-error comment", "pr", pr.Number, "error", cerr)
			}
		}
		// Sticky: record outcome so the next cycle skips without re-running the
		// analyser (Bug #26). The PR is re-tried automatically if dependabot
		// pushes a new commit (new head SHA).
		o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFlagged)
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      models.ActionFlaggedForHuman,
			Detail:      fmt.Sprintf("Analysis failed: %s", err),
			Success:     true,
		}
	}

	// Record the analyser verdict before routing so the drawer shows it for
	// PRs that are approved/flagged without entering the implementation path.
	o.setAnalysis(pr.Number, *analysis)

	// Step 7: Validate and act
	return o.actOnAnalysis(ctx, pr, analysis)
}

// suppressedChecks returns the two sets of CI checks that should not block the
// pre-implementation routing decision (Bug #17), mirroring the implementation
// pipeline's own suppressedChecks (Bug #7):
//   - ignored: operator-supplied check names (config.IgnoreChecks).
//   - baseFailures: checks already failing on the PR's base branch, so the
//     change can't be blamed for them. Best-effort — an unreachable base CI
//     just yields an empty set and we rely on the ignore-list.
func (o *Orchestrator) suppressedChecks(ctx context.Context, pr models.DependabotPR) (ignored, baseFailures map[string]bool) {
	ignored = make(map[string]bool, len(o.config.IgnoreChecks))
	for _, name := range o.config.IgnoreChecks {
		ignored[name] = true
	}

	baseFailures = make(map[string]bool)
	if pr.BaseRef == "" {
		return ignored, baseFailures
	}
	baseCI, err := o.github.GetBranchCI(ctx, pr.BaseRef)
	if err != nil {
		slog.Warn("Could not fetch base-branch CI for pre-existing-failure detection", "base", pr.BaseRef, "error", err)
		return ignored, baseFailures
	}
	for _, name := range baseCI.FailureNames() {
		baseFailures[name] = true
	}
	if len(baseFailures) > 0 {
		slog.Info("Pre-existing base-branch CI failures will not block routing", "base", pr.BaseRef, "checks", baseCI.FailureNames())
	}
	return ignored, baseFailures
}

// terminalSHA picks the head SHA a gave_up outcome is recorded against. It
// prefers the pipeline's captured post-rebase tip (RunResult.TipSHA): a Phase-0
// rebase rewrites the branch head, so the scan-time SHA is stale and next
// cycle's SHA-skip would miss, re-entering the expensive agent (N4). Falls back
// to the scan-time SHA only when the tip was never captured (no rebase reached
// — but those paths return GaveUp=false anyway). Pure.
func terminalSHA(tipSHA, scanSHA string) string {
	if tipSHA != "" {
		return tipSHA
	}
	return scanSHA
}

func (o *Orchestrator) actOnAnalysis(ctx context.Context, pr models.DependabotPR, analysis *models.AgentAnalysis) models.ReviewResult {
	// CI not acceptable + not needs_changes → genuinely-blocking pre-existing
	// failure. Bug #17: gate on AcceptableGiven (the single CI-acceptability
	// source, also used by the post-impl gate) rather than the raw CI.State, so
	// a PR whose only failing/stuck checks are ignored or base-failing is treated
	// as effectively green and falls through to the normal approve/no-op path
	// instead of being flagged for a human.
	ignored, baseFailures := o.suppressedChecks(ctx, pr)
	required := o.github.RequiredChecks(ctx, pr.BaseRef)
	acceptable, blocking := pr.CI.AcceptableGiven(ignored, baseFailures, required, time.Now(), o.config.CIStaleness)
	if !acceptable && analysis.Recommendation != models.RecommendNeedsChanges {
		slog.Info("CI not acceptable but assessment doesn't say needs_changes — flagging", "pr", pr.Number, "blocking", blocking)
		ciNote := fmt.Sprintf("\n\n**CI is failing** (%d check(s): %s). "+
			"Assessment did not identify code changes needed — failures may be pre-existing. "+
			"This review is submitted as a comment, not an approval.",
			len(blocking), strings.Join(blocking, ", "))
		if !o.dryRun {
			if err := o.github.UpsertStatusComment(ctx, pr.Number, pr.HeadSHA, analysis.ReviewBody+ciNote); err != nil {
				slog.Warn("failed to upsert CI-failing comment", "pr", pr.Number, "error", err)
			}
		}
		action := models.ActionFlaggedForHuman
		if o.dryRun {
			action = models.ActionDryRun
		}
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFlagged, "CI failing (pre-existing) — flagged for human")
		o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFlagged)
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      action,
			Detail:      fmt.Sprintf("CI failing (%s) — pre-existing failure, flagged for human", strings.Join(blocking, ", ")),
			Success:     true,
		}
	}

	// Low confidence → flag
	if analysis.Confidence == models.ConfidenceLow {
		slog.Info("Low confidence — flagging", "pr", pr.Number)
		if !o.dryRun {
			if err := o.github.UpsertStatusComment(ctx, pr.Number, pr.HeadSHA, analysis.ReviewBody); err != nil {
				slog.Warn("failed to upsert low-confidence comment", "pr", pr.Number, "error", err)
			}
		}
		action := models.ActionFlaggedForHuman
		if o.dryRun {
			action = models.ActionDryRun
		}
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFlagged, "low confidence")
		o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFlagged)
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      action,
			Detail:      "Low confidence — submitted as comment, not approval",
			Success:     true,
		}
	}

	// Human review recommended
	if analysis.Recommendation == models.RecommendNeedsHumanReview {
		slog.Info("Agent recommends human review", "pr", pr.Number)
		if !o.dryRun {
			if err := o.github.UpsertStatusComment(ctx, pr.Number, pr.HeadSHA, analysis.ReviewBody); err != nil {
				slog.Warn("failed to upsert human-review comment", "pr", pr.Number, "error", err)
			}
		}
		action := models.ActionFlaggedForHuman
		if o.dryRun {
			action = models.ActionDryRun
		}
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFlagged, "agent recommends human review")
		o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFlagged)
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      action,
			Detail:      "Agent recommends human review",
			Success:     true,
		}
	}

	// Recommend merge — the tool PROPOSES, a human decides. For a safe bump (no
	// code change, CI green) it posts its assessment as a comment; a maintainer
	// merges. It must NEVER submit a native APPROVE review: that could satisfy
	// branch protection / trigger dependabot auto-merge and land code with no
	// human in the loop, which contradicts the proposal model. Idempotency is
	// handled upstream (skip at a known terminal head SHA) and by
	// UpsertStatusComment editing its sticky comment in place rather than
	// posting a new one each cycle.
	if analysis.Recommendation == models.RecommendApprove {
		slog.Info("Recommending merge", "pr", pr.Number)
		if !o.dryRun {
			if err := o.github.UpsertStatusComment(ctx, pr.Number, pr.HeadSHA, analysis.ReviewBody); err != nil {
				slog.Warn("failed to upsert recommendation comment", "pr", pr.Number, "error", err)
			}
		}
		action := models.ActionApproved
		if o.dryRun {
			action = models.ActionDryRun
		}
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageApproved, "recommended for merge")
		o.recordOutcome(pr.Number, pr.HeadSHA, models.StageApproved)
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      action,
			Detail:      "Recommended for merge — no breaking changes affect codebase (a maintainer decides)",
			Success:     true,
		}
	}

	// Needs code changes → implementation pipeline
	if analysis.Recommendation == models.RecommendNeedsChanges {
		var changesDesc string
		if len(analysis.CodeChanges) > 0 {
			var lines []string
			for _, c := range analysis.CodeChanges {
				lines = append(lines, fmt.Sprintf("  - %s: %s", c.File, c.Description))
			}
			changesDesc = strings.Join(lines, "\n")
		}
		slog.Info("Needs code changes", "pr", pr.Number, "changes", changesDesc)

		if o.dryRun {
			detail := changesDesc
			if detail == "" {
				detail = "see review"
			}
			return models.ReviewResult{
				PRNumber:    pr.Number,
				PackageName: pr.PackageName,
				OldVersion:  pr.OldVersion,
				NewVersion:  pr.NewVersion,
				Action:      models.ActionDryRun,
				Detail:      fmt.Sprintf("Would run implementation pipeline for: %s", detail),
				Success:     true,
			}
		}

		// Idempotency: if a replacement PR is already open for this branch, skip
		// the pipeline — the prior run succeeded and we don't want to re-run the
		// agent on an already-complete PR (Bug #19).
		expectedBranch := implementation.BuildBranchName(pr.PackageName, pr.NewVersion)
		if existingN, exists, err := o.github.FindPRByBranch(ctx, expectedBranch); err != nil {
			slog.Warn("could not check for existing replacement PR", "pr", pr.Number, "error", err)
		} else if exists {
			slog.Info("replacement PR already exists — skipping pipeline", "pr", pr.Number, "replacement", existingN)
			o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFinalized, fmt.Sprintf("replacement PR #%d already exists", existingN))
			o.reportReplacement(pr.Number, existingN)
			o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFinalized)
			return models.ReviewResult{
				PRNumber:            pr.Number,
				PackageName:         pr.PackageName,
				OldVersion:          pr.OldVersion,
				NewVersion:          pr.NewVersion,
				Action:              models.ActionReplacementPR,
				Detail:              fmt.Sprintf("Replacement PR #%d already exists", existingN),
				ReplacementPRNumber: &existingN,
				Success:             true,
			}
		}

		pipeline := implementation.NewPipeline(o.config, o.github)
		if o.store != nil {
			pipeline.WithStore(o.store)
		}
		if o.logDir != "" {
			pipeline.WithLogDir(o.logDir)
		}
		result := pipeline.Run(ctx, pr, analysis)

		// Check if agent created a PR
		var replacementNumber int
		var hasReplacement bool
		if result.Branch != "" {
			var findErr error
			replacementNumber, hasReplacement, findErr = o.github.FindPRByBranch(ctx, result.Branch)
			if findErr != nil {
				slog.Warn("failed to look up replacement PR", "branch", result.Branch, "error", findErr)
			}
		}

		if result.Success && hasReplacement {
			slog.Info("Implementation agent created PR, finalising",
				"pr", pr.Number, "replacement", replacementNumber)

			if err := o.github.UpdatePRTitle(ctx, replacementNumber, pr.Title); err != nil {
				slog.Warn("failed to update replacement PR title", "pr", replacementNumber, "error", err)
			}
			if err := o.github.MarkPRReadyForReview(ctx, replacementNumber); err != nil {
				slog.Warn("failed to mark replacement PR ready", "pr", replacementNumber, "error", err)
			}
			if err := o.github.ClosePRWithComment(ctx, pr.Number,
				fmt.Sprintf("Replaced by #%d which includes the necessary code changes "+
					"for this dependency upgrade.\n\n_Automated review._", replacementNumber)); err != nil {
				slog.Warn("failed to close original PR", "pr", pr.Number, "error", err)
			}

			o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFinalized, result.Detail)
			o.reportReplacement(pr.Number, replacementNumber)
			o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFinalized)
			return models.ReviewResult{
				PRNumber:            pr.Number,
				PackageName:         pr.PackageName,
				OldVersion:          pr.OldVersion,
				NewVersion:          pr.NewVersion,
				Action:              models.ActionReplacementPR,
				Detail:              result.Detail,
				ReplacementPRNumber: &replacementNumber,
				Success:             true,
			}
		} else if result.Success && !hasReplacement {
			slog.Warn("Implementation succeeded but no PR found", "pr", pr.Number, "branch", result.Branch)
			o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFlagged, "implementation completed but no PR was created")
			// Sticky: record outcome so the next cycle skips (Bug #26).
			o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFlagged)
			return models.ReviewResult{
				PRNumber:    pr.Number,
				PackageName: pr.PackageName,
				OldVersion:  pr.OldVersion,
				NewVersion:  pr.NewVersion,
				Action:      models.ActionFlaggedForHuman,
				Detail:      fmt.Sprintf("Implementation completed but no PR was created on branch %s", result.Branch),
				Success:     true,
			}
		} else {
			// Pipeline failed.
			//
			// Two sub-cases:
			//   GaveUp == true  → deterministic give-up (no-progress / exhausted / time-cap).
			//                     Record as sticky outcome at this SHA so the next cycle skips
			//                     without re-spinning. Stage: gave_up.
			//   GaveUp == false → transient / retriable failure (worker crash, rebase error, …).
			//                     Pass "" SHA so the next run retries. Stage: flagged_human.

			var fb strings.Builder
			if result.GaveUp {
				// One-line, high-confidence signal per Principle (conciseness).
				fmt.Fprintf(&fb, "%s\n\n_Automated review._", result.Detail)
			} else {
				fmt.Fprintf(&fb, "Automated implementation attempted but did not complete.\n\n**Reason:** %s\n\n", result.Detail)
				if result.ReviewVerdict != nil && len(result.ReviewVerdict.Concerns) > 0 {
					fb.WriteString("**Review concerns:**\n")
					for _, c := range result.ReviewVerdict.Concerns {
						fmt.Fprintf(&fb, "- %s\n", c)
					}
					fb.WriteString("\n")
				}
				if result.Branch != "" {
					fmt.Fprintf(&fb, "Branch `%s` contains the partial work.\n\n", result.Branch)
				}
				fb.WriteString("_Automated review._")
			}

			// Close any rogue PR regardless of give-up status.
			if hasReplacement {
				if err := o.github.ClosePRWithComment(ctx, replacementNumber,
					fmt.Sprintf("Closing — automated implementation did not complete successfully.\n\n%s", result.Detail)); err != nil {
					slog.Warn("failed to close rogue replacement PR", "pr", replacementNumber, "error", err)
				}
			}

			if result.GaveUp {
				// Sticky: record the outcome at the *post-rebase* branch tip
				// (result.TipSHA), NOT the scan-time pr.HeadSHA. A Phase-0 rebase
				// rewrites the branch head, so the scan-time SHA is stale; recording
				// there would make next cycle's SHA-skip miss and re-enter the
				// expensive agent (N4 / MAJOR-1). Both the DB outcome and the sticky
				// comment's SHA marker (the nil-store skip key) use this SHA. Other
				// recordOutcome calls stay on pr.HeadSHA — they are pre-rebase
				// analysis flags or finalize (original closed, never re-scanned).
				giveUpSHA := terminalSHA(result.TipSHA, pr.HeadSHA)
				if err := o.github.UpsertStatusComment(ctx, pr.Number, giveUpSHA, fb.String()); err != nil {
					slog.Warn("failed to upsert give-up comment", "pr", pr.Number, "error", err)
				}
				o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageGaveUp, result.Detail)
				o.recordOutcome(pr.Number, giveUpSHA, models.StageGaveUp)
			} else {
				// Sticky: use pr.HeadSHA so the next cycle skips without re-launching
				// the analyser/impl/reviewer (Bug #26). The PR is retried automatically
				// when dependabot pushes a new commit (new head SHA).
				if err := o.github.UpsertStatusComment(ctx, pr.Number, pr.HeadSHA, fb.String()); err != nil {
					slog.Warn("failed to upsert failure comment", "pr", pr.Number, "error", err)
				}
				o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFlagged, result.Detail)
				o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFlagged)
			}

			return models.ReviewResult{
				PRNumber:    pr.Number,
				PackageName: pr.PackageName,
				OldVersion:  pr.OldVersion,
				NewVersion:  pr.NewVersion,
				Action:      models.ActionFlaggedForHuman,
				Detail:      result.Detail,
				Success:     true,
			}
		}
	}

	// Should not reach here
	return models.ReviewResult{
		PRNumber:    pr.Number,
		PackageName: pr.PackageName,
		OldVersion:  pr.OldVersion,
		NewVersion:  pr.NewVersion,
		Action:      models.ActionError,
		Detail:      fmt.Sprintf("Unexpected recommendation: %s", analysis.Recommendation),
		Success:     false,
	}
}

// PrintSummary prints a human-readable summary of all actions taken.
func PrintSummary(results []models.ReviewResult) {
	if len(results) == 0 {
		fmt.Println("\nNo dependabot PRs to process.")
		return
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Printf("Processed %d dependabot PR(s):\n", len(results))
	fmt.Printf("%s\n", strings.Repeat("=", 60))

	for _, r := range results {
		status := "OK"
		if !r.Success {
			status = "ERR"
		}
		var actionSymbol string
		switch r.Action {
		case models.ActionApproved:
			actionSymbol = "APPROVED"
		case models.ActionClosedStale:
			actionSymbol = "CLOSED (stale)"
		case models.ActionReplacementPR:
			if r.ReplacementPRNumber != nil {
				actionSymbol = fmt.Sprintf("REPLACED (#%d)", *r.ReplacementPRNumber)
			} else {
				actionSymbol = "REPLACED"
			}
		case models.ActionFlaggedForHuman:
			actionSymbol = "FLAGGED"
		case models.ActionSkippedPolicy:
			actionSymbol = "SKIPPED (policy)"
		case models.ActionSkippedPending:
			actionSymbol = "SKIPPED (CI pending)"
		case models.ActionSkippedNoChange:
			actionSymbol = "SKIPPED (no change)"
		case models.ActionError:
			actionSymbol = "ERROR"
		case models.ActionDryRun:
			actionSymbol = "DRY RUN"
		default:
			actionSymbol = string(r.Action)
		}

		// Grouped PRs (and anything else without a single version) have empty
		// Old/NewVersion; render just the name rather than "name  -> ".
		header := r.PackageName
		if r.OldVersion != "" || r.NewVersion != "" {
			header = fmt.Sprintf("%s %s -> %s", r.PackageName, r.OldVersion, r.NewVersion)
		}
		fmt.Printf("  #%d %s\n", r.PRNumber, header)
		fmt.Printf("    [%s] %s: %s\n", status, actionSymbol, r.Detail)
		if r.Error != "" {
			fmt.Printf("    Error: %s\n", r.Error)
		}
		fmt.Println()
	}
}
