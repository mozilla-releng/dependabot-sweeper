// Package implementation manages the full lifecycle of fixing a dependency upgrade.
package implementation

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/config"
	ghclient "github.com/mozilla-releng/dependabot-sweeper/internal/github"
	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/progress"
	"github.com/mozilla-releng/dependabot-sweeper/internal/reviewer"
)

var versionSuffixRe = regexp.MustCompile(`^v`)

// BuildBranchName generates a branch name from package name and version.
// For grouped PRs (empty newVersion) it appends "-group" as the suffix.
func BuildBranchName(packageName, newVersion string) string {
	name := strings.TrimPrefix(packageName, "@")
	name = strings.TrimPrefix(name, "github.com/")
	name = strings.ReplaceAll(name, "/", "-")
	if newVersion == "" {
		return fmt.Sprintf("auto/fix/%s-group", name)
	}
	version := versionSuffixRe.ReplaceAllString(newVersion, "")
	return fmt.Sprintf("auto/fix/%s-%s", name, version)
}

// conventionalPrefixRe matches a conventional-commit prefix — a type, an
// optional (scope), then ": ". Group 1 is the type, group 2 the optional scope
// (including its parentheses), and the whole match (group 0) is the prefix.
var conventionalPrefixRe = regexp.MustCompile(`^([a-zA-Z]+)(\([^)]*\))?: `)

// SweeperPRTitle derives the replacement PR's title from the dependabot title by
// swapping the conventional-commit type to `fix`, preserving the scope and
// description (`build(deps): bump X…` → `fix(deps): bump X…`). When the title has
// no recognisable conventional prefix (e.g. a bare `Bump X from A to B`), it
// prepends `fix(deps): `. This makes a sweeper PR visibly and structurally
// distinct from a dependabot PR (T12/Q14) — it replaces the previous verbatim
// copy of the dependabot title, which made the two indistinguishable. Pure.
func SweeperPRTitle(dependabotTitle string) string {
	if m := conventionalPrefixRe.FindStringSubmatch(dependabotTitle); m != nil {
		scope := m[2] // includes the parentheses, or "" when there's no scope
		rest := dependabotTitle[len(m[0]):]
		return fmt.Sprintf("fix%s: %s", scope, rest)
	}
	return "fix(deps): " + dependabotTitle
}

// gitCredentialHelper is a git credential.helper that supplies the GitHub token
// from the GH_TOKEN environment variable. Using it (instead of embedding the
// token in the clone URL) keeps the token out of the remote URL, .git/config,
// process argv, and any git command output — the last of which matters because
// the worker's git output is streamed to the per-PR agent log served by the
// (unauthenticated) dashboard. The helper text itself contains no secret.
const gitCredentialHelper = `!f() { echo username=x-access-token; echo "password=${GH_TOKEN}"; }; f`

// tokenlessCloneURL returns the HTTPS clone URL for a repo with no embedded
// credentials. Authentication is provided out-of-band via gitCredentialHelper.
func tokenlessCloneURL(repoName string) string {
	return fmt.Sprintf("https://github.com/%s.git", repoName)
}

// gitEnv returns the process environment augmented with GH_TOKEN so that
// gitCredentialHelper can read the token. Applied to every networked git
// command (clone/fetch/push) the pipeline runs.
func (p *Pipeline) gitEnv() []string {
	return append(os.Environ(), "GH_TOKEN="+p.config.GitHubToken)
}

var (
	filesChangedRe = regexp.MustCompile(`(\d+) files? changed`)
	insertionsRe   = regexp.MustCompile(`(\d+) insertions?\(\+\)`)
	deletionsRe    = regexp.MustCompile(`(\d+) deletions?\(-\)`)
)

// ParseDiffStat parses a git diff --stat summary line into a readable string.
func ParseDiffStat(statLine string) string {
	statLine = strings.TrimSpace(statLine)
	if statLine == "" {
		return "(no changes)"
	}
	files := "0"
	ins := "0"
	dels := "0"
	if m := filesChangedRe.FindStringSubmatch(statLine); m != nil {
		files = m[1]
	}
	if m := insertionsRe.FindStringSubmatch(statLine); m != nil {
		ins = m[1]
	}
	if m := deletionsRe.FindStringSubmatch(statLine); m != nil {
		dels = m[1]
	}
	suffix := "s"
	if files == "1" {
		suffix = ""
	}
	return fmt.Sprintf("+%s -%s across %s file%s", ins, dels, files, suffix)
}

const implementationBrief = `## Your role

You are the implementation stage of an automated dependency upgrade pipeline.
An assessment agent has already analysed the upstream changes and identified what
needs fixing. Your work will be independently reviewed by a separate review agent
that checks for correctness and consistency with the assessment. The review agent
will flag any deleted tests, workarounds, or unjustified divergences from the plan.

Why this role exists: the tool's goal is to reduce the human attention maintainers
spend on dependency bumps. You exist to make the codebase compatible with the new
version so the maintainer can merge with confidence. The review agent and the
maintainer will both evaluate your work — write clear commit messages that explain
your reasoning so they can judge whether the fix is correct without re-deriving
your analysis.

## Dependency upgrade
- Package: %s
- Old version: %s
- New version: %s
- Ecosystem: %s

## Assessment (from a prior analysis)
%s

### Suggested code changes
%s

## Your task
1. Make the necessary code changes on this branch to be compatible with the new version.
2. Commit your changes with clear commit messages explaining what and why.
3. Push the branch to origin with ` + "`git push`" + `.
4. Open a draft pull request with ` + "`gh pr create --draft`" + ` using exactly this format:

   Title: ` + "`fix(%s): update code for %s %s`" + `

   Body (use this template exactly):
` + "```" + `
## Summary

Automated code changes to make the codebase compatible with %s %s -> %s.

This PR is based on dependabot PR #%d and includes additional code changes
to address breaking changes in the new version.

## What changed upstream

<Summarise the key breaking changes from the dependency upgrade>

## Changes made

<Summarise the code changes you made and why>

---
Original dependabot PR: #%d
_Automated review._
` + "```" + `
5. **End your turn immediately after opening the draft PR and exit.** Do NOT check,
   poll, or monitor CI in any form. You cannot do this efficiently here, and you
   don't need to: the controlling process monitors CI for you. If the change needs
   more work, you will be re-invoked in THIS SAME session with the specific failing
   checks and their logs. When you have pushed and opened the draft PR, stop —
   there is nothing left to do in this turn.
6. Leave the PR as a **draft** — do NOT mark it ready for review.

## Important constraints
- Do NOT delete or disable tests. Do not work around issues by faking results.
- If you cannot fix something, commit what you have, push, open the draft PR, and
  exit — explain what's blocking you in your final commit message.
`

// BuildImplementationBrief builds the brief for the implementation agent.
func BuildImplementationBrief(
	packageName, oldVersion, newVersion, ecosystem string,
	assessmentReviewBody string,
	assessmentCodeChanges []models.CodeChangeEntry,
	dependabotPRNumber int,
) string {
	var codeChangesText string
	if len(assessmentCodeChanges) > 0 {
		var lines []string
		for _, c := range assessmentCodeChanges {
			lines = append(lines, fmt.Sprintf("- %s: %s", c.File, c.Description))
		}
		codeChangesText = strings.Join(lines, "\n")
	} else {
		codeChangesText = "(no specific code changes suggested — use the assessment above to determine what needs changing)"
	}

	return fmt.Sprintf(implementationBrief,
		packageName, oldVersion, newVersion, ecosystem,
		assessmentReviewBody,
		codeChangesText,
		packageName, packageName, newVersion,
		packageName, oldVersion, newVersion,
		dependabotPRNumber,
		dependabotPRNumber,
	)
}

