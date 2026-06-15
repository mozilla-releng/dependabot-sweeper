#!/usr/bin/env bash
#
# kill-run.sh -- cleanly stop a dependabot-sweeper run and tear down its footprint
# on the fork test bed WITHOUT wasting shared CI capacity.
#
# A run leaves three kinds of artifact behind:
#   1. A local orchestrator process (+ its `claude --print` impl-agent children).
#   2. Taskcluster CI -- whole task GRAPHS triggered by (a) replacement PRs the
#      run opened and (b) the dependabot originals it force-pushed (rebased).
#      Cancelling the leaf checks isn't enough: a push schedules a decision task
#      that fans out to a whole task group, so we cancel the GROUP (the decision
#      task's taskId == the taskGroupId of every task in the graph).
#   3. Replacement branches/PRs (the `auto/fix/*` namespace).
#
# Steps (in order):
#   1. Kill the local orchestrator + orphaned impl agents (unless --no-kill).
#   2. Discover the footprint:
#        - replacement PRs/branches  = open PRs on `auto/fix/*` + any auto/fix/* branch
#        - rebased originals         = open PRs whose HEAD commit committer is the
#                                      rebase bot (default: dependabot-helper)
#        - plus any PRs given via --cancel-ci-for
#   3. For each, resolve the Taskcluster task GROUP and VERIFY its decision task's
#      metadata.source contains "<repo>" before cancelling (so we never cancel a
#      task graph that isn't ours -- the pools are shared with production).
#   4. Cancel each verified group (`taskcluster group cancel`).
#   5. Close the replacement PRs and delete the `auto/fix/*` branches.
#
# Safe by default: prints the plan and exits. Pass --execute to actually mutate.
#
# Requirements: gh (authenticated), jq, curl. For --execute, also `taskcluster`
# with credentials in the environment (run `eval "$(taskcluster signin)"` first;
# this script never handles your token).
#
# Configuration comes from utilities/sweeper.env (copy from sweeper.env.example),
# any inherited environment, or --repo. Your fork is per-user — there is no
# default — so set FORK in sweeper.env or pass --repo.
#
# Usage:
#   utilities/kill-run.sh                              # plan for $FORK
#   utilities/kill-run.sh --execute                    # apply
#   utilities/kill-run.sh --cancel-ci-for 51 --cancel-ci-for 53 --execute
#   REPO=me/bar utilities/kill-run.sh --execute
#
# Fork-safety (see utilities/README.md): only ever touches the `auto/fix/*`
# branch namespace and only cancels task groups whose source is the target repo.
# It does NOT delete dependabot/* branches or close originals.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Load per-user config if present (FORK, TASKCLUSTER_ROOT_URL, BOT_NAME, ...).
# shellcheck disable=SC1091
[ -f "$SCRIPT_DIR/sweeper.env" ] && source "$SCRIPT_DIR/sweeper.env"

REPO="${REPO:-${FORK:-}}"
ROOT_URL="${TASKCLUSTER_ROOT_URL:-https://community-tc.services.mozilla.com}"
BOT_NAME="${BOT_NAME:-dependabot-helper}"   # committer identity set by the manual rebase
FIX_PREFIX="auto/fix/"
EXECUTE=0
DO_KILL=1
EXTRA_CI_PRS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --execute|-y|--yes)   EXECUTE=1 ;;
    --no-kill)            DO_KILL=0 ;;
    --repo)               REPO="$2"; shift ;;
    --root-url)           ROOT_URL="$2"; shift ;;
    --bot-name)           BOT_NAME="$2"; shift ;;
    --cancel-ci-for)      EXTRA_CI_PRS+=("$2"); shift ;;
    -h|--help)            sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
  shift
done

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }

if [ -z "$REPO" ]; then
  echo "No fork configured. Set FORK in utilities/sweeper.env (copy from" >&2
  echo "sweeper.env.example), export FORK=owner/repo, or pass --repo owner/repo." >&2
  exit 2
