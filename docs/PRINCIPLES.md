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

3. **Idempotency is a cost-safety invariant.** The tool runs on a scan cycle; "no change since last
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

   **Infrastructure is the program's job, not the agent's.** The Go program creates working
   directories, prepares git clones, manages the bare clone, and cleans up on PR close. Agents are
   not asked to do any of this — code does not forget instructions or make mistakes the way an
   agent might. The rule: *if something can be enforced or performed programmatically, it must be*.
   Agents are autonomous over their domain (reasoning, research, implementation) but do not manage
   their own lifecycle. This is the complement to the agent empowerment principle: agents get full
   tool access and autonomy over their work; the program handles the environment they work in.

## Agent empowerment principle

**This is the most important design constraint for any agent prompt, pipeline stage, or
information-gathering step.** Violating it is the root cause of guesswork, false confidence,
and unnecessary human escalation.

### The rule

**Give agents the tools, context, and autonomy to collect the information they need. Do not
pre-filter information on an agent's behalf and pass in a summary.**

An agent that is told its role, given the full relevant context of the system it operates in, and
equipped with tools to independently gather what it needs will always outperform one that receives
a curated, pre-filtered subset of information assembled by an upstream component. The upstream
component cannot know exactly what the agent will need; the agent, given autonomy, can.

### What this means in practice

- **Agents must receive their role and its purpose.** Every agent prompt must explain what this
  agent does, *why* that role exists, and how it fits into the overall workflow. An agent that
  understands the concerns of the people and systems it interacts with will reason better about
  edge cases.

- **Agents must have tools to gather information, not just receive it.** If an agent needs to
  verify that icon names haven't changed, it must be able to fetch the upstream rename history
  itself — not receive a summary that was assembled upstream and may be incomplete. If it needs to
  search the codebase, it must be able to run that search, not receive a capped snippet list that
  may have missed the critical usage.

- **Never reason on behalf of an agent about what it might need.** The pattern of "collect X
  because the agent will probably want X" is always worse than "give the agent access to X and let
  it decide." Pre-filtering creates manufactured uncertainty: the agent is forced to reason under
  information constraints that don't reflect the actual problem.

- **Programmatic gates and independent reviews are still essential.** Empowering agents does not
  mean removing oversight. The orchestrator must still enforce CI gates programmatically, and
  independent review agents must still validate work. The distinction is: gates enforce *outcomes*
  (did CI pass? did the reviewer approve?), not *inputs* (what information did we decide to give
  the agent). Don't try to govern quality by restricting information.

- **Autonomy over domain; infrastructure from the program.** Agent empowerment is about giving
  agents what they need to do their *reasoning and implementation* work — not about agents
  managing their own environment. Directory creation, clone preparation, and cleanup on PR close
  are performed by the Go program, not delegated to agents. When an agent needs a checked-out
  repo, the program prepares one and tells the agent where to find it. The agent is free to
  re-clone if it needs to, but it should never be *responsible* for infrastructure that the
  program can manage more reliably.

- **Unverified claims are a design smell, not a prompt problem.** If an agent is saying "this is
  unlikely" instead of checking, the fix is not to add "flag your guesses" to the prompt. The fix
  is to give the agent the means to check. Patching prompts to require epistemic hedging turns one
  failure mode (false confidence) into a different, worse one (unnecessary human escalation for
  things the agent could have resolved autonomously).

### The failure pattern to watch for

> *"I know some icons probably changed upstream, and the codebase probably uses some of them, so
> I have to estimate whether mine changed."*

This is a forced guess under manufactured uncertainty. It is always a symptom of one or both of:
1. The agent was not given a means to fetch the upstream rename data directly.
2. The agent was given a capped/pre-filtered view of the codebase instead of full search access.

When you see this pattern — in a PR comment, in a review, in a test result, anywhere — treat it
as a design bug and ask: what information or tool was missing?
