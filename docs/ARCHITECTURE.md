# Architecture

## Overview

Dependabot Sweeper runs as a **periodic cron worker** against a GitHub repo's open dependabot
PRs. Each cycle it triages every open PR, taking one of three actions: approve it silently,
drive an implementation agent to fix code breakage and open a replacement PR, or flag it for
human attention with a concise explanation. The cycle repeats; the PR is the only UI.

## Execution model

The worker is **level-triggered and stateless across cycles**. Each cycle reads current GitHub
state and decides what to do now — it does not rely on what it observed in a prior cycle.
A SQLite database records outcomes (per PR, per head SHA) so the worker can skip PRs it has
already acted on without re-querying them. PRs with unsettled CI are skipped and revisited on
the next cycle.

This model has two hard consequences:

1. **Idempotency is mandatory.** Re-running a cycle must not re-post reviews or comments.
   The worker checks whether it has already produced the equivalent outcome for the current
   head SHA before acting on any PR.
2. **The PR is the only user-facing output.** There is no email, no Slack message, no
   notification outside the PR itself. A "flag for human" disposition is rendered as a single
   sticky status comment on the PR (`<!-- sweeper:status -->`), created once and edited in
   place on subsequent cycles — never duplicated.

## Multi-agent pipeline

Each PR that needs code changes goes through a gated pipeline:

```
Orchestrator
  │
  ├─ Analyser (Claude API)
  │    Reads: PR diff, upstream changelog, codebase usage grep, CI status
  │    Produces: structured JSON — recommendation + breaking changes + guidance
  │
  ├─ Implementation worker (Claude Code subprocess)
  │    Receives: analysis brief + target branch
  │    Does: fix → commit → push → open draft PR → exit
  │    Constraint: bounded by time cap + per-PR spend cap
  │
  ├─ CI gate (orchestrator-owned, not worker-owned)
  │    Polls until CI settles; resumes worker with failure logs if needed
  │    Bounded by MaxImplIterations + MaxImplTime
  │
  └─ Reviewer (Claude API)
       Reads: original analysis + implementation diff + CI status
       Produces: approve or request_changes
```

The worker is a **bounded turn**: it fixes, pushes, opens a draft PR, and exits. It never
waits for CI. The orchestrator owns the CI gate entirely — it polls, evaluates, and resumes
the worker session with failure context if CI fails on a bump-related check. This separation
ensures the worker's context stays focused on the code problem and the orchestrator's context
stays focused on the pipeline logic.

On resume, the same worker session is continued (via `claude --resume <session-id>`), so the
worker accumulates context across CI-fix iterations without starting from scratch.

## CI perception

The worker reads CI exclusively via the **generic GitHub Checks API** — no provider-specific
APIs (no Taskcluster queue, no GitHub Actions job-logs API). This makes the tool work for any
CI provider that reports check runs to GitHub.

For failing checks, the tool reads `output.summary` + `output.text` from the check run. In
practice this carries enough log tail (typically 8+ KB) to diagnose the failure. Provider-
specific log access (richer logs from Taskcluster, GHA, etc.) is a future capability layer,
not part of the core tool.

**Settledness** — a PR's CI is settled when every check is either terminal (completed with any
conclusion) or stale (pending for longer than the staleness threshold, default 12h). The tool
uses the check's `StartedAt` timestamp as the staleness clock, falling back to the PR's head
commit timestamp when no check timestamp is available. A PR with unsettled CI is skipped and
revisited on the next cycle; the early-failure-while-siblings-still-run case is handled
correctly (the PR is not triaged prematurely).

**AcceptableGiven** — the tool distinguishes between failures that are the tool's
responsibility and those that are not:
- Failures on the base branch (pre-existing before the bump) → not blocking
- Failures in the `--ignore-check` list → not blocking
- Stale checks in the `--ignore-check` list → not blocking
- All other failures → blocking

The same `AcceptableGiven` function governs both the pre-implementation routing decision
(should the tool attempt a fix at all?) and the post-implementation gate (is CI good enough
to open the replacement PR?).

## Decision graph

The full PR disposition state machine is specified declaratively in `internal/workflow/` as a
Go package. The spec asserts that its stage node IDs match `models.AllPRStages` — adding a
stage without updating the spec fails `go test`. The `/api/v1/workflow` endpoint exposes the
spec as JSON; the dashboard's `/how-it-works` route renders it as an interactive SVG.

## Idempotency (current state and planned)

**Implemented:** outcomes are recorded in SQLite per `(pr_number, head_sha)`. On the next
cycle, a PR whose head SHA matches a recorded terminal outcome is skipped with zero LLM calls.

**Planned (Spec B):** full idempotent PR *writing* — upsert-not-duplicate for reviews and
sticky status comments, keyed to head SHA + disposition. The `<!-- sweeper:status -->` marker
mechanism is in place; the full create-vs-edit semantics across all dispositions are not yet
implemented.