const groupedImplementationBrief = `## Your role

You are the implementation stage of an automated dependency upgrade pipeline.
An assessment agent has already analysed the upstream changes and identified what
needs fixing. Your work will be independently reviewed by a separate review agent
that checks for correctness and consistency with the assessment. The review agent
will flag any deleted tests, workarounds, or unjustified divergences from the plan.

Why this role exists: the tool's goal is to reduce the human attention maintainers
spend on dependency bumps. You exist to make the codebase compatible with the new
version so the maintainer can merge with confidence. The review agent and the
maintainer will both evaluate your work — write clear commit messages that explain
your reasoning so they can judge whether the fix is correct without re-deriving
your analysis.

## Dependency upgrade
- Group: %s
- Ecosystem: %s
- Packages updated (%d):
%s
## Assessment (from a prior analysis)
%s

### Suggested code changes
%s

## Your task
1. Make the necessary code changes on this branch to be compatible with the updated packages.
2. Commit your changes with clear commit messages explaining what and why.
3. Push the branch to origin with ` + "`git push`" + `.
4. Open a draft pull request with ` + "`gh pr create --draft`" + ` using exactly this format:

   Title: ` + "`fix(deps): update code for %s group`" + `

   Body (use this template exactly):
` + "```" + `
## Summary

Automated code changes to make the codebase compatible with the %s group dependency update.

This PR is based on dependabot PR #%d and includes additional code changes
to address breaking changes in the updated packages.

## What changed upstream

<Summarise the key breaking changes from the dependency upgrade>

## Changes made

<Summarise the code changes you made and why>

---
Original dependabot PR: #%d
_Automated review._
` + "```" + `
5. **End your turn immediately after opening the draft PR and exit.** Do NOT check,
   poll, or monitor CI in any form. You cannot do this efficiently here, and you
   don't need to: the controlling process monitors CI for you. If the change needs
   more work, you will be re-invoked in THIS SAME session with the specific failing
   checks and their logs. When you have pushed and opened the draft PR, stop —
   there is nothing left to do in this turn.
6. Leave the PR as a **draft** — do NOT mark it ready for review.

## Important constraints
- Do NOT delete or disable tests. Do not work around issues by faking results.
- If you cannot fix something, commit what you have, push, open the draft PR, and
  exit — explain what's blocking you in your final commit message.
`

// BuildGroupedImplementationBrief builds the brief for the implementation agent
// when handling a grouped dependabot PR (multiple packages bumped together).
func BuildGroupedImplementationBrief(
	pr models.DependabotPR,
	assessmentReviewBody string,
	assessmentCodeChanges []models.CodeChangeEntry,
) string {
	var packageLines strings.Builder
	for _, u := range pr.GroupedUpdates {
		fmt.Fprintf(&packageLines, "    - %s: %s → %s\n", u.Name, u.From, u.To)
	}

	var codeChangesText string
	if len(assessmentCodeChanges) > 0 {
		var lines []string
		for _, c := range assessmentCodeChanges {
			lines = append(lines, fmt.Sprintf("- %s: %s", c.File, c.Description))
		}
		codeChangesText = strings.Join(lines, "\n")
	} else {
		codeChangesText = "(no specific code changes suggested — use the assessment above to determine what needs changing)"
	}

	return fmt.Sprintf(groupedImplementationBrief,
		pr.PackageName, pr.Ecosystem, len(pr.GroupedUpdates), packageLines.String(),
		assessmentReviewBody,
		codeChangesText,
		pr.PackageName,
		pr.PackageName,
		pr.Number,
		pr.Number,
	)
}

// Pipeline manages the full implementation lifecycle for a single PR.
type Pipeline struct {
	config               *config.Config
	github               *ghclient.Client
	reviewer             *reviewer.Reviewer
	workdir              string
	store                progress.Writer // optional; nil for the one-shot `review` command
	logDir               string          // explicit log dir override; when empty, logs go under workdir
	bareClonePath        string          // optional: path to a local bare clone; when set, cloneAndBranch clones locally
	agentJustification   string          // when non-empty, curateBranch is used instead of squashBranch (Q13)

	// bumpTipSHA is the branch's actual tip SHA captured in cloneAndBranch right
	// after `checkout -b branch pr.HeadRef`, before the worker runs — i.e. the
	// *post-rebase* bump commit. It is the single canonical base for the squash
	// (T9), the reviewer diff, and the gave_up skip key. NEVER use the scan-time
	// pr.HeadSHA for these: a Phase-0 rebase rewrites the branch head, so the
	// scan-time SHA is stale and would (a) bundle every unrelated `main` change
	// merged since the branch's original base into the "fix" commit (T9) and
	// (b) make the gave_up SHA-skip miss next cycle, re-entering the agent (N4).
	bumpTipSHA string
}

// NewPipeline creates a new implementation pipeline.
func NewPipeline(cfg *config.Config, gh *ghclient.Client) *Pipeline {
	return &Pipeline{
		config:   cfg,
		github:   gh,
		reviewer: reviewer.NewReviewer(cfg.AnthropicAPIKey, cfg.ReviewerModel, cfg.ReviewerThinkingBudget),
	}
}

// WithStore attaches a live progress store. Used by the daemon subcommands; nil
// for the one-shot `review` command. Returns the receiver for chaining.
func (p *Pipeline) WithStore(s progress.Writer) *Pipeline {
	p.store = s
	return p
}

// WithLogDir sets an explicit directory where per-PR agent JSONL logs are written.
// When not set, logs are written under the canonical per-PR workdir (see
// canonicalWorkdir), which is the preferred path. Set this only when the workdir
// is not used (e.g. one-shot `review` command or legacy deployments).
func (p *Pipeline) WithLogDir(dir string) *Pipeline {
	p.logDir = dir
	return p
}

// WithBareClone sets the path to a local bare clone of the target repository.
// When set, cloneAndBranch clones from this local path instead of from GitHub —
// a cheap local operation rather than a full network clone. The orchestrator
// manages the bare clone lifecycle (ensureBareClone in orchestrator).
func (p *Pipeline) WithBareClone(path string) *Pipeline {
	p.bareClonePath = path
	return p
}

// WithWorkdir sets a pre-prepared working directory for the pipeline to reuse.
// When set, canonicalWorkdir is skipped and this directory is used directly.
// This avoids re-cloning the repo when the combined agent has already prepared
// it — the combined agent and implementation pipeline share the same workdir.
func (p *Pipeline) WithWorkdir(path string) *Pipeline {
	p.workdir = path
	return p
}

// WithAgentJustification sets the combined agent's justification text. When
// non-empty, the finalize step uses curateBranch (Q13) instead of squashBranch
// — the curate agent authors clean logical commits guided by the justification.
func (p *Pipeline) WithAgentJustification(justification string) *Pipeline {
	p.agentJustification = justification
	return p
}

// reportStage is the nil-safe shim for a pipeline progress update.
func (p *Pipeline) reportStage(prNumber int, pkg, bump string, stage models.PRStage, detail string) {
	if p.store != nil {
		p.store.Report(prNumber, pkg, bump, stage, detail)
	}
}

// setImplMeta is the nil-safe shim for recording the worktree/session/branch.
func (p *Pipeline) setImplMeta(prNumber int, sessionID, worktreePath, branch string) {
	if p.store != nil {
		p.store.SetImplMeta(prNumber, sessionID, worktreePath, branch)
	}
}

// setCI is the nil-safe shim for recording the latest CI snapshot.
func (p *Pipeline) setCI(prNumber int, ci models.CIStatus) {
	if p.store != nil {
		p.store.SetCI(prNumber, ci)
	}
}

