# Architecture

## Overview

Dependabot Sweeper is a **persistent service** that automates the review and
remediation of dependabot PRs in a GitHub repo using Claude. It runs as a
long-lived process with an internal scan ticker — not a system cron job.

On each scan, it triages every open dependabot PR and takes one of three
actions: approve it, drive an automated fix pipeline that opens a replacement
PR, or flag it for human attention with a concise explanation.

## Execution model

The binary exposes three subcommands (`cmd/dependabot-sweeper/`):

- **`worker`** — the persistent daemon. Runs an immediate first scan, then
  repeats on an in-process `time.Ticker` (`internal/service/service.go`,
  default `--interval=30m`, deployed at `10m`). A scan-mutex guard
  (`scanMu.TryLock`) drops a tick if the previous scan is still running —
  scans are strictly sequential. Deployed as a `restart: unless-stopped`
  Docker container.
- **`web`** — the operator dashboard. A separate, concurrent read-only
  HTTP process that reads the same SQLite database the worker writes.
- **`review`** — a one-shot analysis of a single PR. Store-less; uses
  embedded SHA markers in the PR body for idempotency. Intended for
  interactive use and integration testing.

The scanner is **level-triggered**: each scan re-reads current GitHub state
and decides what to do now — it does not carry per-PR memory in process
state. The SQLite database is the memory.

## State

The tool is **stateful across scans**. `internal/sqlitestore` maintains a
SQLite database (WAL mode, file on a persistent bind-mounted disk) with three
main tables:

| Table | Contents |
|---|---|
| `pr_progress` | Per-PR stage, CI summary, session/worktree/branch metadata, analysis blob, replacement-PR linkage, and `pipeline_checkpoint` (resumable-pipeline JSON) |
| `stage_events` | Append-only timeline of stage transitions per PR |
| `created_prs` | Reap-exempt record of every replacement PR the tool created (Q14) |
| `scan_status` | Single-row scan timing written by the worker, read by the web process |

The `(pr_number, head_sha)` pair is the idempotency key: a PR whose head SHA
matches a recorded terminal outcome is skipped on the next scan with zero LLM
calls. The `pipeline_checkpoint` column persists mid-flight pipeline state so
a later scan can resume where a prior one yielded. The database survives
restarts; the worker applies schema migrations idempotently on startup.

## User interfaces

There are two distinct audiences, each with their own interface:

**Operator (the person running the tool):**
- **Web dashboard** (`internal/web/`) — a Svelte SPA served at `--listen-addr`
  (default `localhost:8080`). Endpoints: `/api/v1/prs` (PR list), per-PR agent
  log tail (`/api/v1/prs/{n}/log`, last 200 lines of the JSONL transcript),
  `/api/v1/status` (scan timing), `/api/v1/events` (Server-Sent Events for
  live updates), `/api/v1/workflow` (decision graph JSON). Intentionally
  read-only and unauthenticated. The `web` process and `worker` run
  concurrently against the same SQLite file (writer vs. reader WAL modes).

**Repo maintainers (the people whose PRs are acted on):**
- **GitHub PR** — the only channel to maintainers. A sticky status comment
  (`<!-- sweeper:status -->`) is created once and edited in place on
  subsequent scans (via `UpsertStatusComment`). On a "flag for human"
  disposition, the concise reason appears there. The replacement PR body
  carries the agent's justification; the original dependabot PR is closed
  with a brief comment linking to the replacement.

No email, no Slack, no notifications outside the PR. (The operator dashboard
is an observability surface, not a channel to maintainers — see PRINCIPLES.md.)

## The implementation pipeline

Each PR that needs code changes goes through a gated pipeline driven by the
Go orchestrator (`internal/orchestrator/`). The orchestrator is a **Go
program**, not an LLM agent. It launches `claude` CLI subprocesses.

