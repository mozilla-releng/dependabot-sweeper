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
Once 6.D is in place, the Go program prepares a full clone for the agent from the bare clone;
the `codebase.go` shallow-clone infrastructure is no longer needed and should be removed entirely
— not just the grep step, but the whole module.

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

### Design model after Phase 6

**The Go program owns all infrastructure. Agents own only their designated working directory.**
What can be done programmatically must be done programmatically — code does not forget or
disobey instructions. Agents are not asked to manage directories, clean up globally, or
maintain git infrastructure.

**Bare clone (permanent, managed by the Go program).**
The Go program maintains `sweeper-base/<owner>-<repo>.git`, a bare clone containing the full
repo history, all branches, and all tags. It is re-fetched at the start of each scan cycle,
before any PR goroutines are launched. Agents are told its path and told explicitly not to
modify it. It is never per-PR — it exists for the lifetime of the process.

**Per-PR working directory and clone (created and destroyed by the Go program).**
For each PR, the Go program:
1. Creates `sweeper-data/pr/<owner>-<repo>/pr-<N>/` as the agent's working directory.
2. Prepares a **full git clone** (not shallow) from the bare clone at `<workdir>/repo/`.
3. Passes both paths to the agent in its brief, along with an explicit statement of tool
   access and autonomy, and the expected lifetime of the directory.
4. Deletes `<workdir>` when the PR disappears from the open-PR list.

Standard brief template:
```
Working directory: <workdir>
Repo clone:        <workdir>/repo/   [full clone, ready to use]
Bare clone:        <bare-path>       [do not modify; clone from it if you need a fresh copy]

You have full tool access and are fully autonomous. Work within <workdir> where possible.
If you must do anything outside it, clean up after yourself. This working directory will
remain on disk while [the relevant PR] is open.
```

**Each agent starts from a fresh clone.** Agents do not share each other's working state
except in the deliberate impl→reviewer handoff below. This avoids pollution from prior agent
state; cloning from a local bare clone is cheap.

**Impl → reviewer handoff (the clone is the interface).**
The one exception: the reviewer is handed the **same `<workdir>`** as the implementing agent.
The commit history in `<workdir>/repo/` is the handoff artifact — the reviewer examines what
was committed, not a metadata summary. The implementing agent is told:
```
Your commits will be reviewed — write clean, self-explanatory commit messages. If you
incorporate review feedback across multiple turns, clean up the commit history (squash or
fixup as needed) before re-submitting. The reviewer sees exactly the commits in this repo.
```
The reviewer's brief includes the working directory path, branch, HEAD SHA, and the turn
number (1 = first review, 2+ = reviewing a revision). The reviewer works in `<workdir>/repo/`
directly — no re-clone needed.

---

## Verification checklist (for post-implementation review)

Use this checklist when reviewing Phase 6 implementation to confirm all findings were closed:

### Stage 2 (codebase collection)
- [ ] The 50-snippet cap in `codebase.go` is removed AND no pre-filtered snippet list is
  provided as a primary source; pre-fetched data, if any, is framed as a performance hint
  only and the agent is explicitly told it can search beyond it
- [ ] The agent can run `grep`/`find`/`rg` in the repo for specific symbol names
- [ ] The codebase search is no longer performed before the agent reads upstream data
- [ ] The agent's searches are driven by what it learns from the upstream data, not by a
  fixed set of import-pattern greps
- [ ] The `codebase.AnalyseCodebaseUsage` call in `orchestrator.go:471` is removed

### Stage 3 (upstream info)
- [ ] The combined agent is invoked with `--dangerously-skip-permissions` — no tool enumeration
  or allowlist; the agent uses whatever it needs (verifiable by inspecting the
  `workerCommand`-equivalent function)
- [ ] The dead-letter "follow the compare URL if the changelog is truncated" instruction in
  `analyser.go:46–47` is replaced with a real WebFetch tool access instruction