// RunResult holds the result of an implementation pipeline run.
type RunResult struct {
	Success       bool
	GaveUp        bool // true when the loop gave up (no-progress / exhausted / time-cap); sticky at the SHA
	Detail        string
	ReviewVerdict *models.ReviewVerdict
	Branch        string

	// TipSHA is the post-rebase bump-commit tip captured in cloneAndBranch (the
	// canonical base; see Pipeline.bumpTipSHA). The orchestrator's gave_up path
	// records its terminal outcome against THIS SHA, not the scan-time
	// pr.HeadSHA, so the next scan's SHA-skip fires on the rebased head instead
	// of re-entering the agent (N4 / MAJOR-1). Empty if cloneAndBranch never ran
	// (e.g. an early clone/rebase failure — those return GaveUp=false anyway).
	TipSHA string

	// Justification is the combined agent's structured explanation, held private
	// through the impl↔reviewer loop and posted to the replacement PR body on
	// final approval (Q15). Empty on the legacy analyser path.
	Justification string
}

// Run executes the full implementation pipeline.
//
// The return is named so a single defer can stamp TipSHA (the captured
// post-rebase bump tip) onto every exit path — the orchestrator's gave_up path
// needs it to record the terminal outcome at the rebased head (N4 / MAJOR-1).
// It is empty until cloneAndBranch captures it, which is fine: the paths that
// return before then are transient (GaveUp=false) and stay keyed on pr.HeadSHA.
func (p *Pipeline) Run(ctx context.Context, pr models.DependabotPR, analysis *models.AgentAnalysis) (result RunResult) {
	branch := BuildBranchName(pr.PackageName, pr.NewVersion)
	p.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageImplStarting, "")
	defer p.cleanup()
	defer func() { result.TipSHA = p.bumpTipSHA }()

	// Phase 0: Ensure the PR branch is up to date with base.
	//
	// We always rebase the branch ourselves via git (clone + `rebase -X theirs`
	// + force-push-with-lease) rather than posting `@dependabot rebase`. Two
	// reasons (Bug #2 + Bug #5 + Bug #6):
	//   - Dependabot only acts on PRs it authored, so the comment is a no-op
	//     for our mirror PRs and any non-dependabot author.
	//   - Even for dependabot-authored PRs the comment is not side-effect-free:
	//     Dependabot may decide the update is no longer needed and *close* the
	//     PR (observed on PR #14 — uuid). Driving the rebase ourselves is
	//     deterministic, ~15s, and cannot destroy the PR we're trying to fix.
	behind, err := p.github.IsBranchBehindBase(ctx, pr.Number)
	if err != nil {
		slog.Warn("Could not check if branch is behind base", "pr", pr.Number, "error", err)
	}
	if behind {
		rebaseErr := ensureBranchUpToDate(func() error {
			slog.Info("Branch is behind base — performing manual rebase", "pr", pr.Number, "author", pr.Author)
			return p.manualRebase(ctx, pr)
		})
		if rebaseErr != nil {
			return RunResult{
				Success: false,
				Detail:  fmt.Sprintf("Could not bring branch up to date with base: %v", rebaseErr),
			}
		}
	}

	var createErr error
	if p.workdir == "" {
		// No pre-prepared workdir (e.g. legacy analyser path or standalone pipeline).
		p.workdir, createErr = p.canonicalWorkdir(pr)
		if createErr != nil {
			return RunResult{Success: false, Detail: fmt.Sprintf("Could not create workdir: %v", createErr)}
		}
	}
	slog.Info("Using working directory", "path", p.workdir)

	repoDir, err := p.cloneAndBranch(ctx, pr, branch)
	if err != nil {
		return RunResult{Success: false, Detail: fmt.Sprintf("Clone/branch failed: %v", err)}
	}

	// Determine which CI failures are non-blocking for the success criterion.
	// Q3 (DECIDED): base-branch suppression as a success criterion is dropped —
	// a required check red on main is "more work," not an excuse. The agent must
	// fix those failures too. Only the operator --ignore-check escape hatch remains.
	ignored := p.ignoredChecks()

	// Required-status-check set for the base branch (Q7): when non-empty, only
	// these checks gate the implementation's success criterion; empty falls back
	// to all-checks (M2). Fetched once and reused across every CI-gate poll.
	required := p.github.RequiredChecks(ctx, pr.BaseRef)

	// Phase 3: orchestrator-owned implement → CI-fix → review loop (Bug #15).
	//
	// The worker is now a *bounded turn*: it makes the change, pushes, opens the
	// draft PR, and exits — it never waits for or polls CI. The orchestrator owns
	// the CI wait and the iteration. A single session UUID is pinned on the launch
	// turn so every follow-up turn (--resume) preserves the worker's full context.
	sessionID := newSessionID()
	p.setImplMeta(pr.Number, sessionID, repoDir, branch)
	start := time.Now()

	var brief string
	if pr.Grouped {
		brief = BuildGroupedImplementationBrief(pr, analysis.ReviewBody, analysis.CodeChanges)
	} else {
		brief = BuildImplementationBrief(
			pr.PackageName, pr.OldVersion, pr.NewVersion, pr.Ecosystem,
			analysis.ReviewBody, analysis.CodeChanges, pr.Number,
		)
	}
	brief += suppressedChecksNote(ignored)

	// Turn 1: launch.
	p.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageImplRunning, "launch turn")
	if !p.runWorkerTurn(ctx, repoDir, sessionID, brief, false, pr.Number) {
		return RunResult{Success: false, Detail: "worker launch turn failed", Branch: branch}
	}

	reviewRetriesLeft := p.config.MaxReviewRetries
	var lastVerdict *models.ReviewVerdict
	reviewTurn := 1 // incremented each time the reviewer runs (for its turn-number context)

	for {
		// CI-fix gate: wait for CI to SETTLE, then decide. Only a settled board
		// that is still not acceptable (genuine terminal failures) resumes the
		// worker; a board that hasn't settled (verifyCI timed out) means CI is
		// still running — we keep waiting rather than resume the worker against
		// pending checks it cannot fix (Bug #21). Bounded by MaxImplIterations
		// (resume turns; iter only advances on an actual resume) and MaxImplTime.
		// No-progress metric (Q12): floor = lowest blocking-check count seen so
		// far (math.MaxInt = none yet); stall = consecutive settled attempts since
		// the floor last improved.
		floor := math.MaxInt
		stall := 0
		for iter := 1; ; {
			ci, settled := p.verifyCI(ctx, branch, ignored)
			// Update the dashboard's CI snapshot on every poll iteration so the
			// drawer's CI bar tracks live progress through the impl loop.
			p.setCI(pr.Number, ci)
			// Q3: pass nil for baseFailures — genuine green is the bar;
			// a required check red on main is not suppressed, it must be fixed.
			acceptable, blocking := ci.AcceptableGiven(ignored, nil, required, time.Now(), p.config.CIStaleness)
			if acceptable {
				slog.Info("CI gate passed", "pr", pr.Number, "iter", iter)
				break
			}
			if !settled {
				// CI hasn't finished — the remaining blockers are still running,
				// not failures the worker can fix. Keep waiting; do NOT resume the
				// worker and do NOT consume a fix-iteration. Bounded by MaxImplTime.
				if ctx.Err() != nil || time.Since(start).Seconds() >= float64(p.config.MaxImplTime) {
					return RunResult{
						Success:       false,
						GaveUp:        true,
						Detail:        "CI did not settle within MaxImplTime (still pending: " + strings.Join(blocking, ", ") + ")",
						ReviewVerdict: lastVerdict,
						Branch:        branch,
					}
				}
				p.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageWaitingCI, "CI still running: "+strings.Join(blocking, ", "))
				slog.Info("CI still running — waiting, not resuming worker", "pr", pr.Number, "stillBlocking", blocking)
				continue
			}
			// Settled but not acceptable → genuine terminal failures → resume the
			// worker (bounded), feeding it only the real failing checks + logs.
			verdict, reason := decideCIFixLoop(false, iter, p.config.MaxImplIterations,
				time.Since(start).Seconds(), float64(p.config.MaxImplTime))
			if verdict == ciFixGiveUp {
				detail := "CI not acceptable: " + reason
				if len(blocking) > 0 {
					detail += " (blocking: " + strings.Join(blocking, ", ") + ")"
				}
				return RunResult{
					Success:       false,
					GaveUp:        true,
					Detail:        detail,
					ReviewVerdict: lastVerdict,
					Branch:        branch,
				}
			}
			// No-progress guard (Q12): give up once the lowest failing-check count
			// hasn't improved over MaxNoProgressIterations consecutive settled
			// attempts. A monotonic floor (not a window over raw counts) so a
			// worker that thrashes — fix A breaks B, fix B re-breaks A — can't keep
			// the loop alive forever by oscillating the count; only an actual new
			// low resets the stall. Subsumes the stationary and oscillating cases.
			var giveUp bool
			giveUp, floor, stall = decideNoProgress(len(blocking), floor, stall, p.config.MaxNoProgressIterations)
			if giveUp {
				names := strings.Join(blocking, ", ")
				detail := fmt.Sprintf(
					"Stopped: the failing-check count stalled at a floor of %d across %d fix attempts without improving (still red: %s) — likely pre-existing or beyond an automated bump fix. Flagging for review.",
					floor, p.config.MaxNoProgressIterations, names,
				)
				slog.Info("No-progress give-up", "pr", pr.Number, "blocking", blocking, "floor", floor, "maxStall", p.config.MaxNoProgressIterations)
				return RunResult{
					Success:       false,
					GaveUp:        true,
					Detail:        detail,
					ReviewVerdict: lastVerdict,
					Branch:        branch,
				}
			}
			slog.Info("CI not yet acceptable — resuming worker with failure logs",
				"pr", pr.Number, "iter", iter, "blocking", blocking)
			p.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageResuming,
				fmt.Sprintf("ci-fix iteration %d of %d", iter, p.config.MaxImplIterations))
			logs := p.github.FetchBranchFailureLogs(ctx, ci, pr.Number)
			if !p.runWorkerTurn(ctx, repoDir, sessionID, buildCIFeedback(blocking, logs), true, pr.Number) {
				return RunResult{
					Success:       false,
					Detail:        "worker resume turn (CI fix) failed",
					ReviewVerdict: lastVerdict,
					Branch:        branch,
				}
			}
			iter++
		}

		// Review gate. CI is acceptable; have the reviewer judge the change.
		// Commit list is taken relative to the captured post-rebase bump tip —
		// the agent's own work — not origin/<HeadRef> (M4 / MINOR-1).
		// The reviewer runs as a claude subprocess with full tool access and reads
		// the diff directly via git diff — no pre-fetched, size-capped diff needed.
		commits := p.getBranchCommits(ctx, repoDir, p.bumpTipSHA)
		messages := make([]string, len(commits))
		for i, c := range commits {
			messages[i] = c.Message
		}

		// Capture HEAD SHA for the reviewer brief so it knows the exact commit
		// it is reviewing (the brief template includes HEAD: <sha>).
		var headSHA string
		if tipOut, tipErr := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output(); tipErr == nil {
			headSHA = strings.TrimSpace(string(tipOut))
		}

		p.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageReviewing, "")
		verdict, err := p.reviewer.Review(
			ctx,
			repoDir, p.bumpTipSHA, branch,
			p.workdir, headSHA,
			analysis.ReviewBody, analysis.CodeChanges,
			len(commits), messages,
			reviewTurn,
			p.agentJustification, // Q15: reviewer also evaluates justification (empty on legacy path)
		)
		if err != nil {
			slog.Error("Review failed", "error", err)
			return RunResult{
				Success:       false,
				Detail:        fmt.Sprintf("Review agent error: %v", err),
				ReviewVerdict: lastVerdict,
				Branch:        branch,
			}
		}

		if verdict.Verdict == "approve" {
			// Finalize: curate the commit history (Q13) if an agent justification is
			// available (combined agent path), otherwise fall back to squashBranch
			// (legacy analyser path). Both preserve the two-commit structure:
			// bump commit + agent work. The bumpTipSHA MUST be the live post-rebase
			// tip (T9) so neither operation bundles unrelated `main` changes.
			var finalizeErr error
			if p.agentJustification != "" {
				// Q13 curate: the curate agent authors logical commits from the staged diff.
				p.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFinalized, "curating commit history")
				finalizeErr = p.curateBranch(ctx, repoDir, p.bumpTipSHA, branch, p.agentJustification, pr.Number)
			} else {
				// Legacy path: single squash commit.
				var agentMsg string
				if pr.Grouped {
					agentMsg = fmt.Sprintf("fix: update code for %s compatibility", pr.PackageName)
				} else {
					agentMsg = fmt.Sprintf("fix: update code for %s %s → %s compatibility",
						pr.PackageName, pr.OldVersion, pr.NewVersion)
				}
				finalizeErr = p.squashBranch(ctx, repoDir, p.bumpTipSHA, branch, agentMsg)
			}
			if finalizeErr != nil {
				slog.Error("Finalize (curate/squash) of impl branch failed", "pr", pr.Number, "error", finalizeErr)
				return RunResult{
					Success:       false,
					Detail:        fmt.Sprintf("finalize at approval failed: %v", finalizeErr),
					ReviewVerdict: verdict,
					Branch:        branch,
				}
			}

			// Post-squash CI re-verification (Bug #27): the squash+force-push
			// restarts CI on the replacement branch. The orchestrator un-drafts the
			// PR solely on result.Success, so we must verify CI is still acceptable
			// here before returning Success — otherwise the PR is taken out of draft
			// while post-squash CI is failing or pending.
			//
			// Cap at 30 minutes: if CI hasn't settled by then the branch needs human
			// attention anyway. GaveUp=true so the orchestrator records a terminal
			// outcome at this head SHA (no token burn on the next cycle).
			p.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageWaitingCI,
				"waiting for post-squash CI")
			const postSquashCap = 30 * time.Minute
			postSquashCtx, cancelPostSquash := context.WithTimeout(ctx, postSquashCap)
			defer cancelPostSquash()
			postCI, postSettled := p.verifyCI(postSquashCtx, branch, ignored)
			p.setCI(pr.Number, postCI)
			if !postSettled {
				return RunResult{
					Success:       false,
					GaveUp:        true,
					Detail:        "post-squash CI did not settle within 30 minutes",
					ReviewVerdict: verdict,
					Branch:        branch,
				}
			}
			// Q3: pass nil for baseFailures — genuine green is the bar.
			postAcceptable, postBlocking := postCI.AcceptableGiven(ignored, nil, required, time.Now(), p.config.CIStaleness)
			if !postAcceptable {
				return RunResult{
					Success:       false,
					GaveUp:        true,
					Detail:        "post-squash CI not acceptable: " + strings.Join(postBlocking, ", "),
					ReviewVerdict: verdict,
					Branch:        branch,
				}
			}

			p.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageFinalized, "implementation complete and review approved")
			return RunResult{
				Success:       true,
				Detail:        "Implementation complete and review approved",
				ReviewVerdict: verdict,
				Branch:        branch,
				Justification: p.agentJustification, // Q15: propagate for posting to replacement PR body
			}
		}

		// request_changes: resume the SAME session with the reviewer's concerns
		// (preserving context) rather than rebuilding a fresh brief. Bounded by
		// MaxReviewRetries; after a review-fix turn we re-enter the CI gate, since
		// the new code must still pass CI.
		lastVerdict = verdict
		if reviewRetriesLeft <= 0 {
			concerns := "see review"
			if len(verdict.Concerns) > 0 {
				concerns = strings.Join(verdict.Concerns, "; ")
			}
			return RunResult{
				Success:       false,
				Detail:        fmt.Sprintf("Review agent rejected after retries: %s", concerns),
				ReviewVerdict: verdict,
				Branch:        branch,
			}
		}
		reviewRetriesLeft--
		reviewTurn++
		slog.Info("Review agent rejected — resuming worker with concerns",
			"pr", pr.Number, "retriesLeft", reviewRetriesLeft)
		p.reportStage(pr.Number, pr.PackageName, string(pr.BumpType), models.StageResuming, "review-fix")
		if !p.runWorkerTurn(ctx, repoDir, sessionID, buildReviewFeedback(verdict.Concerns), true, pr.Number) {
			return RunResult{
				Success:       false,
				Detail:        "worker resume turn (review fix) failed",
				ReviewVerdict: verdict,
				Branch:        branch,
			}
		}
		// Loop: re-enter the CI gate with the review-fix changes.
	}
}

