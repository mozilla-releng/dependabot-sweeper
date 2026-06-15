# Principles

The top-level principles that drive every design decision in this tool. They are the *why*;
`docs/ALGORITHM.md` is the *how* (the decision algorithm), `docs/ARCHITECTURE.md` is the system
design, and `docs/WORKPLAN.md` tracks in-flight changes. Where a principle and the current code
disagree, the principle wins and the gap is a bug to fix.

## North Star — emulate a careful human reviewer

The tool exists to **reduce the human attention a repo's dependency bumps require — never to
increase it.** Human attention is the budget being spent; the net effect of the tool must be less
of it, not more. It behaves like a careful human reviewer: triage first, fix and justify when
confident, stay concise or silent when not, and never guess.

For each engaged dependency-bump PR it produces exactly one of two **well-justified** outcomes:

1. **No change needed → comment on the existing dependabot PR**, explaining *specifically why* the
   bump is safe to merge as-is (what changed upstream and why it doesn't affect this codebase).
   Required checks must be green **and** the upstream change must have been reasoned about — green
   CI alone is not a sufficient reason.
2. **Changes needed → open the tool's own replacement PR**, based on the dependabot one (same bump
   plus the necessary additional commits), with a justification of *why those changes are correct
   and the right solution for this bump*, grounded in the upstream docs / changelogs / diffs.

Only as a genuine last resort: a **concise, purpose-built human-attention flag**. If there is no
useful, high-confidence insight to add, the tool says nothing.

**The justification is the product.** A human reads a short, specific rationale and merges with
confidence — they should not have to re-derive the analysis. A justification that just restates
the diff, or that is vague, has failed.

## Non-negotiable constraints

These hold for every code change, agent prompt, and GitHub interaction.

1. **Propose, never approve.** The tool posts comments and opens PRs; it must *never* submit a
   native GitHub APPROVE review (which could satisfy branch protection or trigger auto-merge and
   land code with no human in the loop). This is structural: there is no review-submission method
   in the codebase, and it must stay that way.

2. **The PR is the only UI for the repo's maintainers.** For the people whose PRs are acted on, the
   tool communicates *only* through the PR — no email, no Slack, nothing duplicated on re-runs. (The
   tool's *operators* have a separate admin dashboard; see `docs/DASHBOARD.md`. That is an
   observability surface, not a channel to maintainers.)

3. **Idempotency is a cost-safety invariant.** The tool runs on a cron cycle; "no change since last
   cycle" must be a true no-op — no duplicate comments, reviews, or actions, and no stage-history
   noise. Crucially, every engaged PR triggers an expensive agentic step, so **"never process the
   same PR twice" is the only thing bounding spend.** A state-machine bug that re-admits an
   already-processed PR is a runaway-cost incident, not a cosmetic glitch. This is why the
   transition guard, the never-re-ingest-our-own-PR exclusion, the silent-draft terminal outcome,
   and the already-processed-SHA skip must all be airtight.

4. **"CI passing" means "required checks passing."** The repo owner's required-status-checks set is
   the merge bar, so it is the tool's bar. A non-required red check never blocks; a required red
   check — even one red on the base branch — is more work to fix, not an excuse. (When the required
   set can't be read, the tool falls back to gating on all checks rather than vacuously passing.)

5. **Provider-agnostic CI.** The tool reads only the generic GitHub Checks API — no Taskcluster-,
   GitHub-Actions-, or any provider-specific API in `internal/` or `cmd/`. Litmus:
   `grep -ri taskcluster internal/ cmd/` finds nothing.

6. **No internal tool name in public output.** GitHub comments, reviews, and PR bodies never mention
   the tool's internal name. The tone is indistinguishable from a careful human reviewer.

7. **Multi-agent with programmatic gates.** A lean Go orchestrator owns the CI gate and session
   lifecycle; the agentic worker iterates with persistent context. The orchestrator — not the agent
   — decides when CI is acceptable, and an independent reviewer checks the work on the fix path.
   Complex upgrades take multiple turns; nothing is one-shot.
