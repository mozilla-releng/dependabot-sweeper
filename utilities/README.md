# Test-bed utilities

Scripts for iterating on `dependabot-sweeper` against **your own fork** of a
target monorepo, instead of the shared upstream repo. This lets you exercise the
full pipeline (open replacement PRs, rebase, cancel CI, …) without ever touching
the real upstream or its contributors.

Each contributor brings their own fork, GitHub identity, and (optionally) GCP
project, so several people can work in parallel without colliding. Nothing here
is specific to any one user — personal values live in a git-ignored
`utilities/sweeper.env`.

## Setup

```bash
cp utilities/sweeper.env.example utilities/sweeper.env
# edit utilities/sweeper.env: set FORK=you/taskcluster (and PROJECT_ID if you deploy)
```

`sweeper.env` is git-ignored. Every script below sources it; values can also be
passed as flags or environment variables.

## Scripts

| Script | What it does |
|--------|--------------|
| `mirror-upstream-dependabot.sh` | Reset your fork to mirror `UPSTREAM`'s current open dependabot PRs, so a run against the fork is a faithful dry run. Safe by default (prints a plan); `--execute` to apply. `--only-missing` for an incremental sync. |
| `kill-run.sh` | Cleanly stop a run and tear down its fork footprint — close `auto/fix/*` replacement PRs, delete their branches, and cancel their Taskcluster task **groups** — without wasting shared CI. Safe by default; `--execute` to apply. |
| `patches/` | Optional `*.patch` files `mirror-upstream-dependabot.sh` applies on top of upstream `main` so the fork's CI works off-upstream (e.g. a changelog check that hardcoded the upstream repo). |

Typical loop:

```bash
utilities/mirror-upstream-dependabot.sh --execute      # fresh fork state
# ... run the worker against your fork, iterate ...
utilities/kill-run.sh --execute                        # tear the run's footprint down
```

`kill-run.sh --execute` needs Taskcluster credentials in the environment
(`export TASKCLUSTER_ROOT_URL=… && eval "$(taskcluster signin)"`); the script
never handles your token.

## Fork-safety rules (mandatory)

These keep test-bed activity from leaking onto the real upstream or its
maintainers, and from cancelling CI that isn't ours:

- **Base every mirror PR at `<fork>:main`, never upstream.** Activity on the fork
  must not notify or affect the upstream repo.
- **No `@`-mentions and no upstream `owner/repo#NNNN` cross-references** in any PR
  body or comment — they backlink onto upstream PRs and notify maintainers.
- **Touch only the `auto/fix/*` branch namespace** when cleaning up; never delete
  `dependabot/*` branches or close upstream-style originals.
- **Only cancel a Taskcluster task group after verifying its decision task's
  `metadata.source` names your fork** — the CI pools are shared with production.
