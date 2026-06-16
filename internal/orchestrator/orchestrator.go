// Package orchestrator provides the main processing loop for dependabot PRs.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/agent"
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
	config         *config.Config
	repo           string
	dryRun         bool
	verbose        bool
	reviewers      []string
	onlyPR         int // 0 means process all
	github         *ghclient.Client
	combinedAgent  *agent.CombinedAgent
	analyser       *analyser.Analyser     // legacy path only (--legacy-analyser)
	legacyAnalyser bool                   // when true, use the old analyser instead of combinedAgent
	store          progress.ReadWriter    // optional; nil for the one-shot `review` command
	logDir         string                 // forwarded to each implementation pipeline
	bareClonePath  string                 // set by Run() after ensureBareClone; forwarded to each pipeline
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
		config:        cfg,
		repo:          repo,
		dryRun:        dryRun,
		verbose:       verbose,
		reviewers:     reviewers,
		onlyPR:        onlyPR,
		github:        gh,
		combinedAgent: agent.NewCombinedAgent(cfg.CombinedAgentModel, cfg.CombinedAgentBudget),
		analyser:      analyser.NewAnalyser(cfg.AnthropicAPIKey, cfg.AnalyserModel, cfg.AnalyserThinkingBudget, verbose),
	}, nil
}