// ignoredChecks returns the set of operator-ignored CI check names from
// config.IgnoreChecks. These are the only checks the implementation CI gate
// suppresses (Q3): base-branch suppression as a success criterion was dropped
// in Q3 — a required check red on main is more work, not an excuse; the agent
// fixes those too. Only the --ignore-check escape hatch remains.
func (p *Pipeline) ignoredChecks() map[string]bool {
	ignored := make(map[string]bool, len(p.config.IgnoreChecks))
	for _, name := range p.config.IgnoreChecks {
		ignored[name] = true
	}
	return ignored
}

// suppressedChecksNote builds a brief addendum telling the implementation
// agent which CI checks are operator-ignored (structurally broken / irrelevant),
// so it doesn't waste budget on them. Returns "" when the ignore list is empty.
// Q3: base-branch failures are no longer suppressed — the agent must fix those
// too. Only the operator --ignore-check list is passed here.
func suppressedChecksNote(ignored map[string]bool) string {
	if len(ignored) == 0 {
		return ""
	}
	names := make([]string, 0, len(ignored))
	for n := range ignored {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("\n\n## Operator-ignored CI checks (do NOT try to fix these)\n")
	b.WriteString("The following CI checks are structurally broken or irrelevant on this repo. ")
	b.WriteString("Do not spend effort making them pass; they will be disregarded when judging success:\n")
	for _, n := range names {
		b.WriteString("- " + n + "\n")
	}
	return b.String()
}

// ensureBranchUpToDate brings a PR branch up to date with its base.
//
// We deliberately always drive the rebase ourselves (the injected manualRebase)
// rather than asking Dependabot. Posting `@dependabot rebase` is a no-op for
// PRs Dependabot didn't author, and for ones it did author it can decide the
// update is obsolete and *close* the PR (Bug #6, observed on PR #14). A direct
// git rebase is deterministic and cannot destroy the PR. The operation is
// injected so the control flow stays unit-testable without real git I/O.
func ensureBranchUpToDate(manualRebase func() error) error {
	return manualRebase()
}

// manualRebase clones the repo, rebases the PR's head branch onto its base
// using `-X theirs` (favouring the branch's version of conflicting files,
// which is the right choice for dependency-bump conflicts in package.json /
// yarn.lock / etc.), and force-pushes the result. Used when the PR was not
// authored by Dependabot so the `@dependabot rebase` comment path doesn't
// apply.
func (p *Pipeline) manualRebase(ctx context.Context, pr models.DependabotPR) error {
	// Known short-lived exception: this temp dir exists only for the duration of
	// the rebase and is cleaned up by the defer below — it is not a PR-keyed
	// resource. The rebase always completes (or fails) before p.workdir is created,
	// so there is no overlap with the PR-keyed root. The exception is intentional:
	// the rebase is a pre-clone operation and has no workdir to nest under yet.
	workDir, err := os.MkdirTemp("", "sweeper-rebase-*")
	if err != nil {
		return fmt.Errorf("creating rebase workdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	repoName := p.github.RepoFullName()
	repoDir := filepath.Join(workDir, "repo")

	slog.Info("Cloning repo for rebase", "repo", repoName, "pr", pr.Number)
	cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// Auth via GH_TOKEN through gitCredentialHelper — token never enters the URL.
	clone := exec.CommandContext(cloneCtx, "git",
		"-c", "credential.helper=",
		"-c", "credential.helper="+gitCredentialHelper,
		"clone", "--no-checkout", "--filter=blob:none", tokenlessCloneURL(repoName), repoDir)
	clone.Env = p.gitEnv()
	if out, err := clone.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone failed: %w\n%s", err, out)
	}

	// Persist the credential helper so the later fetch/push authenticate too.
	if out, err := exec.Command("git", "-C", repoDir, "config", "credential.helper", gitCredentialHelper).CombinedOutput(); err != nil {
		return fmt.Errorf("git config credential.helper failed: %w\n%s", err, out)
	}

	// Fetch the head branch as a local branch (so we can checkout it). The
	// base ref is already available as origin/<baseRef> via the clone's
	// default refspec — no need to materialize it locally, and trying to
	// fetch base:base would conflict with the (logically checked-out) main.
	fetchCtx, cancel2 := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel2()
	fetch := exec.CommandContext(fetchCtx, "git", "-C", repoDir, "fetch", "origin",
		fmt.Sprintf("%s:%s", pr.HeadRef, pr.HeadRef))
	fetch.Env = p.gitEnv()
	if out, err := fetch.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch failed: %w\n%s", err, out)
	}

	// Identity for the rebase committer. Author of each replayed commit is
	// preserved (dependabot[bot] for the bump); only the committer changes.
	if out, err := exec.Command("git", "-C", repoDir, "config", "user.name", p.config.BotName).CombinedOutput(); err != nil {
		return fmt.Errorf("git config user.name failed: %w\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "config", "user.email", p.config.BotEmail).CombinedOutput(); err != nil {
		return fmt.Errorf("git config user.email failed: %w\n%s", err, out)
	}

	if out, err := exec.Command("git", "-C", repoDir, "checkout", pr.HeadRef).CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout failed: %w\n%s", err, out)
	}

	rebaseCtx, cancel3 := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel3()
	if out, err := exec.CommandContext(rebaseCtx, "git", "-C", repoDir, "rebase", "-X", "theirs", "origin/"+pr.BaseRef).CombinedOutput(); err != nil {
		// Best-effort abort to leave the workdir in a clean state (we'll rm
		// it anyway, but if the rebase persisted in any way, clean up).
		_ = exec.Command("git", "-C", repoDir, "rebase", "--abort").Run()
		return fmt.Errorf("git rebase failed: %w\n%s", err, out)
	}

	pushCtx, cancel4 := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel4()
	push := exec.CommandContext(pushCtx, "git", "-C", repoDir, "push", "--force-with-lease", "origin", pr.HeadRef)
	push.Env = p.gitEnv()
	if out, err := push.CombinedOutput(); err != nil {
		return fmt.Errorf("git push failed: %w\n%s", err, out)
	}

	slog.Info("Manual rebase succeeded", "pr", pr.Number, "branch", pr.HeadRef)
	return nil
}

func (p *Pipeline) cloneAndBranch(ctx context.Context, pr models.DependabotPR, branch string) (string, error) {
	repoDir := filepath.Join(p.workdir, "repo")
	repoName := p.github.RepoFullName()

	cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if p.bareClonePath != "" {
		// Fast path: clone from the local bare clone — no network, no credentials.
		slog.Info("Cloning repo from local bare clone", "bare", p.bareClonePath)
		clone := exec.CommandContext(cloneCtx, "git",
			"clone", "--no-checkout", "--local", p.bareClonePath, repoDir)
		if out, err := clone.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git clone (local) failed: %w\n%s", err, out)
		}
		// Point origin at the tokenless GitHub URL so the worker's git push
		// (authenticated via gitCredentialHelper + GH_TOKEN) goes to GitHub.
		if out, err := exec.Command("git", "-C", repoDir, "remote", "set-url", "origin", tokenlessCloneURL(repoName)).CombinedOutput(); err != nil {
			return "", fmt.Errorf("git remote set-url failed: %w\n%s", err, out)
		}
	} else {
		// Network path: clone directly from GitHub.
		slog.Info("Cloning repo from GitHub", "repo", repoName)
		// -c credential.helper="" first resets any inherited helper, then ours is
		// added — so auth comes solely from GH_TOKEN via gitCredentialHelper.
		clone := exec.CommandContext(cloneCtx, "git",
			"-c", "credential.helper=",
			"-c", "credential.helper="+gitCredentialHelper,
			"clone", "--no-checkout", "--filter=blob:none", tokenlessCloneURL(repoName), repoDir)
		clone.Env = p.gitEnv()
		if out, err := clone.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git clone failed: %w\n%s", err, out)
		}
	}

	// Persist the credential helper into the repo's local config so the worker's
	// own `git push` (run as a subprocess with GH_TOKEN in its env) authenticates
	// without the token ever being written to the remote URL.
	if out, err := exec.Command("git", "-C", repoDir, "config", "credential.helper", gitCredentialHelper).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git config credential.helper failed: %w\n%s", err, out)
	}

	fetchCtx, cancel2 := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel2()
	fetch := exec.CommandContext(fetchCtx, "git", "-C", repoDir, "fetch", "origin", fmt.Sprintf("%s:%s", pr.HeadRef, pr.HeadRef))
	fetch.Env = p.gitEnv()
	if out, err := fetch.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git fetch failed: %w\n%s", err, out)
	}

	if out, err := exec.Command("git", "-C", repoDir, "checkout", "-b", branch, pr.HeadRef).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git checkout failed: %w\n%s", err, out)
	}

	// Capture the branch's actual tip SHA NOW — before the worker runs — as the
	// canonical post-rebase bump commit (the T9 fix). This is the single base
	// the squash and the reviewer diff reset against, and the SHA the gave_up
	// path records its outcome at. It must come from the live checkout, never
	// from the stale scan-time pr.HeadSHA (which a Phase-0 rebase rewrites).
	tip, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD (capturing bump tip): %w", err)
	}
	p.bumpTipSHA = strings.TrimSpace(string(tip))
	if p.bumpTipSHA == "" {
		return "", fmt.Errorf("captured an empty bump-tip SHA after checkout of %s", pr.HeadRef)
	}

	// Configure git identity for the bot account
	exec.Command("git", "-C", repoDir, "config", "user.name", p.config.BotName).Run()
	exec.Command("git", "-C", repoDir, "config", "user.email", p.config.BotEmail).Run()

	slog.Info("Created branch from dependabot head", "branch", branch, "headRef", pr.HeadRef, "bumpTip", p.bumpTipSHA)
	return repoDir, nil
}