```
Orchestrator (Go)
  │
  ├─ Combined agent (claude subprocess — internal/agent/)
  │    Reads: PR diff, brief with metadata + environment context
  │    The agent fetches upstream data, changelog, CI, and codebase
  │    context itself ("agent empowerment principle")
  │    Produces: structured JSON verdict — recommend / needs_changes /
  │              flag_human / gave_up
  │
  │  On "needs_changes":
  │
  ├─ Implementation pipeline (internal/implementation/)
  │    Phase 0: manual rebase if the dependabot branch is behind base
  │    Turn 1: worker subprocess (claude) fixes code, commits, pushes,
  │            opens draft replacement PR, exits
  │
  ├─ CI gate (orchestrator-owned)
  │    Fetches branch CI once per scan; yields (InProgress) if pending —
  │    the scan thread is freed and the next scan resumes from checkpoint.
  │    If CI fails on a required check: worker is resumed with failure logs.
  │    Bounded by MaxImplIterations (CI-fix turns) + MaxImplTime (wall clock).
  │
  ├─ Reviewer (claude subprocess — internal/reviewer/)
  │    When CI is acceptable, reviews the diff.
  │    approve → finalize; request_changes → resume worker (bounded retries).
  │
  └─ Finalize
       Curate/squash commits (curate agent subprocess), force-push,
       re-verify post-squash CI, un-draft the replacement PR,
       close the original dependabot PR.
```

**Key design points:**

- The worker is a **bounded turn**: fix → push → open draft PR → exit. It
  never polls CI. The orchestrator owns the CI gate entirely.
- On CI-fix resume, the **same Claude session is continued** via
  `claude --resume <session-id>`, so the worker accumulates context across
  turns without starting from scratch.
- The pipeline is **level-triggered**: when CI is pending the pipeline
  persists a checkpoint and returns. The *next scan* resumes it. No thread
  blocks waiting for CI; a scan's wall-clock time is bounded by actual work,
  not idle CI waiting.
- The default analysis path is the **combined subprocess agent** —
  a single `claude` invocation that analyses the PR and decides the action
  in one step. The separate SDK-based `internal/analyser` package is a
  legacy rollback path, reached only via `--legacy-analyser`.

## CI perception

The tool reads CI exclusively via the **GitHub Checks API** — no
provider-specific APIs (no Taskcluster queue, no GitHub Actions log API).
This makes it provider-agnostic. For failing checks it reads
`output.summary` + `output.text` from the check run.

**Settledness** — a PR's CI is settled when every check is either terminal
(completed with any conclusion) or stale (pending longer than
`--ci-staleness`, default 12h, measured from `CreatedAt`). Unsettled PRs are
skipped and revisited on the next scan.

**AcceptableGiven** — the success criterion (`CIStatus.AcceptableGiven` in
`internal/models/`) applies **required-checks gating** (Q7): when branch
protection defines a required-checks set, only checks in that set can block.
If no required set is readable, it falls back to gating on all checks.

The **only** suppression on the production path is the operator
`--ignore-check` list: named checks are never blocking, regardless of their
result. This is the escape hatch for known structural failures (e.g.
`CodeQL`, `Dependabot auto-merge`). The implementation agent is expected to
fix even pre-existing required-check failures — if it cannot, the pipeline
gives up after `--max-impl-iterations` / `--max-no-progress-iterations`.

(The `AcceptableGiven` function accepts a `baseFailures` map for legacy
base-branch-failure suppression, but the production path passes `nil` — this
was a deliberate Q3 decision. The only site that passes a non-nil value is
the `--legacy-analyser` routing path.)

## Decision graph

The full PR disposition state machine is declared in `internal/workflow/` as
a static Go graph. `workflow.ValidateTransition` guards every stage write in
the store — adding a stage constant without updating the spec fails `go test`.

The `/api/v1/workflow` endpoint exposes the graph as JSON. The dashboard's
`/how-it-works` route renders it as an interactive SVG.

## Idempotency

**Implemented:**
- `(pr_number, head_sha)` outcomes in SQLite — a PR at the same head SHA
  skips with zero LLM calls.
- `UpsertStatusComment` — the sticky status comment is created once and
  edited in place; never duplicated.
- `created_prs` table (Q14) — the tool's own replacement PRs are permanently
  excluded from re-ingestion as fresh dependabot PRs.
