# Dependabot Sweeper

Automated review and management of dependabot PRs. Runs as a background cron worker that
scans a GitHub repo's open dependabot PRs, triages each one using Claude, and either posts a
recommendation to merge (if CI is passing and no code changes are needed) or drives an
implementation agent to fix code breakage and open a replacement PR.

The tool emulates a competent human reviewer: it fixes things it is confident about, explains
things it isn't, and stays silent when it has nothing useful to add. The net effect is less
human attention required, not more. It always **proposes** — a human makes the final call; the
tool never submits a native GitHub approval.

## Live prototype

A read-only prototype dashboard is deployed at **http://146.148.23.195:8080** (HTTP only; the
address may change, and HTTPS is not yet configured). It shows live triage state for the repo
the worker is currently scanning.

## Prerequisites

- Go 1.22+
- Node 22+ (to build the embedded dashboard SPA)
- [`gh` CLI](https://cli.github.com/) authenticated (`gh auth login`)
- [Claude Code CLI](https://claude.ai/code) (`claude`) — used to drive the implementation agent

## Build

The web dashboard is a Svelte SPA embedded into the binary via `//go:embed all:ui/dist`,
so the SPA must be built before the Go binary:

```bash
# 1. Build the dashboard SPA (produces internal/web/ui/dist)
npm --prefix internal/web/ui ci
npm --prefix internal/web/ui run build

# 2. Build the binary
go build -o dependabot-sweeper ./cmd/dependabot-sweeper
```

`ui/dist` is a build artifact and is not committed; CI and the Docker image build it from
source. After the first SPA build, `go build`/`go test` work directly until the SPA changes.

## Configuration

Set the following environment variables (or a `.env` file):

| Variable | Description |
|---|---|
| `ANTHROPIC_API_KEY` | Anthropic API key for the analyser and reviewer |
| `DEPENDABOT_REVIEWER_TOKEN` | GitHub PAT with `repo` scope — used to post reviews and open PRs |

## Usage

### Worker (cron loop)

Scans all open dependabot PRs on an interval, writes state to a SQLite database:

```bash
dependabot-sweeper worker \
  --repos owner/repo \
  --db /var/lib/sweeper/sweeper.db \
  --interval 30m
```

### Web dashboard

Read-only API and embedded SPA served from the same database:

```bash
dependabot-sweeper web \
  --db /var/lib/sweeper/sweeper.db \
  --listen-addr :8080
```

Open `http://localhost:8080` to see the live dashboard.

### One-shot review

Review a single PR without the cron loop:

```bash
dependabot-sweeper review --repos owner/repo --pr 42
```

## Key flags

| Flag | Default | Description |
|---|---|---|
| `--repos` | (required) | GitHub `owner/repo` to scan |
| `--db` | `sweeper.db` | SQLite database path |
| `--interval` | `30m` | Scan interval (worker only) |
| `--listen-addr` | `:8080` | HTTP listen address (web only) |
| `--dry-run` | off | Skip all GitHub writes and LLM calls |
| `--pr` | (all) | Process only this PR number |
| `--concurrency` | 20 | Max concurrent PRs |
| `--accept-author` | `dependabot[bot]`, `renovate[bot]` | Additional PR authors to process |
| `--analyser-model` | `claude-sonnet-4-6` | Claude model for the analyser |
| `--impl-model` | Claude Code default | Claude model for the implementation agent |
| `--max-impl-budget` | $50 | Per-PR spend cap for the implementation agent |
| `--ignore-check` | (none) | CI check names to treat as non-blocking (repeatable) |