// newSessionID returns a RFC-4122 v4 UUID using crypto/rand (stdlib only). The
// orchestrator pins this on the worker's launch turn (--session-id) so that
// later CI-fix / review-fix turns can --resume the same session, preserving the
// worker's full prior-turn context.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; fall back to a timestamp-derived id so
		// we still produce a usable (if non-random) session id rather than panic.
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// runWorkerTurn runs ONE bounded worker turn (launch or resume) to completion.
// The worker makes its change, pushes, opens/refreshes the draft PR, and exits;
// it does not wait for CI. Returns true on a clean exit. Events stream to the
// per-PR log (opened with O_APPEND so resume turns accumulate in one file).
func (p *Pipeline) runWorkerTurn(ctx context.Context, repoDir, sessionID, input string, resume bool, prNumber int) bool {
	turnCtx, cancel := context.WithTimeout(ctx, time.Duration(p.config.MaxImplTime)*time.Second)
	defer cancel()

	cmd := workerCommand(sessionID, resume, p.config.MaxImplBudget, p.config.ImplModel)

	// Per-PR live event log. --output-format stream-json --verbose makes the
	// worker emit each event (turn, tool_use, tool_result, result) as a JSON
	// line in real time, captured here so a run can be monitored live (tail -f)
	// and diagnosed afterwards. O_APPEND keeps every turn of the session in one
	// file across launch + resume invocations.
	effectiveLogDir := p.logDir
	if effectiveLogDir == "" {
		if p.workdir != "" {
			// Preferred: co-locate logs with the per-PR workdir so they are
			// accessible for the duration of the PR and cleaned up with it.
			effectiveLogDir = p.workdir
		} else {
			effectiveLogDir = filepath.Join(os.TempDir(), "sweeper-agent-logs")
		}
	}
	if err := os.MkdirAll(effectiveLogDir, 0o755); err != nil {
		slog.Warn("could not create agent log dir", "dir", effectiveLogDir, "error", err)
	}
	logPath := filepath.Join(effectiveLogDir, fmt.Sprintf("pr-%d-agent.jsonl", prNumber))
	logFile, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if logErr != nil {
		slog.Warn("could not open agent log file; worker output will be lost", "path", logPath, "error", logErr)
	} else {
		defer logFile.Close()
	}

	slog.Info("Worker turn",
		"pr", prNumber,
		"session", sessionID,
		"resume", resume,
		"model", p.config.ImplModel,
		"budget", p.config.MaxImplBudget,
		"timeCap", p.config.MaxImplTime,
		"log", logPath)

	proc := exec.CommandContext(turnCtx, cmd[0], cmd[1:]...)
	proc.Dir = repoDir
	proc.Env = append(os.Environ(), "GH_TOKEN="+p.config.GitHubToken)
	if logFile != nil {
		proc.Stdout = logFile
		proc.Stderr = logFile
	}

	stdin, err := proc.StdinPipe()
	if err != nil {
		slog.Error("worker stdin pipe", "error", err)
		return false
	}

	if err := proc.Start(); err != nil {
		slog.Error("worker start", "error", err)
		return false
	}

	// Write the turn's input (brief or feedback) and close stdin so the worker
	// proceeds.
	if _, err := stdin.Write([]byte(input)); err != nil {
		slog.Warn("failed writing brief to worker stdin", "pr", prNumber, "error", err)
	}
	stdin.Close()

	if err := proc.Wait(); err != nil {
		if turnCtx.Err() != nil {
			slog.Warn("worker turn hit the time cap", "pr", prNumber, "timeCapSecs", p.config.MaxImplTime)
		} else {
			slog.Warn("worker turn ended non-zero", "pr", prNumber, "error", err)
		}
		return false
	}
	return true
}

