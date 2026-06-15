# Dependabot Sweeper — Rework Tracker

Living checklist for the rework surfaced in the algorithm review. The **reasoning** for every
item lives in `docs/ALGORITHM.md` (referenced as T# = tension, Q# = open question). This file is
the **status tracker** — check items off as they land. When an item is done, mark `[x]` and note
the commit/PR.

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done · `[?]` needs a maintainer decision first.

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

- **NORTH STAR (drives all architecture).** For each engaged major-bump PR, produce one of two
  well-justified outcomes: (1) **no change needed → comment on the existing dependabot PR** with a
  concrete WHY (required checks green + agent certain no change needed); or (2) **changes needed →
  open the tool's own replacement PR, based on the dependabot one** — same bump + one or more
  additional commits doing the extra work, with a justification of why those changes are
  correct/needed/appropriate and how it's the right solution for this bump, grounded in upstream
  docs/changelogs/diffs. ("Based on" = supersedes it and contains the same bump, *not* literal
  commit reuse.) The justification *is the product* — a human reads a short specific rationale and
  merges with confidence. Last resort only: a concise human-attention flag. (Full text:
  `docs/ALGORITHM.md` → North Star; canonical home `docs/PRINCIPLES.md`.)

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
- [x] **Phase 1 — Safe independent fixes** (low risk, no design dependencies). **PR #1 (draft).**
  T4 stale comment ✓; T6/T6a **reporting-noise fix only** (no-op cycle records nothing) ✓ — the
  transition guard is deferred to Phase 3 per review C2. Dead fields (`ReviewVerdict.Summary`,
  `BudgetSpent`) **deliberately deferred** to their owning phases (Summary → Q15/Phase 3.8 replaces
  it; BudgetSpent → billing backlog) rather than removed-then-re-added.
- [x] **Phase 2 — T9 narrow fix** (squash base = post-rebase branch tip ✓; **stored on a struct
  field** `Pipeline.bumpTipSHA` reused by the squash and the reviewer-diff — review M4 ✓; **also
  returned in `RunResult.TipSHA`** and the orchestrator's gave_up path records the outcome (+ sticky
  comment marker) against it via `terminalSHA()` — review N4/MAJOR-1 ✓; all other `recordOutcome`
  calls left on `pr.HeadSHA`). **PR #2 (draft).** ⚠️ Phase-0 surfaced a `-X theirs` rebase-pollution
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
  4. [x] Skip-by-`min-bump` config + `unknown`→skip (Q5); group↔individual supersession (Q6). **PR #4.**
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
- [~] **Phase 5 — Docs.** `docs/PRINCIPLES.md` **created** (resolves the dangling reference that
  ALGORITHM.md/memory pointed at; North Star + non-negotiables at current-state UX level, mechanism
  left to ALGORITHM.md). The **two-PR-type lifecycle section is deferred** to land with Q14's merge
  (PR #6): documenting the `fix(...)` naming + `created_prs` as current state before that PR merges
  would describe not-yet-current behaviour. (`spec.go` reconciliation is in the Phase 3.2 cluster.)

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
  (4) doc section — **Phase 5 (pending).** (T12, Q14)

---

## Validation experiments

- [x] **Confirm a non-bump PR from the trusted author exits early.** **Done 2026-06-15 (code
  trace — conclusive; no live PR needed).** Traced `GetDependabotPRs` → `processPR`: an
  unparseable title from an accepted author takes the `default` branch in `GetDependabotPRs`
  (`client.go:156-161`) → `BumpUnknown`, `PackageName = title`. In `processPR` the only early
  bump-type skip is `pr.BumpType == BumpPatch && !pr.Grouped` (`orchestrator.go:236`), so an
  `unknown` bump is **not** skipped — it proceeds past the stale/settled/SHA gates straight into
  the analyser (the expensive step). **Confirms the warning: it fails today.** Q5's
  `min-bump-to-engage` skip must treat `unknown`/below-major as skip (Phase 3.4); `maxGroupedBump`
  already ranks `unknown` lowest, so `min-bump = major` naturally excludes it. A regression test
  is authored alongside the Phase 3.4 skip (asserting `unknown` → skipped, never analysed), rather
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
- [x] **Skip-by-bump-type per repo (Q5 → DECIDED → a).** **PR #4.** `--min-bump-to-engage`
  (default `major`); `models.BumpRank` skips anything below it, incl. `unknown` (closes the Phase-0
  finding). Recorded on the dashboard (`ActionSkippedPolicy`).
- [x] **Grouped-PR supersession (Q6 → DECIDED → a).** **PR #4.** `FindSupersedingGroup` closes an
  individual when a group covers its package at ≥ its version; a group is never closed by an
  individual (asymmetric). Kills duplicate work / duplicate replacement PRs.
- ~~**Analyser access model (Q9)**~~ — MOOT: Q10 removes the separate analyser; the single agent
  has a live checkout (resolves T7).

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