fi

command -v gh   >/dev/null || { echo "gh is required" >&2; exit 1; }
command -v jq   >/dev/null || { echo "jq is required" >&2; exit 1; }
command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }

say "Target repo: $REPO   (Taskcluster root: $ROOT_URL)"
if [ "$EXECUTE" = 1 ]; then say "MODE: EXECUTE -- this will cancel CI and mutate $REPO"; else say "MODE: plan only (re-run with --execute to apply)"; fi
echo

# --------------------------------------------------------- 1. kill processes ---
if [ "$DO_KILL" = 1 ]; then
  # Kill only the orchestrator -- its `claude --print` impl-agent children are in
  # its process group and exit with it. We deliberately do NOT broad-pkill
  # `claude --print`, which could hit unrelated Claude automation on the machine.
  PIDS=$(pgrep -f 'dependabot-sweeper worker' 2>/dev/null || true)
  if [ -n "$PIDS" ]; then
    say "Orchestrator process(es) to stop (impl-agent children exit with them):"
    for p in $PIDS; do echo "       dependabot-sweeper  pid=$p"; done
    if [ "$EXECUTE" = 1 ]; then
      # shellcheck disable=SC2086
      kill $PIDS 2>/dev/null || true
      say "   sent SIGTERM"
    fi
  else
    say "No local 'dependabot-sweeper worker' process running."
  fi
else
  say "Skipping local process kill (--no-kill)."
fi
echo

# resolve a PR head SHA -> a community-tc task in its checks -> taskGroupId
group_for_pr() {
  local n="$1" sha tid
  sha=$(gh api "repos/$REPO/pulls/$n" --jq .head.sha 2>/dev/null) || return 1
  tid=$(gh api "repos/$REPO/commits/$sha/check-runs?per_page=100" \
        --jq "[.check_runs[]|select(.details_url|test(\"$( echo "$ROOT_URL" | sed 's#https\?://##' )\"))][0].details_url" 2>/dev/null \
        | sed 's#.*/tasks/##')
  [ -z "$tid" ] || [ "$tid" = "null" ] && return 1
  curl -fsS "$ROOT_URL/api/queue/v1/task/$tid" 2>/dev/null | jq -r '.taskGroupId // empty'
}

# true if a task group's decision task names $REPO as its source (msg-4 safety)
group_is_ours() {
  local grp="$1" src
  src=$(curl -fsS "$ROOT_URL/api/queue/v1/task/$grp" 2>/dev/null | jq -r '.metadata.source // ""')
  case "$src" in *"$REPO"*) return 0 ;; *) return 1 ;; esac
}

# ------------------------------------------------------------- 2. discover -----
say "Discovering replacement PRs/branches ($FIX_PREFIX*) and rebased originals..."

REPL_PRS=$(gh pr list --repo "$REPO" --state open --json number,headRefName,title --limit 300 \
  | jq -r ".[] | select(.headRefName|startswith(\"$FIX_PREFIX\")) | .number")

REPL_BRANCHES=$(gh api "repos/$REPO/branches?per_page=100" --paginate --jq '.[].name' 2>/dev/null \
  | grep "^$FIX_PREFIX" || true)

# rebased originals: open PRs whose head commit was committed by the rebase bot
REBASED_PRS=""
while read -r n; do
  [ -z "$n" ] && continue
  sha=$(gh api "repos/$REPO/pulls/$n" --jq .head.sha 2>/dev/null) || continue
  cn=$(gh api "repos/$REPO/commits/$sha" --jq '.commit.committer.name' 2>/dev/null || true)
  [ "$cn" = "$BOT_NAME" ] && REBASED_PRS="$REBASED_PRS $n"
done < <(gh pr list --repo "$REPO" --state open --json number,headRefName --limit 300 \
          | jq -r ".[] | select(.headRefName|startswith(\"$FIX_PREFIX\")|not) | .number")