// buildCIFeedback is the stdin for a RESUME turn: it tells the worker which
// bump-related checks are still failing (with log excerpts) and to fix + push +
// exit. The worker keeps all prior context from its launch turn. Pure.
func buildCIFeedback(blocking []string, logs map[string]string) string {
	var sb strings.Builder
	sb.WriteString("Your pushed change still has failing CI checks that are caused by this dependency bump. ")
	sb.WriteString("Fix them, commit, push to the SAME branch, and then exit your turn. ")
	sb.WriteString("Do NOT monitor or poll CI yourself — you'll be re-invoked if more work is needed.\n\n")
	for _, name := range blocking {
		sb.WriteString("## Failing check: " + name + "\n")
		if log := logs[name]; log != "" {
			sb.WriteString("```\n" + log + "\n```\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// buildReviewFeedback is the stdin for a RESUME turn after the review agent
// requested changes: it lists the reviewer's concerns and tells the worker to
// address them, push to the same branch, and exit. The worker keeps all prior
// context from its earlier turns. Pure.
func buildReviewFeedback(concerns []string) string {
	var sb strings.Builder
	sb.WriteString("The review agent examined your change and requested changes. ")
	sb.WriteString("Address the concerns below, commit, and push to the SAME branch, then exit your turn. ")
	sb.WriteString("Do NOT monitor or poll CI yourself — you'll be re-invoked if more work is needed.\n\n")
	sb.WriteString("## Review concerns\n")
	for _, c := range concerns {
		sb.WriteString("- " + c + "\n")
	}
	return sb.String()
}

// workerCommand builds the claude CLI args for one bounded worker turn. A launch
// turn pins a known --session-id (so the orchestrator can resume it); a resume
// turn continues that session with new stdin. Stays on --print (keeps
// --max-budget-usd). --bare is deliberately NOT used: it disables hooks, skills,
// and plugins that may be installed on the managed GCP instance — blocking them
// is the same principle violation as restricting tools. The agent must have access
// to all installed capabilities. Pure.
func workerCommand(sessionID string, resume bool, budgetUSD float64, model string) []string {
	cmd := []string{"claude", "--print", "--dangerously-skip-permissions",
		"--output-format", "stream-json", "--verbose",
		"--max-budget-usd", fmt.Sprintf("%.2f", budgetUSD)}
	if resume {
		cmd = append(cmd, "--resume", sessionID)
	} else {
		cmd = append(cmd, "--session-id", sessionID)
	}
	if model != "" {
		cmd = append(cmd, "--model", model)
	}
	return cmd
}

type ciFixVerdict int

const (
	ciFixContinue ciFixVerdict = iota // CI not acceptable, iterations remain -> resume worker
	ciFixDone                         // CI acceptable -> proceed to review
	ciFixGiveUp                       // out of iterations or time -> fail
)

// decideCIFixLoop is the orchestrator's gate after each worker turn: it decides
// whether the bump-related CI is acceptable (done), should iterate (resume the
// worker with the failure logs), or should give up (out of turns / time). Pure.
func decideCIFixLoop(ciAcceptable bool, iteration, maxIterations int, elapsedSecs, maxTimeSecs float64) (ciFixVerdict, string) {
	switch {
	case ciAcceptable:
		return ciFixDone, "CI acceptable for the bump"
	case elapsedSecs >= maxTimeSecs:
		return ciFixGiveUp, fmt.Sprintf("time cap reached (%.0fs)", maxTimeSecs)
	case iteration > maxIterations:
		return ciFixGiveUp, fmt.Sprintf("exhausted %d CI-fix iterations", maxIterations)
	default:
		return ciFixContinue, ""
	}
}

// decideNoProgress detects a stalled CI-fix loop using a monotonic progress
// metric (Q12, replacing the old exact-set guard). It tracks `floor` — the
// lowest blocking-check count seen across settled attempts — and `stall`, the
// number of consecutive settled attempts since `floor` last improved. It reports
// give-up once `stall` reaches `maxStall`.
//
// A monotonic floor (rather than a sliding window over raw counts) cannot be
// gamed by up-down oscillation: a worker that thrashes 5→4→5→4 never lowers the
// floor below 4, so `stall` keeps climbing and the loop still terminates. This
// subsumes both the stationary-stuck-set case and the oscillating-thrash case
// the old exact-set guard missed (T10/Q12). Pure and unit-testable.
//
// Call with floor=math.MaxInt and stall=0 initially. blockingCount is the number
// of genuinely-blocking checks this settled attempt (len(blocking)); it is ≥1
// here, since the loop only reaches this point when CI settled but was not
// acceptable. The first call always records a new floor (no give-up), so a
// give-up means maxStall consecutive *non-improving* attempts after the best low.
func decideNoProgress(blockingCount, floor, stall, maxStall int) (giveUp bool, newFloor, newStall int) {
	if blockingCount < floor {
		return false, blockingCount, 0 // progress: a new low resets the stall
	}
	stall++ // no improvement (equal or worse)
	return stall >= maxStall, floor, stall
}

// verifyCI waits until the branch's CI has SETTLED (every check terminal or
// stale) and returns the settled status with settled=true. If it can't reach a
// settled state within maxWait (or the context is cancelled), it returns the
// last snapshot with settled=false — the caller MUST treat that as "CI still
// running", not as failures to fix (Bug #21). It waits for Settled() — NOT
// merely until the aggregate State stops being "pending" — because State flips
// to "failure" the instant one check fails (e.g. meta-changelog-pr fails
// immediately on forks), which would otherwise return with most checks still
// running. Settled() is the same generic per-check logic as the orchestrator's
// Step 3 gate (Spec A / Bug #18).
func (p *Pipeline) verifyCI(ctx context.Context, branch string, ignored map[string]bool) (models.CIStatus, bool) {
	slog.Info("Independently verifying CI", "branch", branch)
	// Generous cap: the fork's slow checks (generic-worker macOS/Windows) can
	// take a while; we return as soon as CI is SETTLED, so this is only an upper
	// bound, not a fixed wait.
	maxWait := time.Duration(p.config.CIVerifyMaxWait) * time.Second
	start := time.Now()
	last := models.CIStatus{State: "pending"}
	for time.Since(start) < maxWait {
		if err := ctx.Err(); err != nil {
			slog.Warn("CI verification cancelled", "error", err)
			return last, false
		}
		if ci, err := p.github.GetBranchCI(ctx, branch); err == nil {
			last = *ci
			if settled, pending := ci.Settled(time.Now(), p.config.CIStaleness, ignored); settled {
				slog.Info("CI verification complete (settled)", "state", ci.State, "passed", ci.Passed, "total", ci.Total)
				return *ci, true
			} else {
				slog.Info("CI not settled — waiting", "branch", branch,
					"stillPending", len(pending), "elapsed", time.Since(start).Round(time.Second).String())
			}
		}
		select {
		case <-ctx.Done():
			return last, false
		case <-time.After(30 * time.Second):
		}
	}
	slog.Warn("CI verification timed out (not settled)", "seconds", maxWait.Seconds())
	return last, false
}

// getBranchCommits lists the agent's commits on top of baseSHA (the captured
// post-rebase bump tip), baseSHA..HEAD, surfaced to the reviewer.
func (p *Pipeline) getBranchCommits(ctx context.Context, repoDir, baseSHA string) []models.CommitInfo {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "log", "--oneline", fmt.Sprintf("%s..HEAD", baseSHA)).Output()
	if err != nil {
		return nil
	}
	var commits []models.CommitInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		sha, message, _ := strings.Cut(line, " ")
		statOut, err := exec.CommandContext(ctx, "git", "-C", repoDir, "diff", "--stat", sha+"~1.."+sha).Output()
		diffStat := "(no stats)"
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(statOut)), "\n")
			if len(lines) > 0 {
				diffStat = ParseDiffStat(lines[len(lines)-1])
			}
		}
		commits = append(commits, models.CommitInfo{SHA: sha, Message: message, DiffStat: diffStat})
	}
	return commits
}

