# Dependabot Sweeper — Rework Tracker

Living checklist for the rework surfaced in the algorithm review. The **reasoning** for every
item lives in `docs/ALGORITHM.md` (referenced as T# = tension, Q# = open question). This file is
the **status tracker** — check items off as they land. When an item is done, mark `[x]` and note
the commit/PR.

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done · `[?]` needs a maintainer decision first.

> **Status (2026-06-16): PRs #1–#6 are MERGED into `main`** — Phase 0 (validated), Phase 1 (#1),
> Phase 2 (#2), and the independent Phase-3 items Q12 (#3), Q5/Q6 (#4), Q7 (#5), Q14 (#6). Phase 5
> docs are complete (two-PR-type lifecycle section added to ALGORITHM.md). `main` is green
> (build/test/gofmt/staticcheck). Still open: the **analyser-removal cluster**
> (3.0/3.2/3.2b/3.3/3.6/3.8 — agentic, needs a verified env; plan below) and **Phase 4** (UI, needs a
> browser session). See `docs/questions.md` for the two maintainer decisions.

---

## Locked decisions (the direction)

These are decided. They shape everything below. _(Independent Opus review rounds 1–3 incorporated
— findings folded into items below as "review C#/M#/N#". Round-3 verdict: clean to implement, only
trivia remaining after MAJOR-1 (the gave_up SHA plumbing — `RunResult` must carry the post-rebase
tip SHA) was folded in. No Criticals across any round.)_

- **Idempotency is a COST-SAFETY invariant.** Under Q10 every engaged PR triggers an expensive
  agentic step, so "never process the same PR twice" is the *only* thing bounding spend. A
  state-machine bug that re-admits a processed PR is a runaway-cost incident. This is why the
  transition guard (Q8), own-PR exclusion (C1), silent-draft terminal-outcome (M1), and the
  SHA-skip must all be airtight. Cost is NOT capped by skipping the agent on easy PRs (no tier-1
  short-circuit — even green bumps get the full agent to write the WHY).

- **NORTH STAR (drives all architecture).** For each engaged dependabot PR (any bump type, once
  CI has settled), produce one of two well-justified outcomes: (1) **no change needed → comment on
  the existing dependabot PR** with a concrete WHY (required checks green + agent certain no change
  needed); or (2) **changes needed → open the tool's own replacement PR, based on the dependabot
  one** — same bump + one or more additional commits doing the extra work, with a justification of
  why those changes are correct/needed/appropriate and how it's the right solution for this bump,
  grounded in upstream docs/changelogs/diffs. ("Based on" = supersedes it and contains the same
  bump, *not* literal commit reuse.) The justification *is the product* — a human reads a short
  specific rationale and merges with confidence. Last resort only: a concise human-attention flag.
  (Full text: `docs/ALGORITHM.md` → North Star; canonical home `docs/PRINCIPLES.md`.)

- **One agentic step; no separate analyser (Q10→b).** Fix-first, not predict-first. Every engaged
  PR goes to a single agent with a live checkout that analyses upstream + codebase impact and ends
  in: recommend-with-WHY (green + OK), replacement-PR-with-justification (needed changes), or
  human-attention-with-reason / silent-draft. Green CI alone is *not* enough to recommend — the
  agent must reason about upstream changes and state why it's safe. The one-shot analyser is
  eliminated; this removes the analyser/worker split behind the T1–T8 cluster. The independent
  reviewer (fix path only) is kept by default. (Synthesis, T1, T2, T8, Q10)
- **No attribution.** Source of a CI failure is irrelevant — a failure is a failure, fix it.
  (Q1→b, Q2→moot; eliminates T1)
- **Bar to open a PR = genuine green.** Base-branch failures are "more work," not "acceptable."
  Fixing pre-existing `main` breakage is in scope and a welcome side effect. (Q3; replaces the
  base-suppression success criterion)
- **Silent on failure.** If the agent can't reach green, the replacement stays a silent draft —
  no comment, no un-draft, no noise. Output is produced only on success. (Q3)
- **`approve` only when CI is already green**, enforced programmatically (not prompt-only).
  (Q4; T2 / Step 6)
- **`needs_human_attention` is a last-resort *outcome*, never a precondition**, and every
  human-attention flag (any path) must carry a *concise* purpose-built reason — or say nothing.
  (T8, T4, Q10)
- **No "inherited failure" category.** Red-on-PR ⟹ fix / rebase / de-flake. Only legitimate
  suppression is the operator `--ignore-check` list. Bug A is closed (false premise). (T3)
- **"CI passing" ≡ "required checks passing"** (synonymous). Gate only on the repo's
  required-status-checks set; non-required reds never block and need not be fixed. (Q7→a; T5)
- **Inherit-and-rebase + the T9 narrow fix (Q11 → A).** Keep inheriting dependabot's bump commit
  and rebasing it onto current base (`-X theirs`); fix T9 by using the *post-rebase branch tip* as
  the squash base, not the stale scan-time SHA. Reconstruct-from-base was rejected: it would need
  every ecosystem's toolchain in the worker image to regenerate lockfiles; inheriting carries
  dependabot's lockfile for free. Residual: `-X theirs` lockfile-conflict resolution can be subtly
  wrong (rare, CI-caught). (Q11; narrow fix for T9)
- **Sweeper PRs are a distinct, tracked type**, named `fix(...)` (dependabot title with the
  conventional-commit type swapped from `build`→`fix`), excluded from re-ingestion, paired
  bidirectionally with their dependabot origin. (T12, Q14)

---

## Implementation plan (phased — step through in order)

Each phase is a checkpoint; substantive code changes go via **draft PRs** with CI watched (public
repo). Tick the phase when its items below are done.

- [x] **Phase 0 — Validation experiments** (read-only; de-risk before changing anything).
  Confirm T9 on #193; run the empty-commit-PR test. See *Validation experiments* below.
  **Done 2026-06-15** — both confirmed; T9 confirmed with a richer-than-expected picture (a
  `-X theirs` rebase-pollution component, flagged in `docs/questions.md`).
- [x] **Phase 1 — Safe independent fixes** (low risk, no design dependencies). **PR #1 (merged).**
  T4 stale comment ✓; T6/T6a **reporting-noise fix only** (no-op cycle records nothing) ✓ — the
  transition guard is deferred to Phase 3 per review C2. Dead fields (`ReviewVerdict.Summary`,
  `BudgetSpent`) **deliberately deferred** to their owning phases (Summary → Q15/Phase 3.8 replaces
  it; BudgetSpent → billing backlog) rather than removed-then-re-added.
- [x] **Phase 2 — T9 narrow fix** (squash base = post-rebase branch tip ✓; **stored on a struct
  field** `Pipeline.bumpTipSHA` reused by the squash and the reviewer-diff — review M4 ✓; **also
  returned in `RunResult.TipSHA`** and the orchestrator's gave_up path records the outcome (+ sticky
  comment marker) against it via `terminalSHA()` — review N4/MAJOR-1 ✓; all other `recordOutcome`
  calls left on `pr.HeadSHA`). **PR #2 (merged).** ⚠️ Phase-0 surfaced a `-X theirs` rebase-pollution
  component this narrow fix doesn't fully clean — see `docs/questions.md`.
- [~] **Phase 3 — Core rework** (large, interrelated). _Within-phase order deviated to bank
  independent, verifiable wins first; the analyser-removal cluster (0/2/2b/3/6/8) is agentic and
  can't be exercised in this environment — see `docs/questions.md` and the **Phase 3.2 plan** below.
  PR numbers are draft PRs on this repo._
  0. [ ] Reconcile `spec.go` to the post-Q10 state machine FIRST (review C2) — **deferred (cluster)**.
  1. [x] Required-checks gating (Q7) — empty required set → all-checks (M2). **PR #5.**
  2. [ ] One-agent step; remove the separate analyser (Q10) — **deferred (cluster; agentic, see plan)**.
  2b. [ ] Q8 transition guard (after spec.go reconciled) — **deferred (cluster)**.
  3. [ ] No-attribution + silent-draft + approve-only-when-green (Q1/Q2/Q3/Q4) — **deferred (cluster)**.
  4. [x] Group↔individual supersession (Q6). **PR #4.** (Q5 `--min-bump-to-engage` was also shipped here but has since been superseded — see Q5 in ALGORITHM.md.)
  5. [x] No-progress progress-metric, K=8 (Q12). **PR #3.**
  6. [ ] Agent curates its own history (Q13) — **deferred (agentic; pairs with the cluster).**
  7. [x] Sweeper-PR naming + DB-record exclusion + pairing attribute (Q14). **PR #6.**
  8. [ ] Worker-authored justification, private-through-review, posted on approval (Q15) — **deferred (agentic).**
- [ ] **Phase 4 — UI items — DEFERRED (needs a browser-capable session).** PR→GitHub link;
  `#183 / #204` pairing display. Not done autonomously: this is Svelte UI work and the project rule
  is to browser-smoke-test UI changes — not possible headlessly here, so shipping unverified
  rendering would violate "verify before claiming." The pairing half also needs Q14's reverse link
  (PR #6) merged first. **Plumbing sketch:** add `URL` to `PRProgress` (persist via store + schema),
  set it from `pr.URL` in the orchestrator, expose it in the web API (`api.ts` type), render the PR
  number in `PrDrawer.svelte` as a link; the pairing reads `replacement_pr` (forward) + `created_prs`
  (reverse, from Q14).
- [x] **Phase 5 — Docs.** `docs/PRINCIPLES.md` **created** (resolves the dangling reference that
  ALGORITHM.md/memory pointed at; North Star + non-negotiables at current-state UX level, mechanism
  left to ALGORITHM.md). **Two-PR-type lifecycle section added to ALGORITHM.md** (naming via
  `SweeperPRTitle`, reap-exempt `created_prs` exclusion, bidirectional pairing, full lifecycle
  diagram). (`spec.go` reconciliation is in the Phase 3.2 cluster.)

**Cross-cutting (review):**
- **Tests** for every new gate, extending the existing pure-function test pattern
  (`decideNoProgress`, `AcceptableGiven`, `Settled`, `FindNewerPRForPackage`): required-checks
  gating incl. empty/partial protection (M2); the Q12 monotonic-floor metric vs oscillation (M3);
  the Q14 exclusion surviving a `Reap` (C1); the transition guard's decision-node collapse
  including a resume-loop round-trip (Q8/N2); the SHA-skip firing after a rebase changes the head
  (N4).
- **DB migration:** not needed now — the test DB is disposable (recreated each run via the PR-reset
  scripts). Flag for when real production traffic lands: the GCP-persisted DB would then need
  versioned migrations. Q10 rollback: optional — consider keeping the analyser path behind a flag
  for one release in case the single-agent step regresses (low priority given test-bed stage).

Rationale for the order: Phase 0 confirms assumptions cheaply; Phases 1–2 bank low-risk wins and
fix live bugs without touching the architecture; Phase 3 is the architecture and is ordered so
each step rests on the previous (the CI bar, then the single agent, then behaviour, then the
supporting features); Phases 4–5 are polish and durable documentation.

---

## Phase 3.2 cluster — implementation plan (DEFERRED)

The analyser-removal cluster — **3.0 (spec.go reconcile), 3.2 (single agent), 3.2b (Q8 guard),
3.3 (no-attribution/silent-draft/approve-only-green), 3.6 (Q13 curate), 3.8 (Q15 justification)** —
was deliberately **not** shipped autonomously. Two reasons:

1. **It can't be verified here.** These items are fundamentally about *agent behaviour* (prompts,
   multi-turn reasoning, the analyse-then-decide flow). Exercising them needs a real
   `ANTHROPIC_API_KEY` + the `claude` CLI against the test bed. Per the project rule "never imply
   success you haven't observed," shipping an unexercised giant agentic refactor as "done" would be
   dishonest. CI (build/test) passing is necessary but nowhere near sufficient here.
2. **It is one tightly-coupled unit.** These steps collectively *define and implement* the new state
   machine; landing them piecemeal risks an intermediate state where `spec.go` (the map) and the
   code (the territory) disagree — strictly worse than today's drift. They should land together,
   behind the optional Q10-rollback flag, then be e2e-verified.

Phases 2/3 already laid the groundwork the cluster reuses: the **post-rebase tip SHA**
(`Pipeline.bumpTipSHA`, `RunResult.TipSHA`) is the canonical base the Q13 curate and the M1
silent-draft outcome record against; **required-checks gating** is the green bar; the
**reap-exempt `created_prs`** record is the cost-safety backstop.

Recommended sequence when a verified environment is available:

- **A. Reconcile `spec.go` (3.0).** Redraw to the single-agent flow: one agentic node that
  analyses + decides, ending in `recommend` (comment), `finalized` (replacement PR),
  `flagged_human` (concise reason), or `gave_up`/silent-draft. Keep `spec_test` green (every
  `PRStage` a node). Reconcile the `error`-vs-`flagged` stage/outcome mismatch noted in Phase 1.
- **B. Single agentic step (3.2).** Remove/repurpose `internal/analyser`; the orchestrator stops
  calling `Analyse`. Every engaged PR goes to one agent with a live checkout that does its own
  upstream + codebase analysis, then routes to one of the outcomes above. Rewrite the
  implementation brief to own the analyse-and-decide responsibility (apply [[feedback-prompt-why]]).
  Keep the reviewer on the fix path. Consider keeping the analyser path behind a flag for one
  release (Q10 rollback).
- **C. Q8 transition guard (3.2b).** A `Report`-layer guard validating stage→stage transitions,
  collapsing decision **and** back edges (N2); reject/loud-log illegal ones. Test a resume-loop
  round-trip. Cost-safety-critical (stops a processed PR re-entering the agent).
- **D. No-attribution + silent-draft + approve-only-green (3.3).** Drop base-suppression as a
  success criterion (genuine required-green is the bar). Silent failed draft must record a sticky
  `gave_up` at the **post-rebase tip** (Phase 2's `TipSHA` — the plumbing is already in place) and
  open the draft only after a non-empty SHA (M1/N3/N4). `recommend` is re-gated by a fresh
  mechanical required-CI read in the orchestrator, never the agent's self-report (Q4/C3).
- **E. Q13 curate (3.6).** Replace the orchestrator's blind `squashBranch` with an agent curate
  step that soft-resets to `bumpTipSHA` and re-commits the work as one or more logical commits.
- **F. Q15 justification (3.8).** Implementer authors a structured justification, held **private**
  through the implementer↔reviewer loop, posted to the PR body on final approval (PR flips
  draft→ready). Reviewer reviews it too and challenges over-long. Replaces the dead
  `ReviewVerdict.Summary`.

**Before marking any cluster PR ready:** run an e2e cycle against `petemoore/taskcluster` with a
real key, watching that no PR re-enters the agentic pipeline in a loop (the cost-safety invariant),
and confirm each outcome (recommend / replacement / flag / silent-draft) on a real PR.

---

## Confirmed bugs

- [x] **T9 — Squash bundles unrelated `main` changes into the "fix" commit.** **Done — PR #2
  (draft).** Reset base is now the branch tip captured right after `checkout -b branch pr.HeadRef`
  (`Pipeline.bumpTipSHA`), reused by the squash AND the reviewer diff/commit listing (was the stale
  ref name `origin/<HeadRef>` — MINOR-1). Q11 kept inherit-and-rebase. Confirmed empirically on
  petemoore/taskcluster #193 in Phase 0 (the "fix" commit bundled 300 files). The squash-base fix
  is validated by a git-integration test (`TestSquashBranchUsesCapturedTipNotStaleBase`).
- [~] **T6 / T6a — Decision graph not enforced + idempotent skips pollute history. (Q8 → c: both,
  SPLIT across phases — review C2.)** (1) **Phase 1 — DONE, PR #1:** reporting-noise fix — a no-op
  cycle records **nothing**: `prepopulate()` stamps `pending` only for unseen PRs (preserving row
  creation), and the `processPR`-top + skip-path re-stamps are removed; the dashboard reads the
  stored stage from the row. (No `finalized→finalized` self-loop is then emitted.) (2) **Phase 3
  (after spec.go reconciled) — TODO:** runtime guard — `Report(stage)` validates against the
  post-Q10 graph, rejects/loud-logs illegal transitions; collapse non-stage nodes following
  **both decision AND back edges** (review N2). Cost-safety-critical.
- [x] **T4 — Stale `spec.go` comment** on the `gave_up` node. **Done — PR #1.** Comment now
  describes the correct current behaviour (records `gave_up` sticky at the head SHA).
- [x] **T12 — Re-ingestion risk (Q14 → DECIDED):** **PR #6.** Own PRs recorded in a reap-exempt
  `created_prs` table (created→origin) and excluded from scans each cycle; `Store.Reap` only prunes
  `pr_progress` (review C1, regression-tested on both stores). Title is now `SweeperPRTitle` (type
  swapped to `fix`), not a verbatim dependabot-title copy. In-scope stays author-filter-only.
- [ ] **Dead/hollow fields:** `ReviewVerdict.Summary` is computed "for the PR body" but never
  used (T13); `PRProgress.BudgetSpent` is always 0. Wire up or remove. (also in backlog memory)

---

## Design changes to implement

The centrepiece is the fix-first rework; the rest support it.

- [ ] **Collapse analyse+implement into one agentic step (Q10→b).** Remove the separate one-shot
  `analyser` package; a single agent with a live checkout analyses upstream + impact and ends in
  recommend-with-WHY / replacement-PR / human-attention / silent-draft. Green CI alone never
  recommends without the agent's reasoned WHY. Keep the independent reviewer on the fix path.
  (T8, Q10)
- [ ] **Remove attribution + base-suppression-as-success.** Success = genuine green; only
  `--ignore-check` suppresses. (Q1/Q2/Q3; CI acceptance model)
- [ ] **Silent-draft-on-failure + concise-reason-on-flag.** No noise unless there's a useful
  insight; when flagging, a one/two-sentence purpose-built reason, never a `review_body` dump.
  ⚠️ **A silent failed draft MUST record a sticky `gave_up` outcome at the head SHA** (review M1)
  so the SHA-skip fires before `FindPRByBranch` — otherwise next cycle the open draft is mistaken
  for a finalized replacement (false success + original closed) and/or re-enters the agentic step
  (cost). Open the draft only after a **non-empty** head SHA is captured, and record against the
  **post-rebase tip SHA** (review N3/N4 — `recordOutcome` no-ops on empty SHA; scan-time SHA drifts
  after rebase → skip miss → re-entry). (Q3, T8, M1, N3, N4)
- [ ] **Enforce `approve` only when green** programmatically (prompt + output validation). (Q4)
- [ ] **Keep inherit-and-rebase; do NOT reconstruct-from-base (Q11 → A).** Retain
  `manualRebase`/`IsBranchBehindBase`/`-X theirs` (inheriting dependabot's bump carries its
  lockfile for free — no toolchain needed). The only change here is the T9 narrow fix above. Note
  the residual: `-X theirs` lockfile-conflict resolution can be subtly wrong (rare, CI-caught).
- [ ] **Worker-authored fix justification (Q15 → DECIDED).** Implementer authors a structured
  justification (upstream scope, repo impact, how breaking changes were handled, what was
  considered-but-irrelevant). Held **private** through the implementer↔reviewer loop (not in the
  PR body while iterating); the reviewer reviews the justification *and* the code; on final
  approval it's posted to the PR body and the PR flips draft→ready. Length: prompt strongly
  encourages concise *and explains why*, forbids reproducing diff/commit info, allows longer if
  critical context needs it; reviewer challenges over-long ("why so long?") → implementer
  justifies (keep) or shortens (take shorter); **no hard truncation**. Replaces dead
  `ReviewVerdict.Summary`. Apply the "tell-the-agent-why" prompt principle (memory). (T13, Q15)
- [ ] **Commit-history finalization (Q13 → DECIDED → c).** The agent curates its own history before
  finalize: soft-reset to the post-rebase bump tip (same SHA as the T9 fix) and re-commit its work
  as one or more intentional, well-messaged logical commits. Replaces the orchestrator's blind
  `squashBranch`; dependabot's bump commit(s) stay preserved (Q11). Resolves T11; pairs with Q15.
- [x] **Sweeper-PR naming + tracking (Q14 → DECIDED).** **PR #6.** (1) `SweeperPRTitle` swaps the
  conventional type to `fix` / prepends `fix(deps): ` ✓. (2) reap-exempt `created_prs` table +
  scan exclusion ✓. (3) created→origin reverse link stored (feeds the Phase-4 pairing UI) ✓.
  (4) doc section — **Phase 5 (done).** (T12, Q14)

---

## Validation experiments

- [x] **Confirm a non-bump PR from the trusted author exits early.** **Done 2026-06-15 (code
  trace — conclusive; no live PR needed).** Traced `GetDependabotPRs` → `processPR`: an
  unparseable title from an accepted author takes the `default` branch in `GetDependabotPRs`
  (`client.go:156-161`) → `BumpUnknown`, `PackageName = title`. In `processPR` the only early
  bump-type skip is `pr.BumpType == BumpPatch && !pr.Grouped` (`orchestrator.go:236`), so an
  `unknown` bump is **not** skipped — it proceeds past the stale/settled/SHA gates straight into
  the analyser (the expensive step). **Confirms the warning: it fails today.** The pipeline must
  skip `unknown`-typed PRs from the trusted author rather than sending them to the agent.
  A regression test is needed (asserting `unknown` → skipped, never analysed), rather
  than a now-and-rewrite-later test asserting the current (wrong) behaviour.
- [x] **Confirm T9 empirically on petemoore/taskcluster #193** (`c1671f49`). **Done 2026-06-15.**
  The "fix" commit `c1671f49` ("fix: update code for query-string 7.1.1 → 9.3.1 compatibility")
  bundles **300 files, +27,957 / −42,840** — a genuine query-string compat fix touches ~3. **The
  T9 symptom is real and present.** Richer picture than the one-line prediction:
  - The PR's **net** diff (`main`…head) is **clean — 5 files** (changelog, karma.conf.js,
    package.json, Client.js, yarn.lock). The end-tree is correct.
  - The **bump** commit `a8f42104` is *also* bloated (300 files; **289 of them outside
    `clients/client-web`** — `.env`, `.github/*`, `Dockerfile`, `CLAUDE.md`, …). The `-X theirs`
    Phase-0 rebase against a stale base reverted unrelated `main` files **into the bump commit**;
    the fix commit then re-applies `main`'s versions (hence net-clean).
  - So #193's commit-history pollution is dominated by the **rebase**, not solely the stale squash
    base. The narrow T9 fix (post-rebase tip as squash base) is still correct and should land, but
    would not by itself clean #193 (the bump tip it bases off is the polluted commit). This is a
    genuine new problem broader than the Q11-noted `-X theirs` residual — **flagged in
    `docs/questions.md`** (does not block Phase 2).

## Smaller / independent items

- [x] **Gate on required checks only (Q7 → DECIDED).** **PR #5.** `AcceptableGiven` gains a
  `required` set; `Client.RequiredChecks` reads branch protection (cached per base branch per
  cycle) and degrades safely to all-checks on 404/403/error. Empty required set → all-checks (M2).
  Resolves T5. ⚠️ needs an admin-scoped token to take effect (else stays all-checks).
- [x] **No-progress guard → progress metric (Q12 → DECIDED → a, K=8, monotonic floor).** **PR #3.**
  `decideNoProgress` now tracks the lowest blocking-check count (monotonic floor) and gives up when
  it hasn't improved over `MaxNoProgressIterations` (default 8, now a CLI flag) settled attempts.
  Floor beats oscillation; no off-by-one. Subsumes stationary + thrashing. (Resolves T10.)
- [x] ~~**Skip-by-bump-type per repo (Q5).**~~ **PR #4 shipped `--min-bump-to-engage` / `BumpRank`,
  but Q5 is now superseded** — the engage-threshold concept has been removed. Bump type is still
  classified and shown on the dashboard; it is no longer a gate. See Q5 in ALGORITHM.md.
- [x] **Grouped-PR supersession (Q6 → DECIDED → a).** **PR #4.** `FindSupersedingGroup` closes an
  individual when a group covers its package at ≥ its version; a group is never closed by an
  individual (asymmetric). Kills duplicate work / duplicate replacement PRs.
- ~~**Analyser access model (Q9)**~~ — MOOT: Q10 removes the separate analyser; the single agent
  has a live checkout (resolves T7).

---

---

## Phase 6 — Agent pipeline empowerment redesign

**Prerequisite:** Phase 3.2 cluster (single combined agent) should land first or alongside for
items 6.A, 6.B, and 6.D. **6.C (reviewer empowerment) and the agent-log scoping fix in 6.D are
independent of Phase 3.2 and can ship before it — treat them as quick wins.**

**Rollback:** If Phase 6 is deployed and the mdi-react regression test (6.F) fails, the rollback
state is the last green commit before Phase 6 changes. Document and test the rollback procedure
before deploying to GCP.

**Audit reference:** `docs/AGENT_PIPELINE_AUDIT.md` — read this before implementing any item
below. It contains the full findings, the concrete failure scenarios, and the post-implementation
verification checklist that must be used to confirm every gap was closed.

**Regression test (mandatory, final step):** After all Phase 6 items are done and deployed,
process the mdi-react 6.7.0→9.4.0 bump (upstream `taskcluster/taskcluster#6753`, fork
`petemoore/taskcluster`). Rebuild image (`e2`), resync fork (`e1`), watch the PR processed by
the redesigned agent. The output must show verified facts about icon renames — not estimates.
See verification checklist in `docs/AGENT_PIPELINE_AUDIT.md`.

### 6.A — Combined agent: full tool access and autonomous information gathering

- [ ] **Run the combined agent with `--dangerously-skip-permissions`.** Do not enumerate or
  restrict individual tools — the agent is autonomous and uses whatever it needs.
- [x] **Remove `--bare` from the worker command** (`implementation.go:1057`). `--bare` disables
  hooks, skills, and plugins — capabilities installed on the managed GCP instance that the
  agent should be free to use. Blocking them is the same principle violation as restricting
  tools. Remove from both the existing implementation agent and the combined agent. (This can
  land as a standalone fix before the rest of Phase 6.) **Done in 08545ac.**
- [x] **Remove the dead-letter prompt instruction** that tells the analyser to "follow the compare
  URL if the changelog is truncated" — with full tool access the agent fetches what it needs.
  **Done in 08545ac (analyser.go); also done in 6.E below.**
- [ ] **Do not pre-fetch upstream data for the agent.** The agent brief contains only what the
  agent cannot derive itself: PR metadata (number, title, package, old version, new version)
  and the working environment (working directory, clone path, role). Release notes, changelogs,
  codebase snippets — the agent fetches and searches these autonomously. Pre-seeding upstream
  data, even framed as a "hint", is the same pattern we are eliminating. Remove the
  `GetUpstreamInfo` injection from the combined agent's brief entirely.
- [ ] **Specify the combined agent's initial brief.** After Phase 3.2 removes the analyser,
  `BuildImplementationBrief` (`implementation.go:181–207`) can no longer forward
  `analysis.ReviewBody` / `analysis.CodeChanges` — there is no upstream analyser verdict.
  Define what the combined agent starts from: PR metadata only (number, title, package name,
  old version, new version) plus the working environment (working directory, repo clone path,
  role). The agent fetches everything else — diffs, release notes, changelogs — autonomously.
  Define what it is expected to produce (the WHY comment for recommend, or the replacement PR
  body for the fix path).
- [ ] **Combined agent generates its own comment on the `recommend` path.** The approve-comment
  is currently produced from `analysis.ReviewBody` (`orchestrator.go:670`) — output of the
  tool-less analyser. After Phase 6, the combined agent must author this comment itself. The
  comment is the product; its quality is the whole point of the redesign.
- [ ] **Clarify the `confidence` field and low-confidence routing.** `actOnAnalysis` gates on
  `analysis.Confidence == ConfidenceLow`. With a fully-tooled combined agent, low confidence
  should be rare because the agent can verify directly. Decide whether the `confidence` field
  and its routing survive into the combined agent's output schema, or are replaced by a simpler
  "did the agent reach a verdict?" check.

### 6.B — Codebase search: agent-driven, not pre-filtered

- [ ] **Remove the 50-snippet cap as a ceiling.** The agent must be able to search the codebase
  for specific symbol names — not receive a package-name grep capped at 50 lines.
- [ ] **Remove the fixed file-extension list as a gate.** The agent decides what to search.
- [ ] **Move codebase search after upstream data ingestion.** The agent reads the changelog first
  to identify *which specific symbols changed*, then searches for those symbols by name. The
  current pre-filtered approach greps by package name before the agent knows what changed.
- [ ] **Remove `codebase.go` entirely once 6.D lands.** The shallow-clone infrastructure in
  `codebase.go` existed solely to pre-collect data for the tool-less analyser. After 6.D the
  Go program prepares a full clone for the agent from the bare clone; there is no use for a
  separate shallow clone. Remove the whole module, not just the grep step.
- [ ] **Remove the `codebase.AnalyseCodebaseUsage` call in the orchestrator.** After Phase 3.2,
  the combined agent owns the clone; the orchestrator must not call
  `codebase.AnalyseCodebaseUsage` at `processPR` (`orchestrator.go:471`). Remove the call site
  as part of this item — not just the grep logic inside `codebase.go`.

### 6.C — Reviewer empowerment

**Quick win — independent of Phase 3.2.** The reviewer lives in `internal/reviewer/reviewer.go`
and has no dependency on the analyser-removal cluster. Implement this before or in parallel
with Phase 3.2.

- [x] **Run the reviewer as a `claude` subprocess with `--dangerously-skip-permissions` and
  `proc.Dir` set to the repo directory**, matching the `runWorkerTurn` pattern (not a bare
  `Messages.New` call). Do not enumerate individual tools — the reviewer is autonomous.
  **Done in 08545ac (reviewer.go complete rewrite).**
- [x] **Remove the epistemic-hedging patch** from the reviewer prompt ("do not infer the absence
  of changes from the cut-off view") — this is the wrong fix. With full tool access the
  reviewer can run `git diff` itself; there is no size cap and no need to hedge.
  **Done: reviewer.go was rewritten entirely; the hedge is gone.**

### 6.D — Repo checkout, shared state, and directory lifecycle

**Design model:** The Go program owns all infrastructure. Agents own only their designated
working directory. Agents do not manage git infrastructure, create long-lived directories, or
clean up after themselves globally — the Go program handles all of that. What can be enforced
programmatically must be, because code does not forget or disobey.

**Bare clone (Go program — permanent, not per-PR):**
The Go program maintains `sweeper-base/<owner>-<repo>.git`, a bare clone containing the full
repo history, all branches, and all tags. Fetched at the start of each scan cycle. Never
modified by agents — agents are told its path and told explicitly not to modify it. It is the
source used to prepare per-PR clones cheaply without hitting the network.

**Per-PR working directory and clone (Go program — created and destroyed programmatically):**
For each PR, the Go program:
1. Creates `sweeper-data/pr/<owner>-<repo>/pr-<N>/` as the agent's working directory.
2. Runs `git clone <bare-path> <workdir>/repo` to produce a **full git clone** (not shallow)
   inside it — the agent's ready-to-use checkout.
3. Passes both paths to the agent in its brief.
4. Deletes `<workdir>` when the PR disappears from the open-PR list.

Standard brief template for every agent:
```
Working directory: <workdir>
Repo clone:        <workdir>/repo/   [full clone, ready to use]
Bare clone:        <bare-path>       [do not modify; clone from it if you need a fresh copy]

You have full tool access and are fully autonomous. Work within <workdir> where possible.
If you must do anything outside it, clean up after yourself. This working directory will
remain on disk while [the relevant PR] is open.
```

**Impl → reviewer handoff (the clone is the interface):**
The reviewer is handed the **same `<workdir>`** as the implementing agent. The commit history in
`<workdir>/repo/` is the handoff — the reviewer reviews what was committed, not a metadata
summary. The implementing agent is told:
```
Your commits will be reviewed — write clean, self-explanatory commit messages. If you
incorporate review feedback across multiple turns, clean up the commit history (squash or
fixup as needed) before re-submitting. The reviewer sees exactly the commits in this repo.
```
The reviewer's brief includes the branch name and HEAD SHA but does not re-clone; it works in
`<workdir>/repo/` directly. For re-invocations, the brief must include the turn number so the
reviewer knows it is reviewing a revision, not a fresh submission.

---

- [x] **Bare clone lifecycle.** Create `sweeper-base/<owner>-<repo>.git` as a bare clone on
  first use. Re-fetch (including tags) at the start of each scan cycle, **before any PR
  goroutines are launched** — the orchestrator dispatches PRs concurrently
  (`orchestrator.go:244–257`); a fetch racing with active agent processes is a race condition.
  If `git fetch` fails (disk full, network error, `.git` corruption), delete and do a full
  re-clone rather than propagating the failure. Scoped per-repo — never shared across repos.
  **Done in 08545ac (`ensureBareClone` in orchestrator.go; fetch before goroutine loop).**

- [x] **Per-PR working directory and clone.** Go program creates
  `sweeper-data/pr/<owner>-<repo>/pr-<N>/` and runs `git clone <bare-path> repo` inside it.
  Both paths included in every agent brief. Canonical schema used consistently throughout
  codebase and documentation — owner+repo in the path ensures PRs from different repos with
  the same number never collide. **Done in 08545ac (`canonicalWorkdir`, `DataDir` config field).**

- [x] **Stale directory detection.** If `<workdir>` already exists at run start (crash residue),
  the Go program deletes it and recreates it — never silently reuses potentially dirty state.
  No branch ref complications: agents work in their own clones and do not add branches to the
  bare clone. **Done in 08545ac.**

- [x] **Agent log files must be PR-scoped, not shared.** Currently all agent logs go to
  `os.TempDir()/sweeper-agent-logs/` with no PR scoping. Multiple concurrent agents interleave
  logs in the same directory and the files are never cleaned up. Fix: write logs under
  `<workdir>/` so they are removed when the working directory is deleted on PR close. This
  closes both the collision problem and the disk growth problem. **Done in 08545ac.**

- [ ] **All per-PR resources live under one PR-keyed root — nothing hidden outside it.**
  Every resource the Go program creates for a PR must live under
  `sweeper-data/pr/<owner>-<repo>/pr-<N>/`:
  - **Repo clone** (`<workdir>/repo/`): lives inside the working directory. ✓
  - **Log files**: moved under `<workdir>/` (see above).
  - **Claude CLI session files**: the pipeline pins `--session-id <UUID>`; the CLI stores
    session transcripts under `~/.claude/projects/<hash>/` by default — not under `<workdir>`.
    These accumulate indefinitely. **Prerequisite:** investigate the Claude CLI session storage
    format before committing to a cleanup mechanism. The path uses a hash of the project
    context, not the session ID; finding files by session ID may require shell investigation.
    Either redirect storage into `<workdir>`, or record the session ID in the DB and delete
    files explicitly during the closed-PR sweep.
  - **Remote git branches** pushed to GitHub (e.g. `auto/fix/<package>-<version>`): not deleted
    by the closed-PR sweep. Decide whether remote branch cleanup is in scope for Phase 6 or
    documented as a known out-of-scope resource.
  - **SQLite DB rows** (`pr_progress`, `created_prs`): `Reap()` must be triggered by the
    closed-PR sweep so the DB stays in sync with the open-PR list.

- [x] **PR-keyed assets are cleaned up when the PR is closed — one trigger, complete cleanup.**
  On every orchestrator scan cycle, after fetching the open-PR list, the Go program sweeps
  `sweeper-data/pr/<owner>-<repo>/` and deletes any `pr-<N>/` directory whose PR is absent
  from the list. The same sweep triggers `Reap()` for DB rows.
  **Invariant: deleting `sweeper-data/pr/<owner>-<repo>/pr-<N>/` plus calling `Reap(N)`
  leaves zero resources associated with PR N on the host.**
  **Caveat — GaveUp path:** `GaveUp` does not close the original dependabot PR (only the
  success path closes it). A GaveUp PR stays in the open-PR list; its working directory is
  not deleted until the PR is manually closed or merged. Known gap — documented.
  **Done in 08545ac (`reapClosed` in orchestrator.go; GaveUp caveat in comment).**

- [x] **Update the web dashboard's log-serving endpoint.** Moving logs under the PR-keyed root
  requires updating the web API. The web process uses `--log-dir` / `SWEEPER_LOG_DIR`
  (`implementation.go:316`) to serve the agent-log endpoint. Update it to construct the new
  per-PR log path from the canonical schema. **Done in 08545ac (`WithDataDir` in web/server.go).**

- [ ] **Resolve `manualRebase` sequencing vs PR-keyed root.** `manualRebase` creates its own
  `os.MkdirTemp("", "sweeper-rebase-*")` with `defer os.RemoveAll` and runs *before*
  `p.workdir` is created (`implementation.go:415–433`). Either create the PR-keyed root before
  calling `manualRebase` (so the rebase can use a subdirectory of it), or keep the rebase temp
  dir as a documented short-lived exception outside the PR root.

- [x] **Same-package collision is prevented by the staleness gate, not by directory isolation.**
  `FindNewerPRForPackage` runs in `processPR` Step 1, before any PR reaches the pipeline, so
  only the higher-version PR proceeds. Document this explicitly — directory isolation is the
  backstop, not the primary defence. **Done in 08545ac (comment near `FindNewerPRForPackage`).**

- [ ] **Impl → reviewer brief.** The reviewer is handed the same `<workdir>` as the implementing
  agent. Its brief must include:
  ```
  Working directory: <workdir>
  Repo clone:        <workdir>/repo/
  Branch:            <name>
  HEAD:              <sha> ("<commit message>")
  Turn:              <N>  (1 = first review; 2+ = reviewing revised implementation)
  ```
  The reviewer works directly in `<workdir>/repo/` — no re-clone needed.

### 6.E — Agent prompts: role, purpose, and workflow context

Every agent prompt must be reviewed against the Agent Empowerment Principle in `docs/PRINCIPLES.md`.
Two concrete changes are already known from the audit — these must land as part of this item:

- [x] **Remove the dead-letter "follow the compare URL" instruction** from `analyser.go:46–47`.
  Remove it entirely — the combined agent fetches what it needs autonomously; no instruction
  is needed to tell it to do so. **Done in 08545ac.**
- [x] **Remove the epistemic-hedging patch** from `reviewer.go:187–194` ("Do NOT infer the absence
  of any change from this cut-off view; if the visible portion is insufficient to judge, say so").
  After 6.C, the reviewer has Bash access and can run `git diff` itself. Remove the hedge and
  replace with: "Use `git diff` to read the full diff; there is no size cap."
  **Done: reviewer.go was rewritten entirely; the hedge is gone; the `git diff` instruction is
  in the new brief's tool section.**
- [ ] Each prompt explains the agent's role, *why* that role exists, and how it fits the overall
  workflow (the context helps the agent reason about edge cases it wasn't explicitly instructed on).
- [ ] No prompt contains instructions the agent structurally cannot follow.
- [ ] No prompt substitutes epistemic hedging for actual tool access.

### 6.F — Regression test (mdi-react)

This is the end-to-end verification that the principles fixed the problem, not just the design.

- [ ] Phase 6.A–6.E implemented and deployed (`e2` — GCP deploy).
- [ ] Fork resynced (`e1`) — the mdi-react bump (`petemoore/taskcluster` mirror of
  upstream `taskcluster/taskcluster#6753`) is present as a fork PR.
- [ ] If upstream `taskcluster/taskcluster#6753` is no longer reproducible in the fork at test
  time, create a synthetic mdi-react bump PR at the same version range as the test vehicle.
- [ ] Watch the PR processed. Verify using the agent log (`pr-<N>-agent.jsonl`):
  - A `tool_use` event of type `WebFetch` appears with a URL pointing to the MDI changelog,
    GitHub releases, or npm registry for mdi-react in the 6.7.0→9.4.0 range — not just
    the package homepage
  - A Bash `tool_use` event appears searching for specific icon names (e.g.
    `grep -r 'ClockIcon'`), not just the package name
  - The output contains verified facts ("icon `X` was renamed to `Y`; the codebase uses
    `X` in `path/to/file.tsx` — this must be updated") or a verified absence grounded in a
    specific search result ("grep found no matches for `X`"), not absence of evidence in a
    pre-filtered list
  - The comment cites a specific source (URL or release notes section) for the icon rename
    information
  - No "unlikely", "probably", or "common/well-established" language in the output
  - The recommendation is grounded in what was actually checked, not estimated

---

## UI / dashboard (tracked in backlog memory too)

- [ ] **Link the sidebar PR number to the actual GitHub PR.** `models.DependabotPR.URL` exists but
  isn't persisted/exposed; plumb through `PRProgress` + web API.
- [ ] **Show the dependabot↔sweeper pairing (`#183 / #204`)** in PR boxes, with sidebar navigation
  to either and out to GitHub. Needs the reverse link from T12/Q14.

---

## Open questions still needing a maintainer decision

**None — all decided.** Q1–Q8 and Q10–Q15 are decided; Q9 is moot (the analyser is removed by
Q10). Full reasoning for each is in `docs/ALGORITHM.md`. The remaining work is implementation +
the two validation experiments above. Items may surface smaller decisions during implementation;
raise those as they come up.
