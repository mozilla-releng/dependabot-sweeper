# Dashboard

## Overview

The dashboard is a read-only web UI that shows the live state of every open dependabot PR the
worker is processing. It is served by the `web` subcommand and reads from the same SQLite
database that the worker writes. The two processes share no in-process state — the worker
writes, the web server reads, SSE events push updates to the browser.

## Routes

| Path | Description |
|---|---|
| `/` | Main pipeline board |
| `/how-it-works` | Interactive workflow explainer (rendered from the `/api/v1/workflow` spec) |

The server is a Go HTTP server with a Svelte 5 SPA embedded via `go:embed`. All unknown paths
fall back to `index.html` so the client-side router handles deep-links correctly.

## API endpoints

| Method + Path | Description |
|---|---|
| `GET /api/v1/prs` | All tracked PRs as `PRProgress[]` |
| `GET /api/v1/prs/{n}` | Single PR by number |
| `GET /api/v1/prs/{n}/log` | Raw JSONL agent log for a PR (streamed as text/plain) |
| `GET /api/v1/status` | Scan timing: `last_scan`, `next_scan`, `in_flight` |
| `GET /api/v1/events` | SSE stream; emits `update` events when the DB changes |
| `GET /api/v1/workflow` | Declarative workflow graph (`WorkflowGraph` JSON) |

## Data model

### `PRProgress`

The primary data object. Represents one open PR's current state and full history:

| Field | Type | Notes |
|---|---|---|
| `pr_number` | int | |
| `package_name` | string | |
| `bump_type` | string | patch / minor / major |
| `stage` | `PRStage` | Current stage (see below) |
| `last_updated` | RFC3339 | |
| `history` | `StageEvent[]` | Stage transitions with timestamps and detail strings |
| `old_version` / `new_version` | string? | Populated after analysis |
| `ecosystem` | string? | npm / pip / cargo / etc |
| `ci` | `CIStatus?` | Aggregate CI state + per-check detail |
| `analysis` | `AgentAnalysis?` | Analyser output: recommendation, confidence, breaking changes, codebase impact |
| `budget_spent` | float? | USD spent by the implementation agent on this PR |
| `replacement_pr` | int? | PR number opened by the implementation agent |
| `session_id` | string? | Claude Code session ID (for resume) |
| `head_sha` | string? | SHA at the time the terminal outcome was recorded |
| `outcome` | string? | Terminal outcome string (set once, never overwritten) |

### PR stages

```
pending → analysing → approved
                    → impl_starting → impl_running → waiting_ci → impl_resuming (→ impl_running)
                                                                 → reviewing → finalized
                    → flagged_human
                    → gave_up
        → ci_settling   (unsettled CI at entry — revisit next cycle)
        → skipped       (already has a recorded terminal outcome for this SHA)
        → error
```

Stage groupings used by the board's phase filter:

| Phase | Stages |
|---|---|
| Pending | pending, ci_settling, skipped |
| Analysis | analysing |
| Approved | approved |
| Implementation | impl_starting, impl_running, waiting_ci, impl_resuming |
| Review | reviewing |
| Done | finalized, flagged_human, gave_up, error |

### `CIStatus`

Aggregate CI state for a PR: `state` (success/failure/pending), total/passed/failed/pending
counts, a `failures` array of `CheckDetail` for failing checks, and a `checks` array of all
checks. `CheckDetail` includes name, status, conclusion, details URL, and `output` (the check
run's summary+text — used to diagnose failures).

### `AgentAnalysis`

Structured output from the analyser: `recommendation` (approve / needs_changes / flag_for_human),
`confidence` (high / medium / low), `review_body` (human-readable explanation), and optional
arrays for `breaking_changes`, `deprecations`, `codebase_impact` (per-file usage + impact),
and `code_changes` (per-file description of what the implementation agent changed).

## Component structure

```
App.svelte                — root; router; SSE connection; data fetching
├── Header.svelte         — nav bar; PR count; scan status; connected indicator
├── StateMap.svelte       — clickable phase-count map (click to filter by phase)
├── PipelineBoard.svelte  — column-per-phase Kanban board of PR cards
│   └── Column.svelte     — one phase column
│       └── PrCard.svelte — PR card (click to open drawer)
├── PrDrawer.svelte       — slide-in detail panel for a selected PR
│   ├── StageBadge.svelte — stage pill with colour
│   ├── StageTimeline.svelte — history of stage transitions
│   ├── CiChecks.svelte   — CI check list with links to details
│   ├── AnalyserVerdict.svelte — analyser output (recommendation, breaking changes, impact)
│   ├── RunMeta.svelte    — run metadata (budget, session ID, replacement PR link)
│   └── AgentLogTail.svelte — live-tail of the JSONL agent log
└── WorkflowExplainer.svelte — /how-it-works page; fetches /api/v1/workflow and renders SVG
```

## Live updates

The SPA opens a persistent SSE connection to `/api/v1/events`. When the worker completes a
scan (or any PR state changes), the server emits an `update` event. On receipt the SPA
re-fetches both `/api/v1/prs` and `/api/v1/status` and merges the result into the current
view, updating the open drawer in place if its PR changed.

The poll interval for the web process's internal DB watch is configurable via `--poll-interval`
(default 1s).
