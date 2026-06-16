# Agent Pipeline Empowerment Audit

**Date:** 2026-06-16  
**Trigger:** mdi-react 6.7.0→9.4.0 analysis produced "icon names are unlikely to have been
renamed" — a forced guess caused by missing tools and capped pre-filtered data, not a prompt
quality problem.  
**Principle violated:** See `docs/PRINCIPLES.md` → *Agent empowerment principle*.  
**Status:** Findings captured; implementation tracked in `docs/WORKPLAN.md` Phase 6.

This file is the **reference document for both implementation and post-implementation review.**
When Phase 6 items are implemented, use this file to verify every finding was addressed and
every mitigation actually closes the gap described.

---

## Pipeline stages audited

### Stage 1 — Orchestrator (`internal/orchestrator/orchestrator.go`)

**Verdict: well-designed. No violation.**

The orchestrator owns structural gates (CI settle/acceptability, idempotency SHA checks,
staleness, base-branch failure suppression) and sequences stage transitions. It does not
reason about dependency content or pre-filter information for agents. These are programmatic
decisions the orchestrator *should* own. This is the correct model.

---

### Stage 2 — Codebase collection (`internal/codebase/codebase.go`)

**Verdict: severe violation.**

#### What is pre-collected
The codebase module shallow-clones the repository, runs a fixed set of ecosystem-specific grep
patterns against a fixed set of file extensions (`.js .ts .go .py .rs .jsx .tsx .mjs`), and
caps results at **50 snippets**. These are serialised into `models.CodebaseUsage` and injected
into the analyser's prompt as static text.

#### What the agent actually needs
To accurately assess whether a breaking change affects the codebase, the agent may need to:
- Search for the *specific* renamed/removed/changed symbol names (not the package name)
- Read the full file contents of files that import the package
- Follow transitive usages (wrapper modules, re-exports)
- Find usage patterns that don't match the hardcoded import patterns (dynamic requires,
  aliased imports, JSX spread usage, CSS class references from component packages)
- Search in file types not in the fixed include list (`.vue`, `.svelte`, `.mts`, `.cts`,
  `.mdx`, `.html`)
- Retrieve surrounding context lines to understand *how* an API is called, not just *that* it is

#### The forced-guess this causes (concrete)
If `mdi-react` renames `ClockIcon` to `ClockOutlineIcon`, the agent receives import lines from
the 50 snippets that mention `mdi-react`. It cannot ask "does any file use `ClockIcon`
specifically?" The specific rename may be in snippet #73 — invisible. The agent must estimate
"some icons probably changed; this codebase uses icons; mine might be affected." This is
exactly what happened.

**Additional structural problem:** The codebase search runs *before* the agent sees upstream
data. An empowered agent would first read the changelog to identify *which specific symbols
changed*, then search for those symbols by name — not search by package name and then try to
cross-reference against symbols it learns about later.

#### Mitigation
Give the agent Bash + Read tools and a checked-out repo on disk (already done for the
implementation agent). Let it run its own targeted searches after reading the upstream data.
The shallow-clone infrastructure in `codebase.go` can remain as the mechanism that puts the
repo on disk, but the grep step is removed — the agent queries the repo itself.

---

### Stage 3 — Upstream info fetching (`internal/github/client.go: GetUpstreamInfo`)

**Verdict: severe violation.**

#### What is pre-collected
`GetUpstreamInfo` fetches GitHub Releases (truncated at **5,000 chars per body**) and
CHANGELOG.md (truncated at **10,000 chars**), with a further cross-truncation of all releases
combined at **20,000 chars**. Upstream repo discovery is fragile: Go modules derive from the
import path, npm packages look up the registry for a `repository` field, everything else falls
back to a GitHub name search. Failure is silent — the analyser receives "No additional release
information available."

#### What the agent actually needs
- The actual migration guide for the version range (many packages have separate `MIGRATION.md`,
  upgrade guides, or wiki pages not fetched by this code)
- The git diff between the two versions for the *specific APIs the codebase uses* (the PR body
  often contains a compare URL; the agent cannot follow it)
- TypeScript definition files for API signature changes
- Issue/discussion threads linked from release notes ("see #123 for details")
- README sections describing changed API behaviour

#### The forced-guess this causes (concrete)
A package with a 10,001-character CHANGELOG.md will have the critical migration section
truncated. The analyser sees `[... truncated]` and must guess. Worse: the current analyser
*prompt* says "If release notes are truncated or missing, the compare URL lets you see the full
code delta — review it if the scope is manageable." This instruction is a **dead letter** — the
analyser has no tool to follow that URL. The prompt acknowledges the gap and provides no means
to close it.

For packages not discoverable via the fragile lookup chain, the agent receives nothing and must
work from the PR body alone.