- [ ] The agent brief contains an explicit hint-framing clause ("this data was pre-fetched; use
  WebFetch if it is insufficient")
- [ ] Truncation limits in `GetUpstreamInfo` are either removed or explicitly framed as hints
- [ ] The agent can discover and fetch upstream information for packages whose repos are not
  directly discoverable via the current fragile lookup chain

### Stage 4 (analyser)
- [ ] The analyser no longer exists as a tool-less `Messages.New` call
- [ ] The combined agent (post-Phase 3.2) runs with `--dangerously-skip-permissions` in a
  checked-out repo directory — fully autonomous, no tool restrictions

### Stage 6 (reviewer)
- [ ] The reviewer runs as a `claude` subprocess with `--dangerously-skip-permissions` and
  `proc.Dir` set to the repo directory, matching the `runWorkerTurn` pattern — not a
  tool-less `Messages.New` call
- [ ] The epistemic-hedging instruction at `reviewer.go:187–194` ("Do NOT infer the absence of
  any change from this cut-off view") is removed from the prompt

### Repo sharing, directory lifecycle, and isolation
- [ ] The Go program maintains a bare clone (`sweeper-base/<owner>-<repo>.git`); re-fetched
  (including tags) at the start of each scan cycle, **before** any PR goroutines launch; if
  `git fetch` fails, the bare clone is deleted and re-cloned from scratch
- [ ] For each PR, the Go program creates `sweeper-data/pr/<owner>-<repo>/pr-<N>/` and runs
  `git clone <bare-path> repo` inside it — a full (non-shallow) clone handed to the agent
- [ ] Each agent's brief includes: working directory path, repo clone path, bare clone path
  (with explicit "do not modify"), a statement of full tool access and autonomy, and a note
  on when the directory will be cleaned up
- [ ] Stale directory detection: if `<workdir>` already exists at run start (crash residue),
  the Go program deletes it and recreates it — never silently reuses dirty state; no branch
  ref complications since agents work in their own clones and do not write to the bare clone
- [ ] Per-PR working directory paths are repo-scoped AND PR-scoped
  (`sweeper-data/pr/<owner>-<repo>/pr-<N>/`); no two PRs from any repos resolve to the same
  path; guaranteed by construction, not by random suffixes
- [ ] Agent log files are scoped under `<workdir>/`, not under a shared
  `os.TempDir()/sweeper-agent-logs/`; they are removed when `<workdir>` is deleted on PR close
- [ ] ALL per-PR resources live under one PR-keyed root; nothing hidden outside it:
  - Repo clone lives under `<workdir>/repo/`
  - Log files live under `<workdir>/`
  - Claude CLI session files either redirected into `<workdir>` or explicitly deleted by the
    closed-PR sweep (mechanism confirmed feasible — see D1 investigation item in WORKPLAN)
  - SQLite DB rows pruned by `Reap()` triggered from the closed-PR sweep
- [ ] Invariant: deleting `sweeper-data/pr/<owner>-<repo>/pr-<N>/` plus calling `Reap(N)` leaves
  zero resources associated with PR N on the host
- [ ] On each orchestrator scan cycle, after fetching the open-PR list, any working directory
  whose PR is absent from the list is deleted and `Reap()` called for that PR number
- [ ] No time-based expiry or manual sweep is needed; the open-PR scan is the sole cleanup signal
- [ ] The staleness gate (`FindNewerPRForPackage` in Step 1) is documented as the primary guard
  against same-package parallel processing; per-PR directory isolation is the backstop, not
  the primary defence; a comment or test pins this ordering assumption
- [ ] Impl → reviewer handoff: the reviewer is handed the same `<workdir>` as the implementing
  agent; brief includes working directory path, branch, HEAD SHA, and turn number (1 = first
  review, 2+ = revision); reviewer works in `<workdir>/repo/` directly without re-cloning
- [ ] Implementing agent's brief states that commits will be reviewed and that it is responsible
  for clean commit history across review-fix turns

### Regression test
- [ ] The mdi-react 6.7.0→9.4.0 bump (upstream PR taskcluster/taskcluster#6753) is processed
  after the redesign is deployed; if it is no longer reproducible in the fork, create a
  synthetic mdi-react bump PR at the same version range as the test vehicle
- [ ] In the agent log (`pr-<N>-agent.jsonl`), a `tool_use` event of type `WebFetch` appears
  with a URL pointing to the MDI changelog, GitHub releases, or npm registry for mdi-react
  in the 6.7.0→9.4.0 range (not just the package homepage)
- [ ] In the agent log, a Bash `tool_use` event appears with a command searching for specific
  icon names (e.g. `grep -r 'ClockIcon'`), not just the package name
- [ ] The output does NOT contain "unlikely" or "probably" regarding icon name renames
- [ ] The comment cites a specific source (URL or release notes section) for the icon rename
  information
- [ ] If the agent concludes no renamed icons are used, the conclusion is supported by a
  specific search result (e.g. "grep found no matches for X"), not by absence of evidence in
  a pre-filtered list
- [ ] The recommendation is grounded in verified facts, not estimates