// WithLegacyAnalyser enables the pre-Q10 two-step path: the separate tool-less
// analyser runs first, then actOnAnalysis routes. This is the Q10 rollback flag.
// The default (false) uses the combined agent (Q10 path).
func (o *Orchestrator) WithLegacyAnalyser(v bool) *Orchestrator {
	o.legacyAnalyser = v
	return o
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
//
// It also sweeps per-PR working directories under DataDir: any pr-<N>/ directory
// whose PR is absent from the open set is deleted. The GaveUp caveat: a gave_up
// PR's working directory persists until the PR is manually closed or merged (the
// original dependabot PR stays open) — this is a known, accepted gap.
func (o *Orchestrator) reapClosed(open []models.DependabotPR) {
	nums := make([]int, len(open))
	openSet := make(map[int]bool, len(open))
	for i, pr := range open {
		nums[i] = pr.Number
		openSet[pr.Number] = true
	}
	if o.store != nil {
		o.store.Reap(nums)
	}

	// Sweep per-PR working directories: delete any pr-<N>/ directory whose PR
	// is absent from the open-PR list.
	if o.config.DataDir != "" {
		repoSlug := strings.ReplaceAll(o.repo, "/", "-")
		prDir := filepath.Join(o.config.DataDir, "pr", repoSlug)
		entries, err := os.ReadDir(prDir)
		if err != nil && !os.IsNotExist(err) {
			slog.Warn("could not sweep per-PR workdirs", "dir", prDir, "error", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			var prNum int
			if _, err := fmt.Sscanf(e.Name(), "pr-%d", &prNum); err != nil {
				continue
			}
			if !openSet[prNum] {
				dir := filepath.Join(prDir, e.Name())
				slog.Info("Removing closed-PR workdir", "pr", prNum, "path", dir)
				if err := os.RemoveAll(dir); err != nil {
					slog.Warn("could not remove closed-PR workdir", "pr", prNum, "path", dir, "error", err)
				}
			}
		}
	}
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

// reportReplacement is the nil-safe shim for recording a replacement PR number and URL.
func (o *Orchestrator) reportReplacement(prNumber, replacementN int, replacementURL string) {
	if o.store != nil {
		o.store.SetReplacementPR(prNumber, replacementN, replacementURL)
	}
}

// setVersions is the nil-safe shim for recording version metadata and source URL.
func (o *Orchestrator) setVersions(prNumber int, oldVer, newVer, ecosystem, url string) {
	if o.store != nil {
		o.store.SetVersions(prNumber, oldVer, newVer, ecosystem, url)
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

// recordCreatedPR is the nil-safe shim for recording a sweeper PR we opened, so
// it is permanently excluded from future scans (Q14).
func (o *Orchestrator) recordCreatedPR(createdPR, originPR int) {
	if o.store != nil {
		o.store.RecordCreatedPR(createdPR, originPR)
	}
}

// excludeOwnPRs drops PRs the tool itself created (Q14 / review C1). The author
// filter is the primary in-scope gate, but on the test bed the fix-PR author is
// also an accepted author — so without this a replacement PR could be
// re-ingested and re-processed as a fresh dependabot PR, a runaway agentic-cost
// incident. The record lives in a reap-exempt table, so it survives the per-cycle
// Reap. No-op when the store is nil or nothing has been created yet.
func (o *Orchestrator) excludeOwnPRs(prs []models.DependabotPR) []models.DependabotPR {
	if o.store == nil {
		return prs
	}
	created := o.store.CreatedPRs()
	if len(created) == 0 {
		return prs
	}
	out := make([]models.DependabotPR, 0, len(prs))
	for _, pr := range prs {
		if _, ours := created[pr.Number]; ours {
			slog.Info("Skipping our own replacement PR (reap-exempt exclusion)", "pr", pr.Number)
			continue
		}
		out = append(out, pr)
	}
	return out
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

	// Drop any PR the tool itself created — a reap-exempt cost-safety gate so our
	// own replacement PRs are never re-ingested as fresh dependabot PRs (Q14).
	allPRs = o.excludeOwnPRs(allPRs)

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

	// Ensure the bare clone exists and is up to date — must run before PR
	// goroutines launch to avoid a git fetch racing with active agent git
	// operations. Failure is non-fatal: pipelines fall back to network clones.
	if o.config.DataDir != "" {
		bare, err := ensureBareClone(ctx, o.config.DataDir, o.repo, o.config.GitHubToken)
		if err != nil {
			slog.Warn("Could not ensure bare clone — PRs will clone from GitHub directly", "error", err)
		} else {
			o.bareClonePath = bare
			slog.Info("Bare clone ready", "path", bare)
		}
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
	// early (stale, not-settled, already-processed) without waiting
	// for analysis to complete.
	o.setVersions(pr.Number, pr.OldVersion, pr.NewVersion, pr.Ecosystem, pr.URL)
	o.setCI(pr.Number, pr.CI)

	// Step 0: Skip PRs whose bump type couldn't be parsed. BumpUnknown means
	// the title didn't match the dependabot bump pattern — these are not real
	// dependency upgrades (e.g. empty-commit PRs, reverts, unrelated author
	// PRs). The accept-author filter is the primary gate; this is a backstop
	// so unparseable titles never reach the expensive agent.
	if pr.BumpType == models.BumpUnknown {
		detail := "skipped: could not classify bump type from PR title"
		slog.Info("Unknown bump type — skipping", "pr", pr.Number)
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

	// Step 1: Staleness. A PR is superseded if a newer INDIVIDUAL PR bumps the
	// same package higher (existing behaviour), or (Q6) a grouped PR already
	// covers this package at >= this version. A grouped PR is never closed as
	// stale — it bumps other members too — and an individual never closes a
	// whole group; hence the !pr.Grouped guard and the directional group check.
	//
	// NOTE: FindNewerPRForPackage is the PRIMARY guard against same-package
	// parallel processing. Because only the higher-version PR proceeds past
	// this gate, at most one PR per package reaches the implementation pipeline
	// at a time. Per-PR directory isolation (6.D) is the backstop — it prevents
	// data corruption if two PRs for the same package somehow both pass this
	// gate — but it is NOT the primary defence. The staleness check here is.
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

	// Q10 path: combined analysis+decision agent, or legacy two-step analyser.
	if o.legacyAnalyser {
		return o.runLegacyAnalyser(ctx, pr)
	}
	return o.runCombinedAgent(ctx, pr)
}

// runLegacyAnalyser is the pre-Q10 path: pre-fetch upstream data + codebase usage,
// call the tool-less analyser, route on its recommendation. Kept behind
// --legacy-analyser for rollback. New code should use runCombinedAgent.
func (o *Orchestrator) runLegacyAnalyser(ctx context.Context, pr models.DependabotPR) models.ReviewResult {
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

	// Step 6: Fetch failing-CI logs (best-effort).
	var failureLogs map[string]string
	if len(pr.CI.Failures) > 0 {
		slog.Info("Fetching failing-CI logs", "pr", pr.Number, "checks", len(pr.CI.Failures))
		failureLogs = o.github.FetchFailureLogs(ctx, pr)
	}

	// Step 7: Claude analysis
	slog.Info("Running Claude analysis (legacy analyser)", "pr", pr.Number)
	o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageAnalysing, "")
	analysis, err := o.analyser.Analyse(ctx, pr, upstream, usage, failureLogs)
	if err != nil {
		slog.Error("Analysis failed", "pr", pr.Number, "error", err)
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageError, fmt.Sprintf("analysis failed: %s", err))
		if !o.dryRun {
			if cerr := o.github.UpsertStatusComment(ctx, pr.Number, pr.HeadSHA,
				fmt.Sprintf("Automated analysis could not complete.\n\n**Reason:** %s\n\n_Automated review._", err)); cerr != nil {
				slog.Warn("failed to post analysis-error comment", "pr", pr.Number, "error", cerr)
			}
		}
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

	o.setAnalysis(pr.Number, *analysis)
	return o.actOnAnalysis(ctx, pr, analysis)
}

// runCombinedAgent is the Q10 path: a single agentic step analyses upstream +
// codebase impact and decides the outcome. No pre-fetching of upstream data.
// The combined agent runs with full tool access (--dangerously-skip-permissions).
func (o *Orchestrator) runCombinedAgent(ctx context.Context, pr models.DependabotPR) models.ReviewResult {
	// Determine the per-PR working directory. The orchestrator prepares it here
	// so the combined agent and the implementation pipeline (if needed) share it.
	workdir, repoDir, err := o.prepareAgentWorkdir(ctx, pr)
	if err != nil {
		slog.Error("Failed to prepare agent workdir", "pr", pr.Number, "error", err)
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageError, fmt.Sprintf("workdir setup failed: %s", err))
		o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFlagged)
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      models.ActionFlaggedForHuman,
			Detail:      fmt.Sprintf("Workdir setup failed: %s", err),
			Success:     true,
		}
	}

	// Log path co-located with the workdir so reapClosed cleans it up automatically.
	logPath := filepath.Join(workdir, fmt.Sprintf("pr-%d-agent.jsonl", pr.Number))

	slog.Info("Running combined agent", "pr", pr.Number, "workdir", workdir)
	o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageAnalysing, "")
	verdict, err := o.combinedAgent.Analyse(ctx, pr, workdir, repoDir, o.bareClonePath, o.github.RepoFullName(), logPath)
	if err != nil {
		slog.Error("Combined agent failed", "pr", pr.Number, "error", err)
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageError, fmt.Sprintf("combined agent failed: %s", err))
		// No GitHub comment — the agent itself may have posted partial work.
		// Record as flagged so next cycle skips without re-spinning the agent.
		o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFlagged)
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      models.ActionFlaggedForHuman,
			Detail:      fmt.Sprintf("Combined agent failed: %s", err),
			Success:     true,
		}
	}

	return o.actOnAgentVerdict(ctx, pr, verdict, workdir, repoDir)
}