- `pipeline_checkpoint` — a mid-flight pipeline yields and resumes rather
  than starting over, preserving session continuity and spend.

## Package layout

| Path | Role |
|---|---|
| `cmd/dependabot-sweeper/` | Binary entrypoint; subcommand dispatch (`review` / `worker` / `web`) |
| `internal/service/` | Persistent daemon: internal ticker, single-scan overlap guard, scan-status sink |
| `internal/orchestrator/` | Per-scan PR processing loop: gates, routing, finalize, bare-clone management |
| `internal/agent/` | Combined analyse-and-decide `claude` subprocess + verdict parsing |
| `internal/implementation/` | Full fix lifecycle: clone/branch/rebase, worker turns, resumable CI-fix/review state machine, curate/squash, checkpoints |
| `internal/reviewer/` | Independent reviewer `claude` subprocess; structured JSON verdict |
| `internal/analyser/` | **Legacy** SDK-based analyser; only with `--legacy-analyser` |
| `internal/models/` | Shared types; CI evaluation logic (`Settled`, `AcceptableGiven`), stage constants, verdict/result structs |
| `internal/github/` | GitHub API client: fetch PRs, check runs, required checks, log tails, PR mutations |
| `internal/sqlitestore/` | SQLite-backed progress store, schema/migrations, SSE Notifier, scan-status reader/writer |
| `internal/progress/` | Storage abstraction interfaces (`Writer`/`Reader`/`ReadWriter`/`Notifier`); imports only `models` to avoid cycles |
| `internal/web/` | Read-only HTTP server: embedded Svelte SPA, JSON+SSE endpoints, per-PR log tail, workflow graph |
| `internal/workflow/` | Static decision graph and transition validator |
| `internal/config/` | Environment + `.env` config loading, functional-option overrides, validation |
| `internal/llmutil/` | Shared helpers for parsing LLM text output (`ExtractJSON`, `FirstText`, `Truncated`) |

## End-to-end: one PR

1. **Scan** — `Service.runOneScan` → `Orchestrator.Run` → `github.GetDependabotPRs`. Drop the tool's own replacement PRs (`created_prs` check). Reap closed PRs from the store. Prepopulate "pending" rows for new PRs.

2. **Per-PR gates** (`processPR`) — record current versions + CI; skip `BumpUnknown`; staleness/supersession check; resume in-flight checkpoint if valid; skip if CI state is unknown; CI settledness gate; `(pr, head_sha)` idempotency skip.

3. **Combined agent** — `runCombinedAgent`: clone workdir, run `claude` subprocess with a brief built from PR metadata. Returns a structured verdict. Routing:
   - `recommend` → re-gate required CI, upsert sticky comment. Never a native GitHub APPROVE.
   - `flag_human` → upsert concise-reason sticky comment.
   - `gave_up` → post silent draft, record terminal outcome.
   - `needs_changes` → implementation pipeline.

4. **Implementation pipeline** — `Pipeline.Run`: Phase 0 rebase if behind base; clone + branch; pin a session UUID; run worker turn 1 (worker pushes and opens a **draft** replacement PR). Hand to `drive()` state machine.

5. **CI-fix loop** — `drive()` phase `awaiting_impl_ci`: fetch branch CI; `AcceptableGiven` check. If pending → `park()` (persist checkpoint, return `InProgress`; scan completes). Next scan: `Resume()` reloads checkpoint, calls `drive()` again. If settled + failing → resume worker with failure logs. Bounded by `MaxImplIterations` / `MaxNoProgressIterations` / `MaxImplTime`.

6. **Review** — when CI acceptable, `reviewer.Review` subprocess. `request_changes` → resume worker (bounded by `MaxReviewRetries`). `approve` → finalize.

7. **Finalize** — curate commits (curate `claude` subprocess), force-push, phase `awaiting_postsquash_ci` (same park/resume loop). On success: `UpdatePRTitle`, `MarkPRReadyForReview`, post justification to PR body, close original dependabot PR with link, record `finalized` + replacement linkage.