// squashBranch squashes the agent's multi-turn commit history into a single
// "fix:" commit on top of bumpTipSHA (the captured post-rebase bump commit),
// then force-pushes. This is the legacy finalize path — use curateBranch
// (Q13) for the agent-curated commit history. squashBranch is retained as a
// fallback for the legacy analyser path (--legacy-analyser).
//
// Preserves the two-commit structure (bump + agent fix) so reviewers can see
// what the agent changed independently of the version bump. If the agent made
// no net changes, the second commit is skipped and only the bump is pushed.
// bumpTipSHA MUST be the live post-rebase tip (T9).
func (p *Pipeline) squashBranch(ctx context.Context, repoDir, bumpTipSHA, branch, agentMessage string) error {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "reset", "--soft", bumpTipSHA).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git reset --soft %s: %w: %s", bumpTipSHA, err, out)
	}
	if checkCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "diff", "--cached", "--quiet"); checkCmd.Run() != nil {
		out, err = exec.CommandContext(ctx, "git", "-C", repoDir, "commit", "-m", agentMessage).CombinedOutput()
		if err != nil {
			return fmt.Errorf("squashBranch: git commit: %w: %s", err, out)
		}
	}
	push := exec.CommandContext(ctx, "git", "-C", repoDir, "push", "--force-with-lease", "origin", branch)
	push.Env = p.gitEnv()
	if out, err = push.CombinedOutput(); err != nil {
		return fmt.Errorf("force-push: %w: %s", err, out)
	}
	slog.Info("Squashed agent commits onto bump commit", "branch", branch, "bumpTipSHA", bumpTipSHA)
	return nil
}