// prepareAgentWorkdir creates the canonical per-PR working directory and clones
// the repo into it. Returns (workdir, repoDir, error). If DataDir is set, the
// workdir is stable across cycles: <DataDir>/pr/<owner-repo>/pr-<N>/. Otherwise
// falls back to os.MkdirTemp.
func (o *Orchestrator) prepareAgentWorkdir(ctx context.Context, pr models.DependabotPR) (workdir, repoDir string, err error) {
	if o.config.DataDir != "" {
		slug := strings.ReplaceAll(o.repo, "/", "-")
		workdir = filepath.Join(o.config.DataDir, "pr", slug, fmt.Sprintf("pr-%d", pr.Number))
		// Remove stale workdir from a prior crash, then recreate.
		if info, serr := os.Stat(workdir); serr == nil && info.IsDir() {
			slog.Info("Removing stale agent workdir (crash residue)", "path", workdir)
			if rerr := os.RemoveAll(workdir); rerr != nil {
				return "", "", fmt.Errorf("removing stale workdir %s: %w", workdir, rerr)
			}
		}
		if merr := os.MkdirAll(workdir, 0o755); merr != nil {
			return "", "", fmt.Errorf("creating agent workdir %s: %w", workdir, merr)
		}
	} else {
		workdir, err = os.MkdirTemp("", "sweeper-agent-*")
		if err != nil {
			return "", "", fmt.Errorf("creating temp agent workdir: %w", err)
		}
	}

	repoDir = filepath.Join(workdir, "repo")

	// Clone the repo. Use the local bare clone if available; fall back to GitHub.
	var cloneArgs []string
	if o.bareClonePath != "" {
		cloneArgs = []string{"git", "clone", "--local", "--no-checkout", o.bareClonePath, repoDir}
	} else {
		tokenlessURL := fmt.Sprintf("https://github.com/%s.git", o.repo)
		cloneArgs = []string{
			"git",
			"-c", "credential.helper=",
			"-c", fmt.Sprintf("credential.helper=!f() { echo username=x-access-token; echo \"password=%s\"; }; f", o.config.GitHubToken),
			"clone", "--no-checkout", "--filter=blob:none", tokenlessURL, repoDir,
		}
	}
	cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cloneCmd := exec.CommandContext(cloneCtx, cloneArgs[0], cloneArgs[1:]...)
	if out, cerr := cloneCmd.CombinedOutput(); cerr != nil {
		return "", "", fmt.Errorf("git clone for agent workdir failed: %w\n%s", cerr, out)
	}

	// Checkout the PR's head ref so the agent sees the actual PR content.
	fetchCtx, cancel2 := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel2()
	fetchArgs := []string{"git", "-C", repoDir, "fetch", "origin",
		fmt.Sprintf("refs/pull/%d/head:refs/remotes/origin/pr-%d", pr.Number, pr.Number)}
	if out, ferr := exec.CommandContext(fetchCtx, fetchArgs[0], fetchArgs[1:]...).CombinedOutput(); ferr != nil {
		slog.Warn("fetch PR head failed — agent will work off default branch", "pr", pr.Number, "error", ferr, "output", string(out))
	} else {
		checkoutArgs := []string{"git", "-C", repoDir, "checkout", "-b", pr.HeadRef,
			fmt.Sprintf("refs/remotes/origin/pr-%d", pr.Number)}
		if out, coerr := exec.Command(checkoutArgs[0], checkoutArgs[1:]...).CombinedOutput(); coerr != nil {
			slog.Warn("checkout PR branch failed — agent will work off default branch", "pr", pr.Number, "error", coerr, "output", string(out))
		}
	}

	return workdir, repoDir, nil
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
			o.reportReplacement(pr.Number, existingN, fmt.Sprintf("https://github.com/%s/pull/%d", o.repo, existingN))
			o.recordCreatedPR(existingN, pr.Number) // ensure our own PR is excluded even if found pre-existing (Q14)
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
		if o.bareClonePath != "" {
			pipeline.WithBareClone(o.bareClonePath)
		}
		result := pipeline.Run(ctx, pr, analysis)
		return o.handlePipelineResult(ctx, pr, result)
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

// actOnAgentVerdict routes the combined agent's verdict:
//   - recommend: re-gate on a fresh required-CI read; post the recommend_body; approve.
//   - needs_changes: invoke the implementation pipeline.
//   - flag_human: post the concise flag_reason; record flagged.
//   - gave_up: silent draft; record gave_up at scan-time head SHA (no comment posted).
func (o *Orchestrator) actOnAgentVerdict(
	ctx context.Context,
	pr models.DependabotPR,
	verdict *models.AgentVerdict,
	workdir, repoDir string,
) models.ReviewResult {
	switch verdict.Outcome {

	case models.AgentOutcomeRecommend:
		// Mechanical re-gate (Q4/C3): the agent's self-reported "CI is green" is not
		// trusted. Re-read CI from GitHub right now using only the required-status-checks
		// set. If not acceptable, flag instead of approving — this is the programmatic
		// enforce of "approve only when CI is already green."
		slog.Info("Agent recommends merge — re-gating on required CI", "pr", pr.Number)
		required := o.github.RequiredChecks(ctx, pr.BaseRef)
		ignoredForGate := make(map[string]bool, len(o.config.IgnoreChecks))
		for _, name := range o.config.IgnoreChecks {
			ignoredForGate[name] = true
		}
		// No base-failure suppression here: the Q3 decision says genuine green is the bar.
		acceptable, blocking := pr.CI.AcceptableGiven(ignoredForGate, nil, required, time.Now(), o.config.CIStaleness)
		if !acceptable {
			slog.Info("Agent recommended merge but required CI not acceptable — flagging", "pr", pr.Number, "blocking", blocking)
			reason := fmt.Sprintf("Agent recommended merge but required CI checks are not yet acceptable (%s). "+
				"Will re-evaluate when CI settles.", strings.Join(blocking, ", "))
			if !o.dryRun {
				if err := o.github.UpsertStatusComment(ctx, pr.Number, pr.HeadSHA, reason); err != nil {
					slog.Warn("failed to post CI-not-acceptable comment", "pr", pr.Number, "error", err)
				}
			}
			o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFlagged, "recommend re-gate: required CI not acceptable")
			o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFlagged)
			return models.ReviewResult{
				PRNumber:    pr.Number,
				PackageName: pr.PackageName,
				OldVersion:  pr.OldVersion,
				NewVersion:  pr.NewVersion,
				Action:      models.ActionFlaggedForHuman,
				Detail:      "Agent recommended merge but required CI not acceptable — will retry",
				Success:     true,
			}
		}

		slog.Info("Recommending merge", "pr", pr.Number)
		if !o.dryRun {
			if err := o.github.UpsertStatusComment(ctx, pr.Number, pr.HeadSHA, verdict.RecommendBody); err != nil {
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
			Detail:      "Recommended for merge — agent verified no code change needed",
			Success:     true,
		}

	case models.AgentOutcomeFlagHuman:
		slog.Info("Agent flagged for human", "pr", pr.Number, "reason", verdict.FlagReason)
		// Post only the concise reason — never a review_body dump (Q10/T8/T4).
		if !o.dryRun {
			if err := o.github.UpsertStatusComment(ctx, pr.Number, pr.HeadSHA, verdict.FlagReason+"\n\n_Automated review._"); err != nil {
				slog.Warn("failed to post flag-human comment", "pr", pr.Number, "error", err)
			}
		}
		action := models.ActionFlaggedForHuman
		if o.dryRun {
			action = models.ActionDryRun
		}
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFlagged, verdict.FlagReason)
		o.recordOutcome(pr.Number, pr.HeadSHA, models.StageFlagged)
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      action,
			Detail:      "Agent flagged for human: " + verdict.FlagReason,
			Success:     true,
		}

	case models.AgentOutcomeGaveUp:
		// Silent draft (Q3): record gave_up at the scan-time head SHA, no comment.
		// The implementation pipeline is NOT invoked — the agent gave up before deciding
		// on changes. Next cycle's SHA-skip fires before any new agent run.
		slog.Info("Agent gave up — silent outcome", "pr", pr.Number)
		action := models.ActionFlaggedForHuman
		if o.dryRun {
			action = models.ActionDryRun
		}
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageGaveUp, "agent gave up — silent draft")
		o.recordOutcome(pr.Number, pr.HeadSHA, models.StageGaveUp)
		return models.ReviewResult{
			PRNumber:    pr.Number,
			PackageName: pr.PackageName,
			OldVersion:  pr.OldVersion,
			NewVersion:  pr.NewVersion,
			Action:      action,
			Detail:      "Agent gave up — silent outcome recorded",
			Success:     true,
		}

	case models.AgentOutcomeNeedsChanges:
		slog.Info("Agent identified code changes needed — launching implementation pipeline", "pr", pr.Number)

		if o.dryRun {
			return models.ReviewResult{
				PRNumber:    pr.Number,
				PackageName: pr.PackageName,
				OldVersion:  pr.OldVersion,
				NewVersion:  pr.NewVersion,
				Action:      models.ActionDryRun,
				Detail:      "Would run implementation pipeline (agent identified code changes needed)",
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
			o.reportReplacement(pr.Number, existingN, fmt.Sprintf("https://github.com/%s/pull/%d", o.repo, existingN))
			o.recordCreatedPR(existingN, pr.Number)
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
		if o.bareClonePath != "" {
			pipeline.WithBareClone(o.bareClonePath)
		}
		// Pass the already-prepared workdir so the pipeline can skip re-cloning.
		if workdir != "" {
			pipeline.WithWorkdir(workdir)
		}
		// Pass the justification for Q13 curate (agent authors logical commits at approval).
		if verdict.Justification != "" {
			pipeline.WithAgentJustification(verdict.Justification)
		}

		// For the combined agent path, the implementation brief is seeded from the
		// agent's justification (not an analyser review_body). Wrap the justification
		// in an AgentAnalysis shell for backward compat with the existing brief builders.
		// The justification is also passed via WithAgentJustification for the reviewer
		// evaluation and for the curate agent commit-message guidance (Q13/Q15).
		analysisShell := &models.AgentAnalysis{
			ReviewBody: verdict.Justification,
		}
		result := pipeline.Run(ctx, pr, analysisShell)
		return o.handlePipelineResult(ctx, pr, result)
	}

	// Should not reach here
	return models.ReviewResult{
		PRNumber:    pr.Number,
		PackageName: pr.PackageName,
		OldVersion:  pr.OldVersion,
		NewVersion:  pr.NewVersion,
		Action:      models.ActionError,
		Detail:      fmt.Sprintf("Unexpected agent outcome: %s", verdict.Outcome),
		Success:     false,
	}
}

