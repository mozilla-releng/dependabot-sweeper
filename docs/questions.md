# Open questions for a maintainer

Questions raised during autonomous implementation that need a maintainer's decision before the
related work can proceed. Append below; keep working on everything else in the meantime.

Format per entry:
- **[area / WORKPLAN item]** — the question, the options considered, and what you'd recommend.
  What is blocked on it, and what you did instead while waiting.

- **[Phase 0 / Q11 / T9 — `-X theirs` rebase reverts unrelated `main` files into the bump commit]**
  **The question:** Q11 noted a residual — "`-X theirs` lockfile-conflict resolution can be subtly
  wrong (rare, CI-caught)." The Phase-0 validation on petemoore/taskcluster #193 shows this residual
  is **broader and more impactful than documented**: the bump commit `a8f42104` reverted **289 files
  outside `clients/client-web`** (`.env`, `.github/*`, `Dockerfile`, `CLAUDE.md`, …) back to stale
  versions, because the branch was based on an old `main` and `-X theirs` favours the branch side on
  every conflicting file. In #193 the *net* PR diff stayed clean (5 files) only because the fix
  commit happened to re-apply `main`'s versions — so **CI would not catch it** (the net tree is
  correct), yet both commits a reviewer reads are 300-file noise, and the mechanism is fragile (a
  file the fix agent didn't touch could silently stay reverted).
  **Options:** (a) Proceed with the decided narrow T9 fix only (post-rebase tip as squash/curate
  base) and accept the rebase-pollution residual as before. (b) Additionally harden the rebase —
  e.g. drop `-X theirs` in favour of a conflict-aware strategy, or revisit reconstruct-from-base
  (Q11 option B) for badly-stale branches. (c) Detect the pollution (bump commit touching files far
  outside the bump's directory/manifest) and flag for human instead of producing a noisy PR.
  **Recommendation:** **(a) for now** — land Phase 2's narrow T9 fix as decided (it is correct and
  necessary regardless), and **defer (b)/(c) to a maintainer decision.** The narrow fix does not
  fully clean a #193-style branch (it bases off the polluted bump tip), but relitigating Q11's
  inherit-and-rebase decision is out of scope for autonomous work.
  **Blocked:** nothing — Phase 2 proceeds. This is an FYI + a flagged follow-up, not a blocker.
  **What I did instead:** implemented the Phase 2 narrow fix as planned and recorded this finding.

- **[Phase 3 — within-phase sequencing of autonomous work]** **Not a question — a heads-up on the
  order I took, for transparency.** The WORKPLAN lists Phase 3 sub-steps 3.0→3.8. The lynchpin is
  3.2 (remove the separate analyser → single agentic step), and 3.0/3.2b/3.3/3.6/3.8 are tightly
  coupled to it (they collectively define + implement the new state machine). That cluster is a
  large, interrelated rework that is risky to leave half-finished (a reconciled `spec.go` that
  doesn't match the code is *worse* than the current drift). To bank clean, independently-mergeable
  value first, I implemented the **self-contained, decided** Phase-3 items that do NOT depend on the
  analyser removal — **Q12 (no-progress metric), Q5/Q6 (min-bump skip + group supersession), Q7
  (required-checks gating)** — each as its own draft PR. The analyser-removal cluster
  (3.0/3.2/3.2b/3.3/3.6/3.8) is then tackled as a coherent unit (or left with a written plan if I
  run out of time), rather than as risky partial PRs. **Nothing is blocked**; the order within
  Phase 3 is the only deviation, and these items don't rest on 3.2.