const curateBranchBrief = `## Your role

You are the history-curation stage of an automated dependency upgrade pipeline.
An implementation agent has already made code changes to fix compatibility issues
introduced by a dependency bump. Those changes are scattered across multiple
work-in-progress commits from the fix-and-retry loop. Your job is to replace
that raw history with one or more clean, intentional, well-messaged logical commits.

Why this step exists: the PR that humans will review must have a readable history.
The implementation agent's WIP commits are internal scaffolding — they reflect the
iteration process, not the logical structure of the change. You own the final shape
of the commit history that lands in the PR.

## Working context

Repo directory: %s
Branch: %s
Bump tip SHA: %s (post-rebase bump commit — this is your base; do NOT touch it)

## What to do

1. Run ` + "`git log --oneline %s..HEAD`" + ` to see the WIP commits you are replacing.
2. Run ` + "`git diff %s..HEAD`" + ` to understand the full set of changes.
3. Soft-reset to the bump tip: ` + "`git reset --soft %s`" + `
   All agent changes are now staged. They will NOT be lost — they are staged,
   ready for you to split and re-commit as logical units.
4. Re-commit as one or more logical commits with clear, precise commit messages.
   - Each commit should represent one coherent unit of change.
   - Commit messages must follow the conventional-commit format: ` + "`fix(<scope>): <description>`" + `
   - The message must explain WHY the change is needed, not just WHAT changed.
   - Do NOT reproduce the diff in the message — reference the upstream change instead.
5. Force-push: ` + "`git push --force-with-lease origin %s`" + `

## Justification context

The implementation was guided by this analysis:
%s

## Constraints

- Do NOT delete or modify the bump commit (%s) — only the commits ABOVE it.
- Do NOT add new code changes — only re-organize what is already staged.
- Do NOT open or modify any pull request — the controlling process handles that.
- If the staged diff is empty (agent made no net changes), push without committing
  additional commits: just force-push the current state.
- End your turn immediately after force-pushing.
`

// curateBranch replaces the implementation agent's WIP commit history with
// clean logical commits authored by a curate agent (Q13). The curate agent
// soft-resets to bumpTipSHA (preserving the bump commit) and re-commits the
// staged changes as intentional, well-messaged commits, then force-pushes.
//
// This replaces squashBranch for the combined-agent path. squashBranch is
// retained as a fallback for the legacy analyser (--legacy-analyser).
func (p *Pipeline) curateBranch(ctx context.Context, repoDir, bumpTipSHA, branch, justification string, prNumber int) error {
	brief := fmt.Sprintf(curateBranchBrief,
		repoDir,
		branch,
		bumpTipSHA,
		bumpTipSHA, bumpTipSHA, bumpTipSHA, // for git log/diff/reset
		branch, // for push
		justification,
		bumpTipSHA,
	)

	// Log path: co-located with the workdir.
	logPath := ""
	if p.workdir != "" {
		logPath = filepath.Join(p.workdir, fmt.Sprintf("pr-%d-curate.jsonl", prNumber))
	}

	slog.Info("Running curate agent", "branch", branch, "bumpTipSHA", bumpTipSHA)

	const maxAttempts = 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := p.runCurateSubprocess(ctx, repoDir, brief, logPath, prNumber); err != nil {
			slog.Warn("curate agent attempt failed", "attempt", attempt, "error", err)
			if attempt == maxAttempts {
				return fmt.Errorf("curate agent failed after %d attempts: %w", maxAttempts, err)
			}
			continue
		}
		slog.Info("curate agent completed", "branch", branch)
		return nil
	}
	return fmt.Errorf("curate agent: unreachable")
}

// runCurateSubprocess invokes claude as a subprocess for the curate step.
func (p *Pipeline) runCurateSubprocess(ctx context.Context, repoDir, brief, logPath string, prNumber int) error {
	curateCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	args := []string{
		"claude", "--print", "--dangerously-skip-permissions",
		"--output-format", "text",
		"--max-budget-usd", "5.00", // curate is a focused, bounded task
	}
	// Use the impl model if configured; otherwise let the claude CLI pick.
	if p.config.ImplModel != "" {
		args = append(args, "--model", p.config.ImplModel)
	}

	proc := exec.CommandContext(curateCtx, args[0], args[1:]...)
	proc.Dir = repoDir
	proc.Env = p.gitEnv() // so git push can authenticate

	var stdout bytes.Buffer
	proc.Stdout = &stdout
	proc.Stderr = &stdout

	stdin, err := proc.StdinPipe()
	if err != nil {
		return fmt.Errorf("curate stdin pipe: %w", err)
	}
	if err := proc.Start(); err != nil {
		return fmt.Errorf("curate subprocess start: %w", err)
	}
	if _, err := stdin.Write([]byte(brief)); err != nil {
		slog.Warn("failed writing brief to curate stdin", "pr", prNumber, "error", err)
	}
	stdin.Close()

	if err := proc.Wait(); err != nil {
		if curateCtx.Err() != nil {
			return fmt.Errorf("curate subprocess hit the time cap (20 minutes)")
		}
		slog.Warn("curate subprocess ended non-zero", "pr", prNumber, "error", err)
		// Non-zero exit after a timeout or explicit error — check if the output
		// suggests a real failure (not just a non-zero git exit after a push).
		if stdout.Len() == 0 {
			return fmt.Errorf("curate subprocess produced no output and exited non-zero: %w", err)
		}
		// Non-zero but has output — the push may have succeeded; treat as success.
		slog.Warn("curate subprocess exited non-zero but produced output — treating as possible success", "pr", prNumber)
	}

	if logPath != "" {
		appendToLog(logPath, stdout.String())
	}

	return nil
}

// canonicalWorkdir returns the stable per-PR working directory path:
//
//	<DataDir>/pr/<owner>-<repo>/pr-<N>/
//
// This is preferred over os.MkdirTemp for two reasons:
//  1. The path is deterministic, so operator tooling can inspect it by PR number.
//  2. The orchestrator's closed-PR sweep can delete it when the PR is closed.
//
// If the directory already exists (crash residue from a prior run), it is deleted
// and recreated — the program never silently reuses potentially dirty state. This
// is safe because agents work in their own clones and never write to the bare
// clone.
//
// Falls back to os.MkdirTemp when DataDir is empty (one-shot `review` command or
// legacy deployments that have not set SWEEPER_DATA_DIR).
func (p *Pipeline) canonicalWorkdir(pr models.DependabotPR) (string, error) {
	if p.config.DataDir == "" {
		// Legacy / one-shot path: fall back to a temp directory.
		return os.MkdirTemp("", "sweeper-impl-*")
	}
	repoSlug := strings.ReplaceAll(p.github.RepoFullName(), "/", "-")
	dir := filepath.Join(p.config.DataDir, "pr", repoSlug, fmt.Sprintf("pr-%d", pr.Number))

	// If the directory already exists (crash residue), remove it to avoid
	// silently reusing dirty state from a prior run.
	if _, err := os.Stat(dir); err == nil {
		slog.Info("Removing stale workdir (crash residue)", "path", dir)
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("removing stale workdir %s: %w", dir, err)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating canonical workdir %s: %w", dir, err)
	}
	return dir, nil
}

// appendToLog appends text to a log file, creating it if needed. Best-effort.
func appendToLog(path, text string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(text) //nolint:errcheck
}

func (p *Pipeline) cleanup() {
	if p.workdir != "" {
		// Only remove the workdir if it is a temp dir (DataDir not set). When
		// DataDir is set, the orchestrator's reapClosed sweep owns deletion — the
		// directory persists so logs and artefacts are available until the PR closes.
		if p.config.DataDir == "" {
			os.RemoveAll(p.workdir)
			slog.Info("Cleaned up working directory", "path", p.workdir)
		} else {
			slog.Info("Workdir preserved for post-mortem / log serving", "path", p.workdir)
		}
	}
}
