# Dependabot Sweeper — Algorithm Design

This document describes the current decision algorithm, from first principles through to the
implementation pipeline. It is written to be picked apart: the goal is to identify where the
current design is wrong or incoherent, and derive a revised "what it should be" version that
can drive concrete code fixes.

---

## North Star (the driving UX principle)

**This is the high-level UX principle that drives the entire architecture. Everything else
serves it.** For each engaged dependabot PR (any bump type, once CI has settled), the tool
produces exactly one of two outcomes:

1. **No changes needed → comment on the *existing* dependabot PR**, explaining *why* it is OK to
   merge. Required checks must be passing **and** the agent must have determined that *absolutely
   no code change is required*. The explanation is concrete and specific to what actually changed
   upstream, e.g.:
   - "The upstream changes are to features this codebase doesn't call."
   - "We were already using the new calling convention; the deprecated one that was dropped was
     no longer in use here."
   - "The licence changed, but it remains compatible with ours and we already meet its terms."
   It is a *comment* — the tool proposes, a human merges; never a native APPROVE.

2. **Changes needed → open the tool's own replacement PR, based on the dependabot one.** It
   incorporates the same version bump and adds the necessary changes — structured as **one or more
   additional commits** on top of the bump — with a justification of *why those changes are
   correct / needed / appropriate* and *how this is the right solution for this version bump*,
   grounded in the upstream docs / changelogs / diffs and other context the agent gathered.
   Required checks must be green before it is opened; if the agent can't get there, the PR stays a
   silent draft (no noise). ("Based on" means it supersedes the dependabot PR and contains the same
   bump — *not* that it must reuse dependabot's literal commit; see Q11.)

The whole pipeline exists to deliver one of these two well-justified outcomes — or, only as a
genuine last resort, a concise flag for human attention. The justification is not decoration: it
is the product. The human's job is to read a short, specific rationale and merge with confidence,
not to re-derive the analysis.

(Canonical home for this principle is `docs/PRINCIPLES.md`; it is restated here because it drives
every decision in this document.)

---

## Purpose

Dependabot opens PRs to bump dependencies. The tool's job is:

1. **Triage** — for each open dependabot PR, decide what kind of attention it needs.
2. **Fix** — for PRs that need code changes, make those changes autonomously and open a
   replacement PR with CI green, ready for a human to review and merge.
3. **Recommend** — for PRs that need no code changes (the bump is safe as-is), post a
   brief justification so a human can merge with confidence.
4. **Flag** — for PRs the tool cannot handle with confidence, post a one-line explanation
   and leave it for a human.

Output is always a **proposal**. The tool never submits a native GitHub APPROVE. A human
decides whether to merge.

---

## Non-negotiable constraints

- **Idempotent — and this is a COST-SAFETY invariant, not just UX.** The tool runs on a cron
  cycle. Re-running the same cycle must produce no duplicate comments, reviews, or actions; "no
  change since last cycle" must be a no-op. Critically, under Q10 every engaged PR triggers an
  *expensive agentic step*, so the **only** thing bounding spend is "never process the same PR
  twice." A state-machine bug that re-admits an already-processed PR is therefore a *runaway-cost
  incident*, not a cosmetic glitch. This is the primary reason the state-transition laws must be
  fully trustworthy (Q8): a PR determined to have been through the pipeline must never re-enter
  it. The same invariant underpins C1 (never re-ingest our own PRs), M1 (a silent failed draft
  must still record a terminal outcome), and the already-processed-SHA skip.
  - **Skip-key SHA drift (review N4).** The skip compares `stored.HeadSHA == pr.HeadSHA`, but
    Phase-0 rebase changes the branch head — so a PR the tool rebased shows a *new* head next scan,
    the skip misses, and it re-enters the tier-3 agent (bounded to one extra run; bites only the
    gave_up/silent-draft path, since a finalized PR's original is closed and not re-scanned). Fix:
    record that path's terminal outcome against the **post-rebase tip SHA**, not the scan-time
    `pr.HeadSHA`.
    - **Plumbing (review MAJOR-1):** the captured tip SHA (M4) lives *inside* the pipeline, but
      the `gave_up` `recordOutcome` call is in the *orchestrator*, and `RunResult` has no SHA
      field. So this fix has a specific wiring step: **add the captured tip SHA to `RunResult`,
      and have the orchestrator's gave_up path record *that*.** Leave every other `recordOutcome`
      call on `pr.HeadSHA` — they are pre-rebase (analysis-stage flags) or finalize (original
      closed, never re-scanned), so the scan-time SHA is correct there. Applying the post-rebase
      SHA everywhere would be wrong.
- **The PR is the only UI for the repo's maintainers.** For the people whose PRs are being
  acted on, the tool communicates *only* through the PR — no email, no Slack, no notification
  outside the PR itself, and nothing duplicated on re-runs. This is about not creating
  notification noise for the humans being served.
  - This is distinct from the **admin UI**: the tool's *operators* have a web dashboard (the
    embedded SPA) showing live pipeline state, per-PR CI/analysis, agent logs, billing, and
    control actions (reset, cancel). That is a separate observability + control surface for
    whoever runs the sweeper, not a channel to the repo's maintainers. See `docs/DASHBOARD.md`.
- **Provider-agnostic CI.** The tool reads only the generic GitHub Checks API — no
  Taskcluster, no GHA-specific APIs.
- **Propose, never approve.** The bot posts comments. It never submits a native GitHub
  APPROVE review.

---

## What gets processed

Each cycle, the tool lists **all open PRs** on the scanned repo, then keeps only those whose
**author** is an accepted author:
- `dependabot[bot]` and `renovate[bot]` (always accepted), plus
- any logins passed via `--accept-author` (e.g. `petemoore` on the test bed fork).

PRs by any other author are ignored entirely. There is no up-front filter by bump type: a PR
that survives the author filter enters the pipeline **regardless of patch/minor/major** — the
staleness, CI-settled, and already-processed checks all happen *inside* the per-PR steps below,
not as a pre-filter.

Titles that don't parse as a known bump format are still processed (with bump type `unknown`)
rather than dropped — in an unattended cron loop, silently losing a PR is worse than analysing
one off its diff.

**Two gates together determine in-scope PRs.** The author filter is the primary gate. A second,
reap-exempt gate (Q14 — see *Two-PR-type lifecycle* below) permanently excludes any PR the tool
itself created: a `created_prs` DB record checked before processing, so the tool never
re-ingests one of its own replacement PRs as a fresh dependabot PR. In-scope is decided purely
by these two criteria; branch name and title are not used as exclusion signals (branch names are
attacker-spoofable). See T12.

## The decision algorithm (current)

> **⚠️ Baseline, not target (review N5).** This section, "CI acceptance model," and the Step 5
> routing table describe the **pre-rework baseline** — the design as the code stands today,
> including the separate analyser and its `approve`/`needs_changes`/`needs_human_review`
> vocabulary. The **target** design is the North Star + the decided questions (esp. Q10's single
> agentic step). Inline "DECIDED (Q#)" notes mark where the target diverges. `spec.go` currently
> encodes this baseline; Phase 3.0 reconciles it to the target.

Each accepted PR is processed independently on every cycle. The steps below are in order.

### Step 1 — Skip stale PRs

If a newer PR exists for the same package (a later version), the current PR is superseded.
The tool closes it with a comment pointing to the newer one.

**How supersession is determined — fully programmatic, no agent, no fuzzy matching**
(`FindNewerPRForPackage`):
- **Package match:** exact string equality on `PackageName`.
- **Version comparison:** both `NewVersion`s are parsed into a `[major, minor, patch]` semver
  tuple and compared numerically. PR A supersedes PR B iff A's version is *strictly higher*.
- **Tie-break:** if several candidates are higher, the highest version wins.
- **Comparison is by version, not PR number** — deliberately. A real case was observed where
  an older PR number carried a newer version (5.2.4 opened before 5.2.3); closing the
  higher-version PR as "stale" would be wrong.
- **Conservative fallback:** if the current PR's `NewVersion` doesn't parse as semver, nothing
  is considered to supersede it (leave it open rather than close it wrongly).

**Gap: grouped PRs.** The current check does not handle the case where an individual-package
PR overlaps with a grouped update PR that includes the same package. Two reasons:
  1. A grouped PR is explicitly exempt from being *closed* as stale (the step guards on
     `!pr.Grouped`).
  2. A grouped PR can never be detected as a *superseder* of an individual PR: for a group,
     `PackageName` holds the group name (not the member package) and `NewVersion` is empty, so
     it can neither string-match an individual package nor produce a comparable version.

  So an individual `lodash 4→5` PR and a `npm group` PR that also bumps lodash to 5 will both
  stay open and both be processed independently — duplicated work, and potentially two
  replacement PRs touching the same dependency. See Q6.

### Step 2 — Wait for CI to settle

The tool will not triage a PR while CI is still running. "Settled" means every check has
reached a terminal state (completed with any conclusion) or has been pending long enough to
be considered stale (default: 12 hours). Checks in the operator `--ignore-check` list are
not waited on.

If CI is not settled, the PR is skipped and will be revisited on the next cycle.

**Which checks count as "CI".** The CI picture is assembled from *two* GitHub sources for the
head commit, and **every** result from both is included:
- **Check runs** (modern Checks API) — GitHub Actions and anything posting check runs.
- **Legacy commit statuses** (combined-status API) — Netlify, pre-commit.ci, and integrations
  (incl. Taskcluster, depending on how it reports) that surface as commit statuses.

So the tool sees *all* checks from *all* providers registered on the commit — not just one
provider, and not the single combined "state" value (that aggregate is computed but used only
for diagnostics; the real gating reasons over the full per-check list).

**The tool does NOT distinguish required from non-required checks.** It never reads branch
protection's required-status-checks list — every check is weighted equally. This has a sharp
consequence (see T5): a check that GitHub would let you merge past (non-required, optional,
informational) is treated by the tool as just as blocking as a required one.

If the check set cannot be fetched (API error), CI is marked `unknown` and the PR is skipped
and revisited — a partial/empty set must never read as "vacuously green," which would risk an
erroneous approve.

### Step 3 — Skip already-processed SHAs

If the tool has already recorded a terminal outcome for this PR at its current head SHA,
nothing has changed — skip with zero LLM calls. The PR will be re-processed automatically
if dependabot pushes a new commit (new head SHA).

### Step 4 — Analyse

The tool calls the **analyser** (Claude API, structured JSON output). The analyser receives a
fixed, pre-assembled prompt — it is **not agentic** and cannot fetch anything itself (see T7).
Every input is gathered by the Go orchestrator ahead of time and hard-capped by byte budget:
- The PR diff (dependency manifest changes only — lockfile + manifest) — capped ~30 KB
- The PR body (dependabot's description, which links to changelogs and release notes) — ~30 KB
- Additional upstream release notes and changelog (fetched separately) — releases capped ~20 KB
- CI status: which checks passed, which failed (names + conclusions)
- CI failure logs: **only** the `output.summary`+`output.text` tail of each failing check —
  fetched at ≤10 KB *per check* (`logs.go`), then the analyser prompt caps the *total* injected at
  50 KB across all checks. (Both caps are mooted by Q10: the single agent fetches logs itself.)
- Codebase usage: a **grep-based** static scan (ecosystem import patterns + a fixed-string name
  match) over a shallow clone, capped at 50 snippets. The clone is deleted as soon as the grep
  finishes — the analyser sees only the grep *results*, never a live repo.

The analyser returns a structured verdict:
- `recommendation`: one of `approve`, `needs_changes`, `needs_human_review`
- `confidence`: `high`, `medium`, or `low`
- `breaking_changes`: list of breaking changes found upstream
- `code_changes`: list of files and descriptions of what to change (when `needs_changes`)
- `review_body`: markdown text for the GitHub comment

**Critical: the analyser's job is to read the CI failure logs and determine whether those
failures are caused by this specific dependency bump.** If yes → `needs_changes`. If no,
and the bump looks safe → `approve`. If unsure → `needs_human_review`.

**The analyser is a hard gate on whether a fix is even attempted.** The verdict is decided
*before* any implementation, and the implementation pipeline (step 7) runs only when the
verdict is `needs_changes` **and** confidence is `medium`/`high`. Two ways the analyser vetoes
an attempt without ever trying one:
- `needs_human_review` → flagged for a human; the pipeline never runs.
- `needs_changes` but **low confidence** → *also* flagged; the analyser's own self-rated
  confidence is enough to block the attempt.

The only guidance steering toward `needs_human_review` is "be conservative — if unsure, flag
it," plus "recommend `needs_changes` only if the failure plainly traces to the bump." There is
no positive definition of when `needs_human_review` is the *right* answer; it is the residual
bucket for uncertainty. The prompt therefore biases toward flagging on doubt — and that veto is
cast by the worst-equipped agent (non-agentic, truncated context, no checkout, per T7) over
whether the best-equipped one (the agentic worker) should even try. See T8 and Q10.

### Step 5 — Route based on verdict and CI state

This is the branching point. Priority order:

| Condition | Outcome |
|---|---|
| CI is failing AND analyser says `needs_changes` | → implementation pipeline |
| CI is failing AND analyser does NOT say `needs_changes` (i.e. `approve` **or** `needs_human_review`) | → **flag** (CI note + review body) |
| Confidence is `low` | → **flag** |
| Analyser says `needs_human_review` | → **flag** |
| Analyser says `approve` (and CI is acceptable) | → **recommend merge** (comment) |

**Note on row 2 — `approve` is permitted-then-overridden, not forbidden.** "Does not say
`needs_changes`" is the set {`approve`, `needs_human_review`}, and both are routed identically
to a human flag. The analyser is *not* prevented from returning `approve` when CI is failing —
the prompt explicitly allows it — so the desired invariant "`approve` never results when CI is
failing" holds only at the *output* level, by the router downgrading it. It is **not** enforced
at the *verdict* level. **DECIDED (Q4): `approve` must be disallowed when CI is failing, enforced
programmatically** (prompt + output validation), so `approve` structurally means "CI is
acceptable" end-to-end instead of being emitted-then-overridden.

"CI is failing" here means `AcceptableGiven` returns false — see the CI acceptance model
below.

**Where "no recommend-merge when CI is failing" is enforced.** Not in the analyser prompt —
that prompt *permits* `approve` with red CI ("an unrelated pre-existing issue, in which case
the bump may still be safe to recommend separately"). The rule is enforced **programmatically**
in `actOnAnalysis` by the guard `if !acceptable && recommendation != needs_changes → flag`. An
`approve` verdict with unacceptable CI is therefore *silently downgraded* to a human flag (its
`review_body` is still posted as a comment, with an appended "CI is failing — not an approval"
note); it is never rejected or sent back to the analyser. Two precise consequences:
  - The gate is `AcceptableGiven`, not raw CI state — so `approve` *is* honoured as a merge
    recommendation when the only red checks are pre-existing base failures or ignored (those
    are "acceptable"). "CI not passing" means "has a blocking check," not "is fully green."
  - The analyser and the orchestrator disagree about what `approve` means: the prompt allows
    it with failing CI, the router won't honour it. This is the Q4 fault line — the cleaner
    design may be to forbid `approve` in the analyser whenever CI is unacceptable, so the
    verdict means one thing end-to-end instead of being emitted then overridden.

### Step 6 — Implementation pipeline (when `needs_changes`)

See the implementation pipeline section below.

---

## CI acceptance model

The function `AcceptableGiven` decides whether CI is "good enough" to proceed. It is used
in two places:
1. **Routing** (step 6) — is CI acceptable enough that we don't need to flag it?
2. **Implementation gate** — has the agent's work produced acceptable CI?

A CI check **blocks** unless:
- It is in the operator `--ignore-check` list (structurally broken / irrelevant checks)
- It is already failing on the **base branch** (`main`) — the bump didn't cause it

Every other failing check blocks.

The base branch check is best-effort: if the base branch CI cannot be fetched, the set of
base failures is empty and only the ignore list applies.

> **DECIDED — this model is largely replaced (Q1/Q2/Q3).** The tool's job is to get the PR to
> *genuine green*, so:
> - **No attribution** (Q2): source of a failure is irrelevant — a failure is a failure, fix it.
>   This removes the entire "is this the bump's fault?" question (T1 is eliminated).
> - **Base-branch suppression as a *success criterion* is dropped** (Q3): a check red on `main`
>   is not "acceptable," it is *more work* — the agent fixes it too (a welcome side effect). The
>   bar to open (un-draft) the replacement PR is genuine green.
> - **Silent on failure** (Q3): if the agent can't reach green, the replacement stays a silent
>   **draft** — no comment, no un-draft, no noise. Output is produced only on success. (A
>   human-attention flag is the *exception*, reserved for a genuinely useful insight, and then
>   must carry a concise reason — otherwise say nothing.)
> - The only remaining suppression is the operator `--ignore-check` list, kept as an escape
>   hatch for known-unfixable checks (to avoid burning effort), not as a success fudge.
>
> **DECIDED (Q7): "CI passing" ≡ "required checks passing" — treat the terms as synonymous
> everywhere.** The repo owner defines the required set; that *is* the merge bar, so it is the
> tool's bar too. Consequences: "green", "acceptable", and "fix all failures" are all scoped to
> **required** checks. A non-required red check never blocks opening and the agent need not chase
> it (no wasted effort on unfixable non-required `main` failures). A *required* check red on
> `main` still must be fixed (it blocks merge) — that is the "more work" of Q3. This resolves T5.

---

## The implementation pipeline

Entered when the analyser says `needs_changes` and a replacement PR does not already exist.

### Phase 0 — Rebase

If the PR branch is behind its base, the tool rebases it (`git rebase -X theirs`) and
force-pushes. This is always driven directly by the tool — never via `@dependabot rebase`
(which can close the PR).

### Phase 1 — Clone and branch

Clone the repo, create a working branch (`auto/fix/<package>-<version>`) from the
dependabot PR's head.

### Phase 2 — Worker turn 1

Launch a bounded Claude Code worker subprocess. The worker receives:
- A brief describing the dependency change and the analyser's assessment
- The list of code changes the analyser identified
- A note about which CI checks are known to be pre-existing/ignored (so it doesn't waste
  effort on them)

The worker's contract: make the code changes, push, open a **draft** PR, and exit. It
never polls CI — the orchestrator owns that.

### Phase 3 — CI gate loop

After each worker turn, the orchestrator waits for CI to settle on the worker's branch,
then evaluates `AcceptableGiven`. Three outcomes:

| CI state | Action |
|---|---|
| Acceptable | Proceed to review gate |
| Not settled (still running) | Keep waiting |
| Settled but not acceptable | Resume the worker with the failure logs (if iterations remain) |
| Iteration or time cap reached | Give up |

No-progress guard: if the same set of failing checks recurs across N consecutive resume
turns, the worker cannot fix them — the loop is abandoned. Precisely:
- **N** = `config.MaxNoProgressIterations`, **default 3**, and *not exposed as a CLI flag* —
  effectively hardcoded to 3 in deployment.
- The match is **exact set equality** (`sameSet`: same names, order-insensitive). Stale-pending
  checks carry a `" (stuck)"` suffix in the blocking list, so a check flipping between failing
  and stuck reads as a *different* set.
- **Any change resets the counter to 0** — a check appearing *or* disappearing. The guard fires
  only when the identical non-empty set persists for N consecutive resume turns.
- The counter is **per review cycle**: a reviewer-requested-changes turn re-enters the CI gate
  with the no-progress tracking reset.

**Limitation (T10):** because any change resets the counter, a worker that *thrashes* —
different checks failing each turn (fix A breaks B, fix B breaks A) — never trips this guard.
Thrashing is bounded only by `MaxImplIterations` (default **30**) and `MaxImplTime`. The guard
catches a *stationary* stuck set, not an *oscillating* one — yet oscillation is precisely the
expensive-flailing mode the guard was meant to prevent.

### Phase 4 — Review gate

CI is acceptable. A separate **reviewer** Claude API call checks the diff against the
original assessment: are the changes consistent? Were any tests deleted? Any workarounds?

- Reviewer approves → proceed to squash
- Reviewer requests changes + retries remain → resume the worker with reviewer concerns,
  re-enter the CI gate
- Reviewer requests changes + retries exhausted → flag for human

### Phase 5 — Squash and finalize

The agent's multi-turn commit history is squashed into a single `fix:` commit on top of
the dependabot commit (two-commit structure: bump + fix). Force-pushed, post-squash CI is
verified, then:
- Original dependabot PR closed with a reference to the replacement
- Replacement PR marked ready for review

**⚠️ This phase has a serious confirmed bug — see T9.** The squash resets to the *scan-time*
`pr.HeadSHA`, which is stale after a Phase-0 rebase, producing a "fix" commit that bundles in
every unrelated change merged to `main` since the branch's original base.

---

## Two-PR-type lifecycle

The sweeper manages two distinct kinds of pull request on the target repo:

1. **Dependabot PRs** — opened by `dependabot[bot]` or `renovate[bot]` (or a test-bed
   accepted author). These are the unit of work: the tool scans them, decides what to do, and
   either comments on them or closes them in favour of a replacement.

2. **Sweeper ("fix") PRs** — opened by the tool itself when an implementation is needed. Each
   is a replacement for one dependabot PR, containing the same version bump plus one or more
   additional commits that make the change compatible.

### Naming

A sweeper PR's title is derived programmatically from the originating dependabot title by
swapping the conventional-commit type to `fix` (`SweeperPRTitle` in
`internal/implementation/implementation.go`):

- `build(deps): bump X from A to B` → `fix(deps): bump X from A to B`
- `chore(deps): bump X from A to B` → `fix(deps): bump X from A to B`
- A bare `Bump X from A to B` (no conventional prefix) → `fix(deps): Bump X from A to B`

The rule: replace whatever conventional-commit type was there with `fix`, keeping the scope
(e.g. `(deps)`) and everything after the colon intact. When there is no recognisable
conventional prefix, prepend `fix(deps): `. The scope and description are never rewritten.

This makes sweeper PRs visibly and structurally distinct from dependabot PRs — the `fix` type
appears in the PR list, making it clear which PRs the tool authored and which are raw dependabot
bumps. (Before Q14, the tool overwrote the title with the verbatim dependabot string, making
the two PR types indistinguishable.)

### Exclusion from scanning (reap-exempt `created_prs` table)

Because sweeper PRs are opened under the same accepted-author login used on the test bed (and
could in principle be from any accepted author), the author filter alone is not enough to keep
them out of the scan. A sweeper PR re-entered as a fresh dependabot PR would trigger an
expensive agentic step — a runaway-cost incident.

The solution is a permanent DB record. Every time the tool opens a replacement PR it writes a
row to the `created_prs` table:

```sql
CREATE TABLE IF NOT EXISTS created_prs (
    pr_number  INTEGER PRIMARY KEY,   -- the sweeper "fix" PR number
    origin_pr  INTEGER NOT NULL DEFAULT 0,   -- the originating dependabot PR
    created_at INTEGER NOT NULL DEFAULT 0    -- unix nanoseconds, UTC
);
```

At the start of every scan cycle, `excludeOwnPRs` checks the list of currently open accepted-
author PRs against this table and drops any whose number appears in it, *before* any per-PR
processing begins.

Critically, `created_prs` is **never reaped**. `Store.Reap` prunes `pr_progress` (which is
keyed to the live open-PR set and gets stale rows pruned each cycle), but it never touches
`created_prs`. This means the exclusion record outlives the `pr_progress` row for the sweeper
PR — even after the PR is merged or closed and its `pr_progress` row is gone, the tool still
knows it created that PR number and will not re-ingest it if it somehow reappears (Q14 /
review C1).

The exclusion is keyed on PR **number**, not on branch name or title. Branch names are
attacker-controllable (anyone can open a PR from a branch named `auto/fix/...`); the DB record
of what-the-tool-actually-created cannot be spoofed.

### Pairing with the originating dependabot PR

The `created_prs` table records `origin_pr` (the dependabot PR number the sweeper PR was
created for). In parallel, `pr_progress.replacement_pr` on the dependabot PR's row records the
sweeper PR number (forward link). Together these provide bidirectional pairing:

- **Forward** (`pr_progress.replacement_pr`): given a dependabot PR, find its replacement.
- **Reverse** (`created_prs.origin_pr`): given a sweeper PR number, find the dependabot PR it
  came from.

The pairing feeds the dashboard's `#183 / #204` display (Phase 4 — see WORKPLAN.md).

### Full lifecycle

```
dependabot opens PR #183: "build(deps): bump X from 1.0 to 2.0"
         │
         ▼
tool scans #183 → implementation needed
         │
         ▼
tool opens draft PR #204 with title "fix(deps): bump X from 1.0 to 2.0"
  → writes created_prs row: (pr_number=204, origin_pr=183)
  → writes pr_progress.replacement_pr=204 on row 183
         │
         ▼
CI loop: worker pushes additional commits; CI verified green
         │
         ▼
finalize:
  → original PR #183 closed with a reference to #204
  → #204 un-drafted (marked ready for review)
         │
         ▼
next cycle scans open PRs:
  → #183 is closed → not in open set → not scanned
  → #204 is open, authored by the accepted author → author filter admits it
  → but created_prs contains pr_number=204 → excludeOwnPRs drops it
  → #204 is never re-processed
```

---

## Known tensions in the current design

These are the places where the current algorithm is incomplete, ambiguous, or known to fail.

### Synthesis — one root cause behind most of the tensions

A cluster of these tensions are not independent bugs; they are **facets of a single
architectural choice: a separate, weakly-equipped, *predictive* analyser that gates the
implementation phase.** The analyser must predict — from truncated context, with no checkout —
whether a bump is safe, whether CI failures trace to it, and whether a fix is possible; the tool
then *routes on that prediction*. Every place the prediction can be wrong or can contradict the
empirical state becomes a tension:
- **T1** — attribution (the prediction) is the single most critical and most error-prone call.
- **T2** — routing on the prediction creates a contradiction when "approve" meets red CI.
- **T3** — the prediction needs a special "inherited failure" category to avoid chasing ghosts.
- **T7** — the prediction is made by the *least*-equipped agent.
- **T8** — the prediction *blocks the empirical test* that would actually answer the question.

The **fix-first model** (attempt the fix directly; `needs_human_attention` is a last-resort
*outcome* with a concise reason; a cheap pre-pass may only short-circuit the truly-safe no-op)
collapses this entire cluster at once. The remaining tensions below (T4, T5, T6/T6a, T9, T10,
T11, T12, T13) are largely independent of this root cause and stand on their own.

### T1 — The analyser's CI attribution is the single most critical decision → ELIMINATED (Q2)

**Resolved by Q2: there is no attribution step.** Originally the routing decision lived entirely
on whether the analyser correctly identified CI failures as caused by the bump — if it said
`needs_changes` for unrelated failures, the tool chased the wrong thing (the PR #137 case:
11h/$13 on edits unrelated to babel-loader). The prompt's guard ("the failure has to plausibly
trace back to the dependency change") relied wholly on the analyser's reasoning over truncated
logs, with no independent verification.

The maintainer's decision removes the question outright: **source of a failure is irrelevant — a failure
is a failure, fix it (Q1=b, Q2)**. The tool's job is to get the PR to green, whatever is red. So
the most error-prone decision in the system simply ceases to exist. (PR #137's actual causes —
the missing give-up guard, since fixed, and gating on all checks rather than required, T5 —
remain addressed elsewhere.)

### T2 — The routing logic in step 6 has an ambiguous case

When CI is failing AND the analyser says `approve` (i.e. the bump is safe but CI is red),
the tool flags the PR with a note saying "CI is failing, failures may be pre-existing."

But the tool already computed `baseFailures` (what's failing on `main`). If the only
failing checks are pre-existing on main, they should not have blocked routing in the first
place. The current code calls `AcceptableGiven(ignored, baseFailures, ...)` — so base
failures ARE already suppressed. If the tool reaches the "flag for CI" path, it means there
are genuinely new failures on the PR that aren't on main and aren't ignored.

The question is: what should happen when those "genuinely new" failures exist but the
analyser says the bump is safe? The current answer is "flag." An alternative is "flag AND
note that these failures look unrelated to the bump."

**Resolution: this ambiguity is a symptom of the analyser/implementation split (see Synthesis
and T8).** It exists only because the tool routes on a *predicted verdict* that can contradict
the empirical CI state ("approve" while CI is red). Under the fix-first model the case
disappears: red CI on an engaged PR simply triggers an attempt; `approve` is valid only when CI
is already green. There is no contradictory verdict to adjudicate.

### T3 — Bug A's "inherited PR-branch failure" category does not exist; reject it

The proposed Bug A category was: a check green on `main` but red on the dependabot PR *before
the tool touched it*, "not caused by the bump" (fork-mirror artefact, flake, structural issue),
to be snapshotted at first observation, classified "inherited," and flagged-instead-of-fixed.

**This category is a false premise.** If a check is green on `main` and red on the PR branch,
the difference is by definition something about the *branch*. There are only two cases, and
neither warrants suppression:
1. **Branch content or state caused it** — the bump itself, *or* the branch being stale/diverged
   from current `main` (a branch off an old base genuinely lacks newer `main` code, so a
   dependent check fails — the "fork-mirror artefact" is just this). Both are fixable: code
   changes, or the Phase-0 rebase the tool already does. In scope.
2. **Non-determinism (a flake)** — the agent tracks the intermittent down and fixes it too. In
   scope.

Anything genuinely structural to the fork already has a home: if it is red on the fork's `main`
too it is caught by **base-branch suppression**; if it is fork-only noise it goes on the explicit
**`--ignore-check`** list. Neither needs an *automatic* "inherited" category.

**Conclusion: red on the PR ⟹ fix it (or rebase, or de-flake).** (Note — refined by Q7: the bar
is *required* checks, so a red *non-required* check on `main` simply isn't gated on at all, rather
than being "suppressed." A red *required* check on `main` must be fixed, per Q3. So the only real
suppression left is *operator-ignored*; "red-on-`main` suppression" as a standalone concept is
subsumed by required-checks gating.) Bug A was not a deferred-but-valid fix;
it was built on a category that shouldn't exist — close it. (The PR #137 blow-up was the absent
give-up guard, since fixed as Bug #24, plus gating on *all* checks rather than *required* ones,
T5 — not a missing inherited-detector.) One pragmatic caveat: confirming a flake vs a real
failure can need re-runs, and "fix the flake" can expand scope — an execution detail to bound,
not a reason to suppress.

### T4 — `gave_up` is a valid terminal and is wired correctly; only a stale comment is wrong

**Verified.** Give-up after exhausting allowed attempts is a valid, intended terminal result
(confirmed by the maintainer), and the code already implements it: when `result.GaveUp == true`
(no-progress / exhausted iterations / time-cap / post-squash CI fail), `actOnAnalysis` reports
`StageGaveUp` and records that outcome (orchestrator.go:760-766). The graph edge
`dec_ci_gate → gave_up` matches.

The only defect is a **stale comment** on the `gave_up` node in `spec.go`, which still claims
"currently the CI-fix-loop give-up path routes to `flagged_human` instead (Bug #24) … once Bug
#24 is fixed." Bug #24 *is* fixed (the no-progress guard exists), so that text contradicts the
actual behaviour and the graph edge. Action: correct the comment. No behavioural change.

Note for later: under the fix-first model + the "every human-attention flag carries a concise
reason" rule, `gave_up` becomes the principal "needs human attention" terminal (tried hard,
couldn't), with `flagged_human` covering other triggers. Decide whether they stay distinct (the
trigger differs) or consolidate behind a single human-attention outcome whose reason field
carries the specifics.

### T6 — The decision graph is descriptive, not enforced (and the code violates it)

`internal/workflow/spec.go` is presented as "the single source of truth" for the state
machine, and it *looks* authoritative — there's a test suite and a rendered `/how-it-works`
diagram. But it is **pure data that nothing at runtime consults**:
- Its only non-test consumer is the web server, which serves it as JSON for the diagram.
- There is no `allowedTransition`/`validTransition` guard anywhere. `Store.Report(stage)`
  records *any* stage unconditionally and appends a history event for it.
- `spec_test.go` only checks the graph's internal consistency (every `PRStage` is a node,
  edges point at real nodes, terminals are reachable from entry). It does **not** check that
  the running code's transitions follow the graph's edges.

The consequence is observable: a finalized PR shows a history of `finalized → pending →
pending → finalized` on every cycle, even though the graph has no `finalized → pending` arc.
That transition happens because two reporting calls fire unconditionally each cycle (see the
reporting-noise note below), and nothing rejects the illegal transition.

**This is the core "lost the narrative" failure.** The map (graph) and the territory (code)
were never wired together, so they drift independently and the graph stops describing reality.
See Q8.

### T6a — Idempotent skips still pollute the stage history (reporting noise)

The step-4 "already processed at this head SHA" skip correctly avoids all LLM work — but it is
not silent. Every cycle, for *every* PR (including terminal ones), three `reportStage` calls
fire: the `Run()` pre-populate loop stamps `pending`, `processPR` stamps `pending` again, and
the skip path stamps the stored terminal outcome. So a settled, finalized PR accrues three new
history events per cycle forever. The idempotency guarantee ("no LLM calls, no PR comments") is
intact; the *dashboard history* guarantee is not. A no-op cycle should record no transition.

### T9 — ⚠️ BUG: squash uses a stale base SHA, bundling unrelated `main` changes into the fix commit

**Confirmed serious bug.** Replacement-PR "fix" commits sometimes contain a large volume of
changes unrelated to the bump (observed on petemoore/taskcluster #193, commit `c1671f49`). The
commit is labelled as just the fix but its diff includes unrelated code.

**Root cause — a stale SHA after the Phase-0 rebase:**
1. Phase 0 (`implementation.go:368`): if the PR branch is behind base, `manualRebase` rebases it
   onto `origin/<base>` and force-pushes. Rebase **rewrites every commit SHA**, including the
   bump commit.
2. `pr.HeadSHA` was captured at scan time (`GetDependabotPRs`), *before* the rebase, and is
   **never updated**. It now points to the old, pre-rebase bump commit.
3. `cloneAndBranch` (`:837`) checks out the branch at its new post-rebase head.
4. `squashBranch` (`:557`) runs `git reset --soft pr.HeadSHA` against the **stale** SHA. Since
   `reset --soft` leaves the working tree untouched (= current-main + bump + agent fix), the
   staged-and-committed diff becomes **(everything merged to `main` since the branch's original
   base) + (the agent's actual fix)**, collapsed into one commit mislabelled "fix".

The tip *tree* is correct; it is the squashed commit's *diff/history* that is wrong, because its
parent is a commit from before `main` advanced. Only PRs that were **behind base and rebased**
are affected — which matches the "several PRs" observation.

**Empirical confirmation to run:** on an affected PR, check the bloated commit's parent SHA — it
should equal the pre-rebase head (the scan-time `pr.HeadSHA`), not the post-rebase bump commit.

**Fix — DECIDED (the narrow fix; Q11 kept inherit-and-rebase).** Never use the scan-time
`pr.HeadSHA` as the squash base. Capture the branch's *actual* tip SHA immediately after
`checkout -b branch pr.HeadRef` in `cloneAndBranch` (before the worker runs) — the true
post-rebase bump commit — and squash the agent's work relative to that. (The fundamental
alternative — reconstruct-from-base — was rejected in Q11 because it would require every
ecosystem's toolchain in the worker image to regenerate lockfiles; inheriting dependabot's bump
commit carries its lockfile for free.)

### T13 — The worker's reasoning is never captured as a first-class artifact → RESOLVED (Q15)

**Resolved by Q15:** the implementer authors a structured justification (held private through the
review loop, reviewed by the reviewer, posted to the PR body only on final approval), replacing
the dead `ReviewVerdict.Summary`. Detail below describes the original gap.

The replacement PR should carry a clear, concise justification for *why the fix is valid* — but
the worker's reasoning (the agent with the most context) is currently lost three ways:
1. **The PR body is free-form.** The worker fills a loose template (`## What changed upstream`,
   `## Changes made`) at `gh pr create` time. No length discipline, no required "what upstream
   changes were considered and dismissed as irrelevant, and why." Quality is unconstrained.
2. **The reviewer's summary is discarded.** `ReviewVerdict.Summary` is prompted explicitly "for
   the PR body" (2–4 sentences) but `verdict.Summary` is never read anywhere — a hollow field.
3. **The reviewer never receives the worker's reasoning.** It is given the *analyser's*
   assessment + the diff + commit messages — not the worker's own explanation. So the downstream
   reviewer must *infer* the worker's intent from the raw diff, with strictly less context than
   the worker had. (Compounded by T11: the squash also discards the worker's commit messages.)

**Decided direction — see Q15:** the worker (most context) authors a single concise justification,
capped (e.g. ≤3 sentences): the scope of the upstream update, which parts affected the repo, how
the upgrade was made compatible (how breaking changes were handled), and what upstream changes
were considered but not relevant and why. That one artifact is routed to *both* the PR body (for
humans) and the reviewer (so it evaluates the worker's actual intent, not a reverse-engineered
guess), and it survives the squash.

### T12 — The sweeper PR is an untracked second PR type, and is named indistinguishably → RESOLVED (Q14)

**Resolved by Q14:** name = dependabot title with type swapped to `fix`; exclude our own PRs via a
DB record of what we created (not branch name — spoofable); pair as an attribute on the dependabot
PR's row. Detail below describes the original problems.

The tool manages two kinds of PR — the **dependabot PRs** it triages, and the **replacement
("fix") PRs** it generates — but the algorithm (and this doc) only models the dependabot side.
The sweeper PR's lifecycle (naming, exclusion from re-processing, pairing, closure) is implicit.
Three concrete problems:

1. **Named indistinguishably.** The agent creates the replacement titled `fix(<pkg>): update
   code for …`, but the orchestrator immediately *overwrites* it with the **verbatim dependabot
   title** (`UpdatePRTitle(replacementNumber, pr.Title)`, orchestrator.go:684) — e.g.
   `build(deps): bump X from A to B`. So a sweeper PR is titled identically to a dependabot PR;
   you cannot tell them apart, and the pairing is invisible.

2. **Re-ingestion risk.** `GetDependabotPRs` filters only by *author* — there is no exclusion of
   the tool's own fix PRs by branch (`auto/fix/`), title, or a tracked marker. In production the
   fix-PR author isn't `dependabot[bot]`/`renovate[bot]`, so it's skipped *by coincidence*. On
   the test bed (`--accept-author petemoore`, which is also the fix-PR author) the fix PR — now
   titled like a dependabot bump — parses as one and can be **re-ingested and re-processed as a
   fresh dependabot PR**. Relying on author mismatch is fragile; the exclusion should be explicit.

3. **Pairing tracked only one way.** `SetReplacementPR` stores `replacement_pr` on the dependabot
   PR's row (forward link). There is no reverse link, the sweeper PR is not a tracked entity in
   its own right, and the UI doesn't surface the pairing.

Proposed direction (see Q14): enforce the replacement title programmatically as the dependabot
title with the conventional-commit type swapped to `fix` (`s/^build/fix/` on the type, scope and
description preserved) — making sweeper PRs visibly and structurally distinct *and* enabling a
clean "exclude our own PRs" filter; and track the pairing bidirectionally so the UI can show
`#183 / #204` and navigate to either.

### T11 — Squash flattens intentional commit structure and discards the agent's messages → RESOLVED (Q13)

**Resolved by Q11 + Q13:** dependabot's bump commit(s) are preserved, and the agent curates its
own work into intentional, well-messaged logical commits before finalize (replacing the blind
`squashBranch`). The detail below describes the original flattening behaviour.

`squashBranch` collapses *all* the agent's commits into a single `fix:` commit with an
auto-generated canned message (`fix: update code for X A → B compatibility`). Two losses:
- **Logical structure is destroyed.** If the agent split its work into coherent steps for good
  reason (migrate calls / update schema / fix tests), that's flattened into one opaque commit —
  indistinguishable from flattening genuine iterative churn. The squash treats all agent commits
  as churn.
- **The agent's commit messages are discarded**, contradicting the implementation brief, which
  explicitly tells the agent to "write clear commit messages that explain your reasoning,
  because the review agent and human reviewers will use them." We request thoughtful messages,
  then overwrite them with a canned string.

The squash's legitimate goal is bump-vs-fix separation (two-commit structure for reviewability),
but it overshoots — buying that separation at the cost of the logical steps *within* the fix and
the reasoning in the messages. See Q13.

### T10 — The no-progress guard only catches a stationary stuck set, not thrashing → RESOLVED (Q12)

The no-progress guard (Phase 3) originally gave up only when the *exact same* blocking-check set
recurred across N consecutive resume turns (default N=3). Any change reset the counter, so a
worker that oscillates — fix A breaks B, fix B re-breaks A — kept changing the set and never
tripped it, running to `MaxImplIterations` (30) / `MaxImplTime`. **Resolved by Q12:** replace it
with a progress metric — give up when the count of failing *required* checks hasn't strictly
decreased over the last **8** attempts (configurable). This catches both stationary and
oscillating flailing.

### T8 — The analyser predicts fixability instead of letting the fixer discover it empirically

The analyser decides, *before any code is touched*, whether a fix should be attempted at all.
A `needs_human_review` verdict — or even a `needs_changes` verdict at *low confidence* — blocks
the implementation pipeline entirely. So a guess about fixability prevents the empirical test
of fixability.

This is backwards in three ways:
1. **Worst-equipped agent gates the best-equipped one.** The analyser is non-agentic with
   truncated context (T7); the worker is agentic with a live checkout. The analyser's
   prediction overrides the worker's ability to actually try.
2. **The guidance biases toward not-attempting.** "Be conservative — if unsure, flag it"
   converts uncertainty into a human flag rather than into an attempt. There is no positive
   definition of when `needs_human_review` is *correct*, so it absorbs all doubt.
3. **The cases that genuinely warrant a human would surface from an attempt anyway.** A
   bounded iterate-until-fixed loop investigating a failure would *discover* a true blocker
   (e.g. "the new version introduces a critical vulnerability" / "no code change can satisfy
   these conflicting constraints") and surface it with evidence — strictly better than the
   analyser guessing "looks risky, human please" from a log tail.

What the up-front pass is *legitimately* for: (a) the genuine no-op — CI green and the bump is
safe, so recommend merge without spinning up the expensive agentic pipeline; and (b) seeding
guidance (breaking-change notes, suggested changes) into the worker's brief. Neither requires
it to be a *gate* on attempting a fix.

**Decided direction:** be pragmatic — ask the agent to **fix directly** rather than to
assess whether it *could* fix. The prompt carries a last-resort escape hatch: "if you really
can't fix this, mark it `needs_human_attention`." So `needs_human_review` becomes an **outcome of
a bounded fix attempt** ("tried, iterated, genuinely stuck"), never a **precondition** that
prevents the attempt. A cheap pre-pass may short-circuit only when it is *certain there is
nothing to do* (truly safe → recommend); uncertainty must route to an attempt, never to a flag.

**Firm rule — every human-attention flag must carry a concise explanation of *why*.** This
applies to *all* flag paths (agent escape hatch, give-up, review-exhausted, errors), not just
one. Today most paths instead dump the analyser's full `review_body` (the CI-failing,
low-confidence, and needs_human_review paths all post `review_body`; only the give-up path posts
a concise one-liner). The target is a purpose-built one/two-sentence reason a human can act on at
a glance — the same conciseness bar as the recommend-merge comment. See Q10.

### T7 — The analyser does the most critical reasoning with the weakest access

The analyser makes the single most consequential decision in the system (CI attribution →
routing, per T1), yet it is the least equipped agent to make it:
- It is **not agentic** — a one-shot `Messages.New` call with no tools. It cannot read the
  failing test's source, follow the dependabot compare URL, fetch fuller logs, search the
  codebase beyond the pre-computed grep, or inspect the lockfile resolution.
- Its inputs are **pre-digested and truncated**: failure logs are only the check's
  `output.summary`+`output.text` tail (capped 50 KB total), codebase usage is a grep result
  set (50 snippets max) with the actual checkout already discarded, the diff is manifest-only.

If the real cause is past the log tail, in a file the grep patterns didn't match, or only
visible by following the compare view, the analyser is structurally blind to it — and must
still emit a confident verdict. This is a likely contributor to mis-attribution (the PR #137
class of failure).

There is a sharp asymmetry: the **implementation worker is fully agentic** (a `claude` CLI
subprocess with tools and a live checkout), but the **analyser and reviewer are one-shot API
calls**. The hardest judgement gets the least capability. See Q9.

### T5 — The tool blocks on all checks, but merge only cares about required checks → RESOLVED (Q7)

**Resolved by Q7: gate on *required* checks only** — "CI passing" ≡ "required checks passing."
The body below describes the original (all-checks) behaviour and why it was wrong; the fix is to
read the repo's required-status-checks set and treat only those as blocking.


`AcceptableGiven` treats every failing check as blocking (modulo the ignore list and base
failures). But GitHub's actual merge gate — branch protection's required-status-checks — only
blocks on *required* checks. A non-required, optional, or informational check (CodeQL may well
be one) can be red while the PR is still perfectly mergeable.

So the tool's notion of "acceptable CI" is *stricter than the repo's own merge policy*. This
directly feeds the T1 problem: a red non-required check makes the tool believe the bump is
broken and either flags it or (if the analyser mis-attributes it) sends it into the
implementation pipeline to chase a failure that doesn't actually gate merge. The current
mitigation is the manual `--ignore-check` list — an operator hand-enumerates the noisy checks.
Reading the repo's required-checks list would make this principled instead of manual. See Q7.

---

## Open questions for the revised algorithm

These are not answered in the current code. Answering them determines what the "what it
should be" version looks like.

**Q1. DECIDED → (b).** Try to fix *all* failures on the PR; the entire CI state is the target.
Not just failures attributable to the bump.

**Q2. DECIDED → moot.** Source of a failure does not matter — a failure is a failure and should
be fixed. There is no attribution step, so there is no attribution mechanism to choose. (This is
what eliminates T1.)

**Q3. DECIDED.** The bar to open (un-draft) the replacement PR is *genuine green*, not "green
modulo base failures." If `main` is failing CI, that is "bad luck for sweeper — more work to do":
the agent fixes those too, and getting them green is a welcome side effect. If it *can't* reach
green, the replacement stays a **silent draft** — no comment, no un-draft, no noise, no harm.
The tool produces PR output only when it took a failing PR and got it working. So base-branch
suppression as a success criterion is dropped; only the operator `--ignore-check` escape hatch
remains. "Green" here means **required checks green** (Q7 decided) — non-required reds never
block opening and need not be fixed.

  **Silent-draft idempotency (review M1) — must record a terminal outcome.** A silent failed draft
  is a real open PR on the `auto/fix/…` branch. The current idempotency shortcut
  `FindPRByBranch` (`orchestrator.go:640`) treats "a replacement PR exists" as "finalized" — so a
  silent draft would, next cycle, be falsely marked finalized and the original dependabot PR
  closed. Fix: a give-up/silent-draft outcome must record a sticky `gave_up` outcome at the head
  SHA so the **SHA-skip fires before `FindPRByBranch` is reached**. This is also a cost-safety
  requirement (an un-recorded draft re-enters the agentic step every cycle). **Precondition
  (review N3):** the draft must only be opened *after* a non-empty head SHA is captured, and the
  outcome recorded against the post-rebase tip SHA (per N4) — `recordOutcome`/`SetOutcome` no-op
  on an empty SHA, so an empty one silently leaves the draft un-stickied.

**Q4. DECIDED.** `approve`/recommend-merge is valid only when CI is already green; a failing PR
routes to a fix attempt, never to a recommendation. **Enforcement point under Q10 (review C3):**
there is no longer an `approve` enum to validate — a `recommend` outcome is always **re-gated by
a fresh, mechanical required-CI read in the orchestrator** (not the agent's self-report). If the
agent claims "safe to recommend" but the orchestrator's own read shows required checks not green,
it does not recommend. That mechanical re-read *is* the Q4 enforcement; the old "validate the
`approve` enum" framing is dropped.

**Q5. SUPERSEDED.** Previously decided a `--min-bump-to-engage` config value (default `major`)
that skipped patch/minor bumps. That concept has been removed. The current rule is: engage any
open dependabot PR once its CI has settled, regardless of bump type. The only skip conditions are
stale/superseded PR, already-processed SHA, or dry run. Bump type (`patch`/`minor`/`major`/`unknown`)
is still classified and shown on the dashboard; it is no longer a gate.

**Q6. DECIDED → (a).** Decompose a grouped PR into its member `(package, version)` pairs (already
parsed into `GroupedUpdates`) and run the same semver comparison against individual PRs.
Directional rule:
  - If the **group** covers package P at a version **≥** the individual PR's → the individual is
    redundant → **close it** ("superseded by group #N").
  - If the **individual** PR's version is **>** the group's for P → keep the individual; do **not**
    close the group (it still bumps its other members). The mismatch resolves when dependabot
    re-groups, or the individual lands separately.
  So in practice a group can supersede an individual, but an individual never closes a whole
  group. This kills the duplicate-work / duplicate-replacement-PR problem at the source (b/c
  rejected as papering over it).

**Q7. DECIDED → (a).** "CI passing" ≡ "required checks passing" — synonymous everywhere. Read the
repo's required-status-checks set and gate only on those; a red non-required check never blocks
and need not be fixed. The repo owner defines the required set, so it is the authoritative merge
bar. (Resolves T5; scopes Q1/Q3's "all failures"/"green" to required checks.) **Empty/
under-configured required set (review M2) — DECIDED: fall back to all-checks.** A vacuously-true
"required passing" would otherwise let the tool recommend-merge an all-red PR. Implementation:
needs branch-protection read scope on the token; cache the required set per-base-branch per cycle
to avoid an extra fetch per PR.

**Q8. DECIDED → (c): both.**
  - **Fix the reporting** (mandatory): a no-op cycle records **no transition at all**. Remove the
    unconditional `pending` re-stamp in `Run`'s pre-populate loop and at the top of `processPR`,
    **and the skip-path terminal re-stamp** (`orchestrator.go:347-357`). The dashboard reads the
    last real stage straight from the `pr_progress` row (which already stores it), so no event is
    needed on a no-op — matching the "show last real state, not a per-cycle heartbeat" decision.
    (Review N1: this resolves the contradiction where the skip path re-stamps `finalized` every
    cycle. By emitting nothing, there is also no `finalized → finalized` self-loop for the guard
    to special-case.)
  - **Add a runtime transition guard**: `Report(stage)` (or a layer above it) validates the
    transition against the workflow graph and rejects / loudly logs an illegal one (e.g.
    `finalized → pending`). Converts silent map-vs-code drift into a loud failure — directly
    addressing the "lost the narrative" risk.
  Implementation note: the graph's edges route *through* decision nodes (diamonds, which aren't
  `PRStage`s), so the guard must derive the allowed **stage→stage** transitions by collapsing
  decision nodes — following **both `EdgeKindDecision` and `EdgeKindBack` edges** through non-stage
  nodes (review N2). The back-edges encode the legal CI-fix / review resume loops
  (`impl_resuming → waiting_ci → impl_resuming`, `reviewing → impl_resuming`); a collapse that only
  chases forward/decision edges would make the guard reject a legal mid-implementation resume. The
  guard's test must cover a resume-loop round-trip, not just a forward path.
  **Sequencing (review C2):** the *reporting-noise fix* is independent and lands in Phase 1. The
  *transition guard* must be authored against the **post-Q10 graph**, so it lands only after
  `spec.go` is reconciled to the new state machine (move that reconciliation to the *front* of
  Phase 3, not Phase 5). Building the guard in Phase 1 against the current graph would make it
  reject legal new transitions mid-rework. The guard is also now **cost-safety-critical** (see the
  Idempotency constraint): it is what stops a processed PR re-entering the expensive agentic step.

**Q9. MOOT — superseded by Q10.** This asked whether the *separate* analyser should be agentic.
Q10 removes the separate analyser entirely (folded into the single agentic step), so the question
no longer applies. (T7, which motivated it, is resolved the same way: the one agent has a live
checkout and does its own digging.)

**Q10. DECIDED → (b): one agentic step; the separate one-shot analyser is eliminated.** Green
required-CI is *not* sufficient on its own — it shows the code builds and passes tests, but not
whether an upstream change is semantically concerning. Every engaged PR needs agentic analysis of
the upstream changes + codebase impact. A single agent with a live checkout handles each PR end to
end and ends in one of: **recommend** (green + judged OK, *with a concise WHY* — e.g. "only `xxxx`
changed and we don't use it"); **replacement PR** (needed changes / required-CI red → fix →
required-green → open with justification); or **`needs_human_attention`** with a concise reason
(else silent draft). Cost is tier-3 on every engaged PR but bounded — cost is controlled entirely by the
already-processed-SHA skip (once per new dependabot-PR head SHA) and the idempotency invariant.
This removes the analyser/worker split that was the root cause of the T1–T8 cluster.
  - The **reviewer** (independent check for deleted tests / workarounds on the *fix* path) is a
    separate concern and is kept by default — an independent perspective has value. Revisit only
    if it proves redundant.
  - (a) / (c) — rejected: (a)'s cheap no-op shortcut can't produce the required green-bump
    justification, and (c)'s predictive gate is the thing being removed.
  - **No tier-1/tier-2 short-circuit (locked).** Even a green-CI bump gets the full agentic step,
    because the recommend outcome requires reasoning about upstream changes to write the WHY. Cost
    is *not* capped by skipping the agent on easy PRs — it is capped by the idempotency /
    never-reprocess invariant (see Non-negotiable constraints). So the cost-control burden sits
    entirely on Q8 + C1 + M1 + the SHA-skip being correct. (Resolves the review's M5.)
  **The cost distinction is between three tiers, not "AI vs no AI":** (1) *mechanical* — reading
  the required-checks CI state via the API (free); (2) *one-shot API call* — a single Messages
  request, no clone/tools/multi-turn (cheap, seconds); (3) *full agentic session* — clone +
  `claude` subprocess + tools + many turns (expensive, ~10–100× tier 2). (b) puts every engaged PR
  through tier 3 even for pure no-ops; (a) resolves the easy cases at tier 1/2 and reserves tier 3
  for real code work.
  **Key under Q7:** "can this be trivially accepted?" reduces to "are the *required* checks already
  green?" — which is **tier 1 (mechanical)**. If they pass, the repo's merge bar is met → recommend
  (at most a tier-2 call to *write* the justification). So the no-op gate may need no agent at all.
  **The counter (pulls toward b):** if a green-CI bump should still be sanity-checked for risks CI
  can't see (supply-chain, license, semantic breakage that compiles), a tier-2 triage is exactly
  the weakly-equipped reasoner of T7 — doing it well may need tier 3 anyway, narrowing the gap.
  **So the decision hinges on one belief:** is "required checks green" sufficient to recommend
  (→ (a), cheap, mechanical), or must an agent vet even green bumps (→ tier-3 cost regardless, so
  (b)'s simplicity wins)?
  Also decide what remains a legitimate *pre-attempt* hard stop (e.g. a known critical
  vulnerability in the new version), if any, versus what must be discovered by attempting.
  **Firm sub-requirement:** any human-attention outcome — from any path — must carry a concise,
  purpose-built explanation of why (not a full `review_body` dump).

**Q11.** Branch-construction strategy. How is the replacement branch built so it ends up as
"dependabot bump commit(s) + the agent's additional commits" (per the North Star), without the
T9 stale-SHA bug?

  Three candidate models:
  - **A. Inherit-and-rebase (current).** Take dependabot's branch, rebase onto current base with
    `-X theirs`, agent works on top, squash. Bump commit is *literally* dependabot's. Source of
    the T9 bug and the rebase machinery (`manualRebase`, `IsBranchBehindBase`, `-X theirs`).
  - **B. Reconstruct-from-base.** Agent branches from current base and re-creates the bump from the
    PR description (edit manifest, regenerate lockfile), then adds compatibility commits. Removes
    the rebase machinery and the T9 bug class; conflicts become "just build on current base." But
    the bump commit is a *reproduction*, not dependabot's literal commit.
  - **C. Cherry-pick-the-bump onto current base (hybrid).** Clone current base, cherry-pick *only*
    dependabot's bump commit(s) onto it (preserving the literal bump content), then the agent adds
    its commits. Clean base + a freshly-created, known bump-commit SHA (so the squash base is
    never stale → no T9). Downside: cherry-picking a lockfile bump onto a moved base can conflict
    messily (where B's regenerate-the-lockfile is cleaner).

  **DECIDED → A (inherit-and-rebase) + the T9 narrow fix.** Reconstruct-from-base (B) was briefly
  preferred, but it requires the worker to *regenerate the lockfile* by running the real package
  manager — so every ecosystem's toolchain (Go, yarn, Python, …) would have to be installed in the
  worker image. For a polyglot repo like taskcluster that is a heavy, ongoing cost. Inheriting
  dependabot's bump commit carries its already-correct lockfile as-is, so **no toolchain is
  needed** to produce the bump. The agent makes only *source-code* compatibility edits (it never
  builds/tests locally — it pushes and CI validates), which also need no toolchain. So A wins on
  pragmatism.

  What A still requires / its residuals:
  - **The T9 narrow fix is mandatory.** Use the **post-rebase branch tip** (captured right after
    `checkout -b branch pr.HeadRef`, before the agent runs) as the squash base — never the stale
    scan-time `pr.HeadSHA`. Without this, A still bundles unrelated `main` changes into the fix
    commit. This is a small fix, not a rearchitecture.
  - **Residual edge case:** `-X theirs` resolving a *lockfile* conflict during the rebase (when
    `main` also changed the lockfile) can be subtly wrong; genuinely fixing a broken lockfile would
    then need the toolchain. Rare (only on lockfile conflicts when behind base) and caught by CI —
    acceptable, vs B needing the toolchain always.

  Remaining sub-confirmations (small):
  - **Version fidelity** — inherited from dependabot's commit, so automatically exact.
  - **Authorship** — under A the bump commit *stays* authored by dependabot (a plus).
  - **Commit structure** — preserve dependabot's bump commit(s); consolidate only the *agent's*
    work into one or more logical commits on top (ties to Q13 — don't flatten the agent's logical
    structure, and don't fold it into the bump commit).
  - **Grouped updates** — handled the same way (inherit the group bump commit, rebase, add work).

**Q12. DECIDED → (a), K=8, monotonic floor.** Replace the exact-set no-progress guard with a
*progress metric*: track the **lowest failing-required-check count seen so far** (a monotonic
floor) and give up when that floor has **not improved over the last 8 attempts**. Refined from
"hasn't decreased over the last 8" (review M3): a plain sliding window is gamed by up-down
oscillation (5→4→5→4 strictly decreases every other turn, resetting it); a monotonic floor is
not. Subsumes the stationary case (same set forever) and the thrashing case. Make K configurable
(default 8). `MaxImplIterations` (30) remains the absolute ceiling.
  - Implementation note (review minor): the current `decideNoProgress` sets `prevBlocking` *after*
    the give-up check, so it effectively measures N−1 repeats (off-by-one). Don't inherit that in
    the floor-based rewrite.
  - Cost note: each attempt is a worker turn + a full `verifyCI` settle (up to `CIVerifyMaxWait`,
    default 90 min), so 8 non-improving attempts can be a large real-time/token spend before
    giving up — acceptable but acknowledged. (Resolves T10.)

  Original options for reference:
  - (a) Give up when the failing surface stops *shrinking* over a window of turns (e.g. failing
        count hasn't decreased in K turns), so oscillation at a plateau is caught.
  - (b) Track the union/churn of failing checks and give up when it's not converging.
  - (c) Keep exact-set but also cap distinct-set-changes, so endless oscillation is bounded
        sooner than the global `MaxImplIterations`.
  Also: should N (and this metric) be operator-configurable rather than hardcoded to 3?

**Q13. DECIDED → (c).** The agent produces a clean final history itself — a curate step before
finalize where it reorganises its work into intentional, well-messaged logical commits, exactly
as a careful human would before opening a PR. (a) ships iterative-turn churn; (d) needs a brittle
marking convention; (b) is excluded by the North Star's "one or more commits." Dependabot's bump
commit(s) stay preserved at the base (Q11); the curate shapes only the agent's commits on top.
Resolves T11, and dovetails with the worker-authored justification (Q15) — the same "package it
up for review" instinct.

  Mechanism note: the curate replaces the orchestrator's current blind `squashBranch`. The agent
  soft-resets to the **post-rebase bump tip** (the same SHA the T9 fix captures right after
  checkout) and re-commits its changes as one or more logical commits with real messages.
  **One canonical base SHA (review M4):** the T9 squash base, the Q13 curate reset, *and* the
  reviewer's diff base must all be this same captured post-rebase bump-tip SHA. Today the reviewer
  diffs against `origin/<HeadRef>...HEAD` (`implementation.go:1099`) — i.e. the branch's *own*
  remote head, so it can read empty/wrong once the worker force-pushes (review MINOR-1: it's
  mis-*based*, not merely stale). Replace it with the explicit captured SHA so all three stay
  consistent after the curate's force-push.

**Q14. DECIDED.**
  - **Naming:** programmatically set the replacement title to the dependabot title with the
    conventional-commit type swapped to `fix` (`build(deps): bump X…` → `fix(deps): bump X…`);
    when there is no recognisable type prefix (e.g. a bare `Bump X from A to B`), prepend
    `fix(deps): `. Replaces the current verbatim-copy of `pr.Title`.
  - **Exclusion — by DB record of our own PRs, NOT by branch name.** The tool records every PR it
    creates; those are permanently off-limits for future scans. Branch-name / title heuristics are
    rejected: a branch name is **attacker-controllable** (anyone can open a PR from `auto/fix/…`),
    so keying exclusion off it is an attack surface; the DB record of what-we-created cannot be
    spoofed. **Must be a reap-exempt store (review C1).** The exclusion record CANNOT live in
    `pr_progress`: `Store.Reap` deletes every row whose PR isn't in the open-accepted-author set
    each cycle (`store.go:177`, keyed off `allPRs`), and our own replacement PR isn't an accepted
    author in prod — so a `pr_progress`-based record would be wiped next cycle and the
    re-ingestion bug returns. Use a separate, never-reaped table (e.g. `created_prs`). This ties
    directly to the cost-safety invariant (a re-admitted own-PR = a runaway agentic step). **In-scope** is decided purely by the **author filter** (config: `dependabot[bot]` in
    prod, `petemoore` in the test env) — GitHub controls author identity, so a third party can't
    masquerade as the trusted author. The two together (process only the trusted author's PRs;
    never our own) need no branch/title signal.
    - *Hypothesis to validate (see WORKPLAN):* a PR from the trusted author that *isn't* a bump
      should exit early at bump-classification and never reach an agent — making author-only
      sufficient. ⚠️ NOT guaranteed in current code: an unparseable title is processed as bump type
      `unknown`, and the pipeline does not skip `unknown` bumps.
      Validate with an empty-commit PR from the trusted author.
  - **Pairing/tracking:** the sweeper PR is an **attribute of the originating dependabot PR's row**
    (add a reverse link), not a first-class tracked entity. The dependabot PR is the unit of work;
    the sweeper PR is its outcome. Enables the dashboard `#183 / #204` pairing without a second row.
  - **Doc:** still add a section modelling the two PR types and the sweeper-PR lifecycle end-to-end
    (create → name → record-as-ours → pair → close original).

**Q15. DECIDED.** The implementer (most context) authors the fix justification as a structured
artifact — covering upstream scope, repo impact, how breaking changes were handled, and what was
considered but dismissed as irrelevant.
  - **Private during iteration.** The implementer↔reviewer loop (multiple rounds if changes are
    requested) is private — the justification is **not** posted to the PR body while iterating, so
    the user is never shown intermediate churn. It is held as a structured artifact (state), not
    written to the PR until the end.
  - **The reviewer reviews the justification too**, not just the code. On the *final* positive
    review (changes + justification both approved), the justification is posted to the PR body and
    the PR flips draft → ready-for-review (this is the "open the PR" moment of the North Star).
  - **Length: strong encouragement, no hard cuts.** The prompt asks for a concise rationale **and
    explains why** (human reviewers appreciate brevity; it is human-processed; over-long context
    overloads). It must **not reproduce information already elsewhere** in the PR — the *what* is
    in the diff, some *why* is in the commit messages; the justification adds the connective *why*.
    Going longer is explicitly allowed when there is critical context that genuinely doesn't fit.
    If it comes back long, the reviewer **challenges** it ("why so long — sure it's needed?"); the
    implementer either justifies keeping it or returns a shorter version (take the shorter). The
    reviewer never truncates — it lacks the implementer's full context.
  - **Replace the dead `ReviewVerdict.Summary`** with this artifact (it was the vestigial version
    of the same idea — computed "for the PR body" but never used).
  See the general prompt-design principle in memory ([[feedback-prompt-why]]): give the agent the
  *why* behind an instruction, not only the *what*.