#### Mitigation
Give the agent WebFetch tools. Let it open the compare URL from the PR body, fetch the
migration guide, read the full changelog. The orchestrator's pre-fetched release data can serve
as a **performance hint** ("here is what we found quickly; go deeper if you need more") but
must not be the ceiling. Remove the truncation-instruction dead letter from the prompt.

---

### Stage 4 — Analyser agent (`internal/analyser/analyser.go`)

**Verdict: structural violation — the tool-less design makes Stages 2 and 3 unrecoverable.**

The analyser is a single `Messages.New` call with **zero tools**. It receives a static prompt
and must produce a verdict purely from the injected text. Every limitation in Stages 2 and 3 is
final: if a snippet was beyond the 50-cap, the analyser cannot search for it. If the changelog
was truncated, it cannot fetch the rest. If the upstream repo was undiscoverable, it cannot try
another strategy.

Note that **Phase 3.2 (WORKPLAN)** already plans to remove the separate analyser and merge it
into the single implementation agent. Phase 6 of the WORKPLAN builds on that decision: the
combined agent must be designed with tools from the outset — it should not simply inherit the
analyser's tool-less architecture.

#### Mitigation
The analyser should run as a `claude` process (like the implementation worker) with at minimum
WebFetch, Bash, and Read tools, in a checked-out repo. The orchestrator passes the PR metadata
as its brief and the agent gathers what it needs. See Phase 6 in WORKPLAN.

---

### Stage 5 — Implementation agent (`internal/implementation/implementation.go`)

**Verdict: well-designed. No violation.**

The implementation worker runs as `claude --print --dangerously-skip-permissions` with its
working directory set to the cloned and branched repository. It has unrestricted tool access
(Bash, Read, Edit, Write, `gh` CLI) and can search the codebase, read any file, and run tests.
It receives the PR metadata and the analyser's `ReviewBody`/`CodeChanges` as its brief and can
verify or extend that information autonomously.

**Note:** The implementation agent inherits any errors in the analyser's output (Stage 4). If
the analyser's assessment was based on truncated/incomplete data, the implementation agent starts
from a flawed brief. Within its own stage the design is correct; the upstream stages must be
fixed to prevent this.

**This is the reference implementation of the empowered-agent pattern.** The analyser and
reviewer should be redesigned to match it.

---

### Stage 6 — Reviewer agent (`internal/reviewer/reviewer.go`)

**Verdict: moderate violation — tool-less design with a prompt-level epistemic-hedging patch.**

The reviewer is a single `Messages.New` call with **zero tools**. It receives the original
assessment, the suggested code changes, the implementation diff (truncated at **50,000 chars**),
and commit messages.

The diff truncation is acknowledged in the reviewer's own prompt: *"Do NOT infer the absence of
any change (deleted tests, workarounds) from this cut-off view; if the visible portion is
insufficient to judge, say so in your concerns."*

This is precisely the epistemic-hedging patch the Agent Empowerment Principle identifies as the
wrong fix: it turns false confidence into unnecessary human escalation for things the agent
could have resolved autonomously (by running `git diff` itself in the repository).

The violation is moderate rather than severe because the reviewer's scope is naturally narrower
(did the implementation match the assessment?) and the 50k cap covers most real diffs. But
the pattern is wrong regardless of severity.

#### Mitigation
Run the reviewer as a `claude` process with `proc.Dir = repoDir`, like the implementation
agent. It can run `git diff bumpTip..HEAD` itself (no size cap), read specific test files to
verify they weren't weakened, and confirm coverage directly rather than hedging.

---

## Cross-cutting: repo checkout, directory lifecycle, and isolation

### Current state (before Phase 6)
The pipeline currently does multiple redundant repo operations:
1. `codebase.go` shallow-clones the repo for pre-collection grepping (separate, short-lived,
   cleaned up immediately)
2. `implementation.go` creates `os.MkdirTemp("", "sweeper-impl-*")` per PR for the agent's
   full clone; `defer p.cleanup()` removes it when `Pipeline.Run()` returns — so per-PR
   workdir cleanup works correctly today
3. `manualRebase()` creates a separate `os.MkdirTemp("", "sweeper-rebase-*")` with its own
   `defer os.RemoveAll` — also cleaned up correctly

**Known gap in the current code:** Agent log files go to `os.TempDir()/sweeper-agent-logs/`
(hardcoded at `implementation.go:959`). This path is shared across all concurrent PRs and is
never cleaned up. On a long-running deployment, agent log files accumulate indefinitely.

After the Phase 6 redesign, the combined analyse+implement agent needs the repo for both
analysis AND implementation. A separate reviewer step also needs the same repo state.

### Principles for repo sharing

**Sequential agents on the same PR must share a checkout, not re-clone.**
The analyse→implement→review pipeline for a single PR is sequential. These agents should
operate on the same repo directory, not clone three times. The `implementation.go` clone is
already the right place to anchor this — the repo should be cloned once and passed to each
subsequent stage.

