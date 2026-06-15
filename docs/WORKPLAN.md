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

- [ ] **Phase 0 — Validation experiments** (read-only; de-risk before changing anything).
  Confirm T9 on #193; run the empty-commit-PR test. See *Validation experiments* below.
- [ ] **Phase 1 — Safe independent fixes** (low risk, no design dependencies). T4 stale comment;
  T6/T6a **reporting-noise fix only** (no-op cycle records nothing) — the transition guard is
  deferred to Phase 3 per review C2; dead fields (`ReviewVerdict.Summary`, `BudgetSpent`).
- [ ] **Phase 2 — T9 narrow fix** (squash base = post-rebase branch tip; **store the SHA on a
  struct field** so Q13's curate and the reviewer-diff can reuse it — review M4; **also return it
  in `RunResult`** so the orchestrator's gave_up path can record the outcome against it, not the
  scan-time `pr.HeadSHA` — review N4/MAJOR-1; leave all other `recordOutcome` calls on `pr.HeadSHA`).
  Small but serious.
- [ ] **Phase 3 — Core rework** (large, interrelated; sequence as its own PRs in this order):
  0. Reconcile `spec.go` to the post-Q10 state machine FIRST (review C2), so the transition guard
     is authored against the final graph.
  1. Required-checks gating (Q7) — the new CI bar; empty required set → fall back to all-checks (M2).
  2. One-agent step; remove the separate analyser (Q10) — the structural change.
  2b. Q8 transition guard (now that spec.go is reconciled) — cost-safety-critical.
  3. No-attribution + silent-draft + approve-only-when-green (Q1/Q2/Q3/Q4).
  4. Skip-by-`min-bump` config + `unknown`→skip (Q5); group↔individual supersession (Q6).
  5. No-progress progress-metric, K=8 (Q12).
  6. Agent curates its own history (Q13).
  7. Sweeper-PR naming + DB-record exclusion + pairing attribute (Q14).
  8. Worker-authored justification, private-through-review, posted on approval (Q15).
- [ ] **Phase 4 — UI items.** PR→GitHub link; `#183 / #204` pairing display.
- [ ] **Phase 5 — Docs.** Add the two-PR-type lifecycle section; fold the North Star into
  `docs/PRINCIPLES.md`. (`spec.go` reconciliation moved to Phase 3.0 per review C2.)

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

## Confirmed bugs

- [ ] **T9 — Squash bundles unrelated `main` changes into the "fix" commit.** Squash resets to
  the stale scan-time `pr.HeadSHA` after a Phase-0 rebase. **Fix (decided):** reset to the branch
  tip captured right after `checkout -b branch pr.HeadRef`, before the agent runs. (Q11 kept
  inherit-and-rebase, so this narrow fix is the chosen one.) **Store the SHA on a struct field**
  (review M4) — it's the single canonical base for the squash, the Q13 curate, AND the reviewer
  diff (which currently uses the ref *name* `origin/<HeadRef>`, same stale-ref hazard). Serious.
  Confirm empirically on petemoore/taskcluster #193 (`c1671f49`): bloated commit's parent should
  equal the pre-rebase head.
- [ ] **T6 / T6a — Decision graph not enforced + idempotent skips pollute history. (Q8 → c: both,
  SPLIT across phases — review C2.)** (1) **Phase 1:** reporting-noise fix — a no-op cycle records
  **nothing**: remove the `pending` re-stamp in `Run`'s pre-populate loop and at the top of
  `processPR`, **and the skip-path terminal re-stamp** (`orchestrator.go:347-357` — review N1);
  the dashboard reads the stored stage from the row. (No `finalized→finalized` self-loop is then
  emitted, so the guard needs no self-loop exception.) (2) **Phase 3 (after spec.go reconciled):**
  runtime guard — `Report(stage)` validates against the post-Q10 graph, rejects/loud-logs illegal
  transitions; collapse non-stage nodes following **both decision AND back edges** (review N2 — or
  it rejects legal resume loops). Cost-safety-critical.
- [ ] **T4 — Stale `spec.go` comment** on the `gave_up` node still claims it routes to
  `flagged_human` (Bug #24 era). Behaviour is correct; just fix the comment.
- [ ] **T12 — Re-ingestion risk (Q14 → DECIDED):** exclude the tool's own PRs via a DB record of
  what it created (never a branch-name/title heuristic — branch names are attacker-spoofable).
  ⚠️ **Must be a reap-exempt table (review C1)** — NOT a `pr_progress` column, which `Store.Reap`
  wipes each cycle for non-accepted-author PRs (our own replacement PRs in prod). In-scope stays
  author-filter-only. Cost-safety-critical (re-admitted own-PR = runaway agentic step).
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
- [ ] **Sweeper-PR naming + tracking (Q14 → DECIDED).** (1) Title = dependabot title with type
  swapped `build`→`fix`; prepend `fix(deps): ` if no parseable prefix (replaces verbatim
  `pr.Title` copy). (2) Record every PR the tool creates in the DB and permanently exclude those
  from scans — **not** a branch-name/title heuristic (branch names are attacker-spoofable);
  in-scope stays author-filter-only. (3) Sweeper PR is an **attribute on the dependabot PR's row**
  (add reverse link), not a first-class entity. (4) Add a doc section modelling the two-PR-type
  lifecycle. (T12, Q14)

---

## Validation experiments

- [ ] **Confirm a non-bump PR from the trusted author exits early.** Create a PR from the
  configured author with an arbitrary change (e.g. an empty commit / no recognisable bump title)
  and trace it through the workflow. Expected: it never reaches an agent — it's skipped at
  bump-classification. ⚠️ Current code processes unparseable titles as bump type `unknown`, so this
  likely *fails* today; Q5's `min-bump-to-engage` skip must treat `unknown`/below-major as skip.
  Validates that author-filter-only in-scope (Q14) is safe.
- [ ] **Confirm T9 empirically on petemoore/taskcluster #193** (`c1671f49`): the bloated commit's
  parent SHA should equal the pre-rebase head (the stale scan-time `pr.HeadSHA`).

## Smaller / independent items

- [ ] **Gate on required checks only (Q7 → DECIDED).** Read the repo's required-status-checks set;
  treat only those as blocking ("CI passing" ≡ "required checks passing"). Resolves T5. **Empty
  required set → fall back to all-checks** (review M2 — else we'd recommend-merge an all-red PR).
  Needs branch-protection read scope on the token; cache the required set per-base-branch per cycle.
- [ ] **No-progress guard → progress metric (Q12 → DECIDED → a, K=8, monotonic floor).** Track the
  lowest failing-*required*-check count seen so far; give up when that floor hasn't improved over
  the last **8** attempts (configurable). Floor (not sliding window) so up-down oscillation can't
  game it (review M3). Don't inherit the existing `prevBlocking` off-by-one. Subsumes stationary +
  thrashing. `MaxImplIterations` (30) stays the ceiling. (Resolves T10.)
- [ ] **Skip-by-bump-type per repo (Q5 → DECIDED → a).** Per-repo `min-bump-to-engage` config
  (default `major`); skip anything below it. Replace the current hardcoded "skip only patch."
  Record skipped-out-of-policy bumps on the dashboard with a note.
- [ ] **Grouped-PR supersession (Q6 → DECIDED → a).** Decompose grouped PRs into member
  `(package, version)` pairs and run semver comparison against individual PRs. A group supersedes
  (closes) an individual when it covers that package at ≥ version; an individual never closes a
  whole group. Kills duplicate work / duplicate replacement PRs.
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
