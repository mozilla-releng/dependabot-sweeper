# Phase 6 Plan Review — Findings

**Date:** 2026-06-16  
**Reviewer:** Independent subagent  
**Documents reviewed:** `docs/AGENT_PIPELINE_AUDIT.md`, `docs/WORKPLAN.md` (Phase 6),
`internal/analyser/analyser.go`, `internal/codebase/codebase.go`,
`internal/reviewer/reviewer.go`, `internal/implementation/implementation.go`,
`internal/orchestrator/orchestrator.go`

**Purpose:** These findings must be folded into `docs/WORKPLAN.md` (Phase 6) and
`docs/AGENT_PIPELINE_AUDIT.md` before any implementation work begins. See the prompt
at the bottom of this file for the next session.

---

## 1. Completeness — gaps between audit findings and Phase 6

**A1 — Combined agent's initial brief is unspecified.**
Phase 6.A removes the tool-less analyser but doesn't specify what the combined agent's brief
*does* contain. `BuildImplementationBrief` (implementation.go:181–207) currently forwards
`analysis.ReviewBody` and `analysis.CodeChanges`. After Phase 3.2 there is no upstream
analyser verdict. The plan needs to specify what the combined agent receives as its starting
context (PR metadata, diff, the pre-fetched hints) and what it is expected to produce.

**A2 — The `recommend` path (approve-with-comment) is not addressed.**
The public comment posted on the approve path is generated from `analysis.ReviewBody`
(orchestrator.go:670) — produced by the tool-less analyser under exactly the conditions
the audit criticises. Phase 6 addresses the tool-less design but doesn't explicitly state
how the combined agent authors the public comment. The comment is the product; its quality
is the whole point of the principles.

**A3 — The `confidence` field and low-confidence routing are unaddressed.**
`actOnAnalysis` gates on `analysis.Confidence == ConfidenceLow`. With the combined agent
having tools, low confidence should be rare. The plan doesn't say whether the `confidence`
field survives, whether the combined agent still emits it, or how the routing changes.

**A4 — `manualRebase` creates its own temp dir before `p.workdir` exists.**
`manualRebase` runs before `os.MkdirTemp` for `p.workdir` (implementation.go:415–433).
Phase 6.D's "all resources under one PR-keyed root" requires the PR-keyed root to exist
before the rebase runs, but the sequencing currently prevents this. The plan must address
this ordering dependency.

**A5 — Log path change requires web dashboard update.**
Moving logs from `sweeper-agent-logs/pr-<N>-agent.jsonl` to a PR-keyed root requires
updating the web API that serves the agent-log endpoint (`--log-dir` / `SWEEPER_LOG_DIR`,
implementation.go:316). The plan doesn't mention this dependency.

---

## 2. Internal consistency

**B1 — 6.C (reviewer empowerment) is independent of Phase 3.2 — ship it earlier.**
The reviewer (`internal/reviewer/reviewer.go`) is entirely independent of the
analyser-removal cluster. It can be redesigned as a `claude` subprocess now. Similarly,
the log-scoping fix (move logs under the per-PR workdir) is a trivial independent change.
The plan should call these out as "can ship before Phase 3.2."

**B2 — "Evaluate whether the existing per-PR model already does worktrees" — don't evaluate, it's known.**
The code already answers this: `cloneAndBranch` does a full `git clone` per PR every time
(implementation.go:879). The plan should state this as a fact, not leave the implementer
to rediscover it.

**B3 — Inconsistent path naming: `sweeper-wt/` vs `sweeper-data/pr/`.**
6.D bullet 4 uses `sweeper-wt/<owner>-<repo>/pr-<N>/`; the "all resources under one root"
bullet and the verification checklist use `sweeper-data/pr/<owner>-<repo>/pr-<N>/`. These
are different paths. One canonical path schema must be chosen and used uniformly throughout
both WORKPLAN.md and AGENT_PIPELINE_AUDIT.md.

**B4 — Base-clone `git fetch` races with concurrent goroutines.**
The plan says the shared base clone is "re-fetched at the start of each scan cycle."
`orchestrator.go:244–257` dispatches PRs concurrently. A `git fetch` updating refs in the
shared base while concurrent worktrees are running a `git rebase` against `origin/main` is
a race condition. The plan must specify that the base-clone fetch completes before any PR
goroutines are launched for that cycle.

---

## 3. Missing prerequisites

**C1 — `codebase.AnalyseCodebaseUsage` call in the orchestrator is not addressed.**
After Phase 3.2, the combined agent owns the clone. The orchestrator currently calls
`codebase.AnalyseCodebaseUsage` at `processPR` (orchestrator.go:471) before any clone
exists. 6.B says "remove the grep step" but doesn't say "remove the
`codebase.AnalyseCodebaseUsage` call in `processPR`." The call site must be removed as
part of 6.B.

**C2 — 6.E lists prompt review as a general requirement but doesn't enumerate the specific changes.**
The dead-letter "follow the compare URL if the changelog is truncated" instruction is at
`analyser.go:46–47`. The reviewer's epistemic-hedging patch "do NOT infer the absence of
any change from this cut-off view" is at `reviewer.go:187–194`. These should be named
specifically in 6.E rather than left as a generic "review all prompts."

---

## 4. Resource lifecycle correctness

**D1 — Claude CLI session file deletion is unconfirmed feasible.**
The plan offers two options: redirect session storage into the PR-keyed root, or delete
by sessionID. Neither is confirmed workable:
- The Claude CLI stores sessions under `~/.claude/projects/<hash>/` where `<hash>` is a
  hash of the project context, not the sessionID. The mapping from sessionID to file path
  is not documented.