# union of all PRs whose CI group we should cancel
CI_PRS=$(printf '%s\n' $REPL_PRS $REBASED_PRS "${EXTRA_CI_PRS[@]:-}" | sort -un | grep -v '^$' || true)

echo
say "PLAN"
echo "  Replacement PRs to CLOSE:        ${REPL_PRS:-<none>}" | tr '\n' ' '; echo
echo "  Replacement branches to DELETE:"
[ -n "$REPL_BRANCHES" ] && echo "$REPL_BRANCHES" | sed 's/^/       - /' || echo "       <none>"
echo "  PRs whose CI task GROUP to CANCEL: $(echo $CI_PRS | tr '\n' ' ')"

# resolve + verify groups now (read-only; works without TC creds)
# Collect verified group IDs as a newline-delimited string (portable: no bash
# arrays / mapfile, so this runs the same under macOS's stock bash 3.2).
# NB: NOT named GROUPS -- that's a reserved bash array (the user's GIDs) and
# assignments to it are silently ignored.
TASK_GROUPS=""
for n in $CI_PRS; do
  grp=$(group_for_pr "$n" || true)
  if [ -z "$grp" ]; then warn "PR #$n: no Taskcluster group found (skipping)"; continue; fi
  if group_is_ours "$grp"; then
    echo "       #$n -> group $grp  (verified $REPO)"
    TASK_GROUPS="${TASK_GROUPS}${grp}"$'\n'
  else
    warn "PR #$n -> group $grp NOT sourced from $REPO -- refusing to cancel"
  fi
done
# dedupe (a PR set can share a group in odd cases)
TASK_GROUPS=$(printf '%s' "$TASK_GROUPS" | sort -u | sed '/^$/d')
echo "  Task groups to cancel:           $(echo $TASK_GROUPS | tr '\n' ' ')"
echo

if [ "$EXECUTE" != 1 ]; then
  say "Plan only -- nothing changed. Re-run with --execute to apply."
  exit 0
fi

# --------------------------------------------------------- 3+4. cancel CI ------
if [ -n "$TASK_GROUPS" ]; then
  command -v taskcluster >/dev/null || { echo "taskcluster CLI is required for --execute" >&2; exit 1; }
  if [ -z "${TASKCLUSTER_CLIENT_ID:-}" ] && [ -z "${TASKCLUSTER_ACCESS_TOKEN:-}" ]; then
    echo "No Taskcluster credentials in the environment." >&2
    echo "Run:  export TASKCLUSTER_ROOT_URL=$ROOT_URL && eval \"\$(taskcluster signin)\"" >&2
    exit 1
  fi
  export TASKCLUSTER_ROOT_URL="$ROOT_URL"
  say "Cancelling $(printf '%s\n' "$TASK_GROUPS" | grep -c .) task group(s)..."
  for g in $TASK_GROUPS; do
    if taskcluster group cancel --force "$g"; then echo "   cancelled group $g"; else warn "could not cancel group $g"; fi
  done
fi

# ----------------------------------------------- 5. close PRs + del branches ---
if [ -n "$REPL_PRS" ]; then
  say "Closing replacement PR(s)..."
  for n in $REPL_PRS; do
    gh pr close "$n" --repo "$REPO" --delete-branch \
      --comment "Closing replacement PR and stopping its CI: the originating run was killed." \
      && echo "   closed #$n (+branch)" || warn "could not close #$n"
  done
fi

# delete any leftover auto/fix/* branches that had no open PR
if [ -n "$REPL_BRANCHES" ]; then
  say "Deleting any leftover $FIX_PREFIX* branches..."
  while read -r b; do
    [ -z "$b" ] && continue
    gh api -X DELETE "repos/$REPO/git/refs/heads/$b" >/dev/null 2>&1 && echo "   deleted $b" || true
  done <<< "$REPL_BRANCHES"
fi

echo
say "Done. Run footprint torn down on $REPO."
