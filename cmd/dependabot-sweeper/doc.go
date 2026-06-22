// Package main is the dependabot-sweeper binary, which provides three
// subcommands for automating dependabot PR management using Claude.
//
// # Subcommands
//
//   - review  — one-shot: analyse a single PR and print a summary. No
//     persistent state; uses GitHub PR body SHA markers for idempotency.
//     Intended for interactive use and integration testing.
//
//   - worker  — persistent daemon: runs a scan on a configurable interval
//     (--interval, default 30m) via an internal ticker, processing all open
//     dependabot PRs in the configured GitHub repo. Stores per-PR state in a
//     SQLite database (--data-dir); restarts recover in-progress pipelines
//     from the persisted checkpoint.
//
//   - web     — live operator dashboard: a read-only HTTP server (--listen-addr,
//     default localhost:8080) serving a Svelte SPA backed by Server-Sent
//     Events. Reads the same SQLite database the worker writes; the two
//     processes can run concurrently (the worker opens in WAL write mode, the
//     web process in read mode).
//
// # Architecture at a glance
//
// The worker and web subcommands share a single SQLite database on disk.
// The worker processes PRs with bounded concurrency (--concurrency, default
// 20). For each PR needing code changes it:
//
//  1. Runs the combined analyse-and-decide agent (a claude subprocess) to
//     assess the bump and decide on an action.
//  2. On a "needs_changes" verdict, hands off to the implementation pipeline:
//     a series of bounded claude worker turns that fix the code, push a draft
//     replacement PR, and exit. The orchestrator owns the CI gate between
//     turns, waking the pipeline on the next scan rather than blocking.
//  3. When CI is acceptable, runs the reviewer agent, then curates/squashes
//     commits and marks the replacement PR ready for review.
//
// The web server exposes the live PR state, per-PR agent logs, scan status,
// and the workflow decision graph (/api/v1/events SSE, /api/v1/prs,
// /api/v1/workflow, /how-it-works).
//
// See docs/ARCHITECTURE.md for the full picture.
package main