- Whether a CLI flag redirects session storage to an arbitrary path is unverified.
The plan must acknowledge that the deletion mechanism needs to be confirmed against the
Claude CLI's actual storage format before either option is committed to. Add an explicit
investigation item.

**D2 — Remote git branches created by the sweeper are not in the resource inventory.**
The sweeper pushes `auto/fix/<package>-<version>` branches to the remote repo. These are
not deleted by the PR-closed sweep. The plan should note whether remote branch cleanup is
in scope (and if not, why not).

**D3 — GaveUp PRs stay open on GitHub — the cleanup sweep never fires for them.**
The sweep deletes resources when a PR disappears from the open-PR list. But `GaveUp` does
not close the original PR — it posts a comment and leaves it open. A GaveUp PR's resources
are only cleaned up when the PR is manually closed or merged. The plan's "one trigger,
complete cleanup" framing is misleading for this case — make it explicit.

---

## 5. Repo sharing and isolation

**E1 — Stale branch ref not addressed alongside stale directory.**
The plan handles "stale directory from crashed run" but not "stale branch ref from crashed
run." `git worktree add` fails if the branch already exists in the repo. Recovery requires
`git branch -D <branch>` before re-creating the worktree. Add this to the stale-path
detection item.

**E2 — Base clone corruption has no recovery path.**
If the shared base clone's `.git` is corrupted (interrupted fetch, disk full, etc.), all
worktree creation fails. The plan needs a fallback: if `git fetch` fails, delete the base
clone and do a full re-clone.

**E3 — Multi-turn handoff brief needs turn-number context.**
The state handoff template includes "Left by: <prior agent role>." For re-invocations (a
second reviewer run after a review-fix turn), the brief should include the turn number so
the reviewer understands this is a revised submission, not a fresh one.

---

## 6. Verification checklist quality

**F1 — "50-snippet cap removed OR snippet list is a hint" is too permissive.**
The "or hint" alternative can be satisfied by relabelling. Change to: "The 50-snippet cap
is removed AND no pre-filtered snippet list is provided as a primary source; pre-fetched
data is framed as a performance hint only."

**F2 — "Agent has WebFetch (or equivalent) tools" is too vague.**
Change to: "The combined agent is invoked with a tool allowlist that explicitly includes
WebFetch, Bash, and Read — verifiable by inspecting the `workerCommand`-equivalent
function."

**F3 — "Reviewer is no longer a Messages.New call" leaves an unspecified positive state.**
Change to: "The reviewer runs as a `claude` subprocess with `proc.Dir` set to the repo
directory and at least Bash and Read in its tool allowlist, matching the
`runWorkerTurn` pattern."

**F5 — Stale path checklist item must include stale branch ref.**
Add: "AND any stale branch ref for the worktree branch is deleted before re-creating the
worktree."

---

## 7. Regression test specification

**G1 — Pass criteria are not verifiable from the PR comment — require the agent log.**
"The agent fetches the upstream MDI icon rename list" is only verifiable from
`pr-<N>-agent.jsonl` JSONL tool-use events, not from the PR comment text. Change to:
"In the agent log, a `tool_use` event of type WebFetch appears with a URL to the MDI
changelog, GitHub releases, or npm registry for mdi-react in the 6.7.0–9.4.0 range"
and "A Bash `tool_use` event appears searching for specific icon names (e.g.
`grep -r 'ClockIcon'`), not just the package name."

**G2 — Negative conclusion must cite a search result, not an absence from snippets.**
Add: "If the agent concludes no renamed icons are used, that conclusion is supported by
a specific search result (e.g. 'grep found no matches for X'), not by absence of evidence
in a pre-filtered list."

**G3 — Add fallback if the upstream PR is no longer reproducible.**
Note: "If `taskcluster/taskcluster#6753` is no longer reproducible in the fork at Phase 6
completion time, create a synthetic mdi-react bump PR at the same version range."

---

## 8. Additional gaps

**H1 — No rollback specification for Phase 6.**
Phase 3.2 has a Q10 rollback flag. Phase 6 makes large structural changes but has no
defined rollback path. Add: "Rollback: revert to the Phase 3.2 commit as the last
known-good state before Phase 6."

**H2 — The "hint, not the ceiling" framing requires a specific prompt clause.**
6.A specifies behaviour but 6.E only says "review all prompts." The specific clause needed
is: "The following release data was pre-fetched as a performance shortcut; if it is
insufficient or truncated, use your WebFetch tool to retrieve more." This must appear as a
concrete implementation item, not just an audit principle.

**H5 — `--bare` flag may suppress WebFetch tools.**
The worker command currently includes `--bare` (implementation.go:1057). `--bare` may
suppress WebFetch depending on how it is provided. Confirm that `--bare` + WebFetch works
in the Claude CLI, or remove `--bare` from the combined agent invocation. This must be
resolved before 6.A is implementable.

---

## Summary by priority

**Must resolve before implementation:**
- B3: choose one canonical path schema (sweeper-wt vs sweeper-data/pr)
- B4: base-clone fetch must complete before goroutines launch
- H5: confirm `--bare` + WebFetch is a working combination
- D1: investigate Claude CLI session file storage format

**High value — address before implementation:**
- A1: specify the combined agent's initial brief
- E1: add stale branch ref to stale-path recovery
- E2: add base clone corruption recovery path
- G1: rewrite regression test criteria to use agent log, not PR comment

**Quick wins — independent of Phase 3.2, ship now:**
- B1: 6.C (reviewer empowerment) and log-scoping can ship before Phase 3.2

**Clarifications and tightening:**
- A2, A3, A4, A5, C1, C2, D2, D3, E3, F1, F2, F3, F5, G2, G3, H1, H2