**Concurrent PR processing must use git worktrees.**
The orchestrator processes multiple PRs concurrently (configurable `--concurrency`). Each
PR must get its own worktree (`git worktree add`) from a shared base clone, not a separate
full clone per PR. This avoids redundant network I/O and disk use while keeping PR state
isolated.

**Shared state must be made explicit in every agent's brief.**
If two sequential agents on the same PR share a repo directory, each must be told:
- What state the repo is in when it starts (e.g. "the repo is on branch `X`, the previous
  agent has made commits `Y` and `Z`; the working tree is clean")
- What changes it is expected to leave (e.g. "leave the repo with the fix committed and
  CI passing; do not squash or rebase")
- Whether it should clean up after itself or leave state for the next agent
- Which previous agent may have left local changes and whether those are intentional

An agent that finds unexpected local changes without this context has no way to reason about
them correctly — it may wrongly discard work, wrongly preserve noise, or be confused by state
it didn't create.

**Prompt context for state handoff (required fields):**
When handing off between sequential agents sharing a repo, the brief must include:
```
Repo state at handoff:
  Branch:    <name>
  HEAD:      <sha> ("<commit message>")
  Worktree:  clean | <N uncommitted files — <reason>>
  Left by:   <prior agent name/role>
  Expected:  <what this agent should find and what it must leave>
```

---

## Verification checklist (for post-implementation review)

Use this checklist when reviewing Phase 6 implementation to confirm all findings were closed:

### Stage 2 (codebase collection)
- [ ] The 50-snippet cap in `codebase.go` is removed or the snippet list is explicitly a hint,
  not the ceiling of what the agent can see
- [ ] The agent can run `grep`/`find`/`rg` in the repo for specific symbol names
- [ ] The codebase search is no longer performed before the agent reads upstream data
- [ ] The agent's searches are driven by what it learns from the upstream data, not by a
  fixed set of import-pattern greps

### Stage 3 (upstream info)
- [ ] The agent has WebFetch (or equivalent) tools to follow compare URLs and migration guides
- [ ] The prompt no longer contains instructions the agent structurally cannot follow
  (e.g. "follow the compare URL if the changelog is truncated")
- [ ] Truncation limits in `GetUpstreamInfo` are either removed or explicitly framed as hints
- [ ] The agent can discover and fetch upstream information for packages whose repos are not
  directly discoverable via the current fragile lookup chain

### Stage 4 (analyser)
- [ ] The analyser no longer exists as a tool-less `Messages.New` call
- [ ] The combined agent (post-Phase 3.2) has at minimum Bash, Read, and WebFetch tools
- [ ] The combined agent runs in a checked-out repo directory

### Stage 6 (reviewer)
- [ ] The reviewer is no longer a tool-less `Messages.New` call
- [ ] The reviewer runs in the repo directory and can run `git diff` itself
- [ ] The epistemic-hedging instruction about the 50k diff cap is removed from the prompt

### Repo sharing, directory lifecycle, and isolation
- [ ] Sequential agents on the same PR share one repo checkout; no redundant clones
- [ ] Concurrent PRs use git worktrees from a shared base clone (`git worktree add`)
- [ ] Shared base clone is repo-scoped (path includes owner+repo); re-fetched each scan cycle;
  cleaned up on process shutdown
- [ ] Per-PR worktree paths are repo-scoped AND PR-scoped (e.g. `sweeper-wt/<owner>-<repo>/pr-<N>/`);
  no two PRs — including PRs from different repos with the same number — can resolve to the
  same path
- [ ] No accidental directory sharing between agents that should be isolated: every directory
  path has a documented answer to "which agents may access this?"; paths intended to be isolated
  are guaranteed unique by construction, not by probabilistic random suffixes
- [ ] Stale path detection: if a per-PR worktree path already exists at run start (crash
  residue), it is removed and recreated — never silently reused as potentially dirty state
- [ ] Agent log files are scoped under the per-PR workdir, not under a shared
  `os.TempDir()/sweeper-agent-logs/`; they are removed by `Pipeline.cleanup()`
- [ ] The staleness gate (`FindNewerPRForPackage` in Step 1) is documented as the primary guard
  against same-package parallel processing; worktree isolation is the backstop, not the
  primary defence; a comment or test pins this ordering assumption
- [ ] Each agent's brief includes explicit repo state at handoff (branch, HEAD, cleanliness,
  who left the current state, what is expected)

### Regression test
- [ ] The mdi-react 6.7.0→9.4.0 bump (upstream PR taskcluster/taskcluster#6753) is processed
  after the redesign is deployed
- [ ] The output does NOT contain "unlikely" or "probably" regarding icon name renames
- [ ] The output shows the agent independently verified which icons were renamed in the
  6.7.0→9.4.0 range and whether any renamed icon is used in the taskcluster codebase
- [ ] The recommendation is grounded in verified facts, not estimates