// handlePipelineResult processes the result of an implementation pipeline run,
// shared between actOnAnalysis (legacy) and actOnAgentVerdict (combined agent).
func (o *Orchestrator) handlePipelineResult(ctx context.Context, pr models.DependabotPR, result implementation.RunResult) models.ReviewResult {
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

		if err := o.github.UpdatePRTitle(ctx, replacementNumber, implementation.SweeperPRTitle(pr.Title)); err != nil {
			slog.Warn("failed to update replacement PR title", "pr", replacementNumber, "error", err)
		}
		if err := o.github.MarkPRReadyForReview(ctx, replacementNumber); err != nil {
			slog.Warn("failed to mark replacement PR ready", "pr", replacementNumber, "error", err)
		}
		// Q15: Post justification to replacement PR body if the combined agent
		// provided one AND the reviewer confirmed it is OK. The justification is
		// held private through the impl↔reviewer loop and posted here — at the
		// moment of final approval — so it is visible to human reviewers of the
		// replacement PR. On the legacy analyser path, result.Justification is empty.
		if result.Justification != "" {
			verdictOK := result.ReviewVerdict == nil || result.ReviewVerdict.JustificationOK
			if verdictOK {
				if err := o.github.UpdatePRBody(ctx, replacementNumber, result.Justification); err != nil {
					slog.Warn("failed to post justification to replacement PR body", "pr", replacementNumber, "error", err)
				} else {
					slog.Info("posted justification to replacement PR body", "pr", replacementNumber)
				}
			} else {
				slog.Warn("reviewer flagged justification — not posting to PR body",
					"pr", replacementNumber, "concern", result.ReviewVerdict.JustificationConcern)
			}
		}
		if err := o.github.ClosePRWithComment(ctx, pr.Number,
			fmt.Sprintf("Replaced by #%d which includes the necessary code changes "+
				"for this dependency upgrade.\n\n_Automated review._", replacementNumber)); err != nil {
			slog.Warn("failed to close original PR", "pr", pr.Number, "error", err)
		}

		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFinalized, result.Detail)
		o.reportReplacement(pr.Number, replacementNumber, fmt.Sprintf("https://github.com/%s/pull/%d", o.repo, replacementNumber))
		o.recordCreatedPR(replacementNumber, pr.Number)
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
	}

	// Pipeline failed.
	if hasReplacement {
		if err := o.github.ClosePRWithComment(ctx, replacementNumber,
			fmt.Sprintf("Closing — automated implementation did not complete successfully.\n\n%s", result.Detail)); err != nil {
			slog.Warn("failed to close rogue replacement PR", "pr", replacementNumber, "error", err)
		}
	}

	if result.GaveUp {
		// Q3: silent draft — no comment posted to the original dependabot PR.
		// The replacement stays a draft; no noise to maintainers. Record the
		// terminal outcome so the next cycle's SHA-skip fires and does not
		// re-enter the agentic pipeline (cost-safety invariant).
		giveUpSHA := terminalSHA(result.TipSHA, pr.HeadSHA)
		slog.Info("Pipeline gave up — recording silent gave_up outcome (no comment posted)",
			"pr", pr.Number, "detail", result.Detail)
		o.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageGaveUp, result.Detail)
		o.recordOutcome(pr.Number, giveUpSHA, models.StageGaveUp)
	} else {
		var fb strings.Builder
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

// gitCredentialHelperOrch is a git credential.helper that supplies the GitHub
// token from the GH_TOKEN environment variable. Duplicated from implementation
// package (same constant, not imported to keep packages decoupled).
const gitCredentialHelperOrch = `!f() { echo username=x-access-token; echo "password=${GH_TOKEN}"; }; f`

// bareCloneDir returns the stable path for the bare clone of a repo:
//
//	<dataDir>/base/<owner>-<repo>.git
func bareCloneDir(dataDir, repoName string) string {
	slug := strings.ReplaceAll(repoName, "/", "-")
	return filepath.Join(dataDir, "base", slug+".git")
}

// ensureBareClone creates or re-fetches the bare clone of repoName at the
// canonical path. On fetch failure the bare clone is deleted and re-cloned from
// scratch. Returns the bare clone path on success.
//
// Must be called before PR goroutines launch — a concurrent git fetch from
// multiple pipelines reading the same bare clone is safe (reads are safe), but
// a fetch that modifies the bare clone while a pipeline is reading it is not.
func ensureBareClone(ctx context.Context, dataDir, repoName, token string) (string, error) {
	barePath := bareCloneDir(dataDir, repoName)
	gitEnv := append(os.Environ(), "GH_TOKEN="+token)
	tokenlessURL := fmt.Sprintf("https://github.com/%s.git", repoName)

	if _, err := os.Stat(barePath); err == nil {
		// Bare clone exists — re-fetch to pick up new commits and tags.
		slog.Info("Re-fetching bare clone", "path", barePath)
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		fetch := exec.CommandContext(fetchCtx, "git",
			"-c", "credential.helper=",
			"-c", "credential.helper="+gitCredentialHelperOrch,
			"-C", barePath, "fetch", "--tags", "origin")
		fetch.Env = gitEnv
		if out, err := fetch.CombinedOutput(); err != nil {
			slog.Warn("Bare clone fetch failed — re-cloning from scratch",
				"path", barePath, "error", err, "output", string(out))
			if rerr := os.RemoveAll(barePath); rerr != nil {
				return "", fmt.Errorf("removing failed bare clone: %w", rerr)
			}
			// Fall through to create a fresh bare clone.
		} else {
			return barePath, nil
		}
	}

	// Create a fresh bare clone.
	slog.Info("Creating bare clone", "path", barePath, "repo", repoName)
	if err := os.MkdirAll(filepath.Dir(barePath), 0o755); err != nil {
		return "", fmt.Errorf("creating bare clone parent dir: %w", err)
	}
	cloneCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	clone := exec.CommandContext(cloneCtx, "git",
		"-c", "credential.helper=",
		"-c", "credential.helper="+gitCredentialHelperOrch,
		"clone", "--bare", tokenlessURL, barePath)
	clone.Env = gitEnv
	if out, err := clone.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone --bare failed: %w\n%s", err, out)
	}
	return barePath, nil
}
