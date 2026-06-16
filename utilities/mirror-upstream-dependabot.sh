#!/usr/bin/env bash
#
# mirror-upstream-dependabot.sh -- reset the fork test bed so it mirrors the
# upstream repo's *current* open dependabot PRs. After running this, a tool run
# against the fork is a faithful dry run of what would happen against upstream.
#
# Steps (in order):
#   1. Close every open PR on the fork.
#   2. Force the fork's main to match upstream main.
#   3. For every open dependabot PR upstream, push its branch (at the PR's head
#      SHA) to the fork and open an equivalent PR (base = fork main, authored by
#      the fork owner -- dependabot[bot] can't author on the fork).
#
# --only-missing (incremental): KEEP the fork's existing open PRs (and reuse
# their already-settled CI); still advance main to upstream; mirror only the
# upstream dependabot PRs whose head branch isn't already open on the fork.
# Cheaper than a full re-mirror (no re-running CI on the kept PRs) and useful
# when a killed run left the fork partially mirrored. Note: moving main leaves
# the kept PRs shown as "behind" base, but it does NOT re-trigger their CI
# (a base move emits no pull_request event), and the impl pipeline rebases.
#
# Safe by default: prints the plan and exits. Pass --execute to actually mutate.
#
# Requirements: git with SSH push access to the fork, gh (authenticated), jq.
#
# Configuration comes from utilities/sweeper.env (copy from sweeper.env.example),
# any inherited environment, or the flags below. Your fork is per-user — there is
# no default — so set FORK in sweeper.env or pass --fork.
#
# Usage:
#   utilities/mirror-upstream-dependabot.sh                 # show the plan
#   utilities/mirror-upstream-dependabot.sh --execute       # apply it
#   utilities/mirror-upstream-dependabot.sh --only-missing --execute  # incremental
#   UPSTREAM=foo/bar FORK=me/bar utilities/mirror-upstream-dependabot.sh --execute
#
# Fork-safety (see utilities/README.md):
#   - mirror PR base is always <fork>:main, never upstream;
#   - no @-mentions and no upstream `owner/repo#NNNN` cross-references in bodies
#     (they'd backlink onto the upstream PRs and notify maintainers);
#   - no project name in any comment or body.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Load per-user config if present (FORK, UPSTREAM, DEPENDABOT_AUTHOR, ...).
# shellcheck disable=SC1091
[ -f "$SCRIPT_DIR/sweeper.env" ] && source "$SCRIPT_DIR/sweeper.env"

UPSTREAM="${UPSTREAM:-taskcluster/taskcluster}"
FORK="${FORK:-}"
DEPENDABOT_AUTHOR="${DEPENDABOT_AUTHOR:-app/dependabot}"
CLONE_DIR="${CLONE_DIR:-$HOME/.cache/dependabot-sweeper-fork-mirror}"
EXECUTE=0
ONLY_MISSING=0

while [ $# -gt 0 ]; do
  case "$1" in
    --execute|-y|--yes) EXECUTE=1 ;;
    --only-missing|--incremental) ONLY_MISSING=1 ;;
    --upstream) UPSTREAM="$2"; shift ;;
    --fork)     FORK="$2"; shift ;;
    --clone-dir) CLONE_DIR="$2"; shift ;;
    -h|--help)  sed -n '2,36p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
  shift
done

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
command -v gh >/dev/null || { echo "gh is required" >&2; exit 1; }

if [ -z "$FORK" ]; then
  echo "No fork configured. Set FORK in utilities/sweeper.env (copy from" >&2
  echo "sweeper.env.example), export FORK=owner/repo, or pass --fork owner/repo." >&2
  exit 2
fi

FORK_OWNER="${FORK%%/*}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Trim dependabot's "@dependabot commands" footer from a PR body (stdin->stdout)
# and drop trailing separator/wrapper junk left behind. Keeps the changelog /
# release-notes / commits sections, which is what the analyser reads.
sanitize_body() {
  awk '
    /Dependabot commands and options/                  { exit }
    /You can trigger Dependabot actions by commenting/  { exit }
    /Dependabot will resolve any conflicts with this PR/{ exit }
    { lines[NR] = $0; n = NR }
    END {
      while (n > 0 && (lines[n] ~ /^[[:space:]]*$/ \
                    || lines[n] ~ /^-{3,}[[:space:]]*$/ \
                    || lines[n] ~ /^<details>[[:space:]]*$/ \
                    || lines[n] ~ /^<br ?\/?>[[:space:]]*$/)) n--
      for (i = 1; i <= n; i++) print lines[i]
    }
  '
}

say "Test bed: fork=$FORK  upstream=$UPSTREAM"
if [ "$EXECUTE" = 1 ]; then say "MODE: EXECUTE -- this will mutate $FORK"; else say "MODE: plan only (re-run with --execute to apply)"; fi
echo

# ---------------------------------------------------------------- discover ---
say "Discovering upstream open dependabot PRs..."
gh pr list --repo "$UPSTREAM" --state open --author "$DEPENDABOT_AUTHOR" \
  --json number,headRefName,headRefOid,title,body --limit 300 > "$TMP/upstream.json"
UCOUNT=$(jq length "$TMP/upstream.json")

say "Discovering open PRs on the fork (to close)..."
gh pr list --repo "$FORK" --state open \
  --json number,headRefName,title --limit 300 > "$TMP/forkprs.json"
FCOUNT=$(jq length "$TMP/forkprs.json")

# --only-missing: reuse the fork's existing open PRs (and their settled CI),
# advancing main and mirroring only the upstream dependabot PRs whose head
# branch isn't already open on the fork. Cheaper than a full re-mirror because
# moving the base branch does NOT re-trigger CI on the kept PRs.
if [ "$ONLY_MISSING" = 1 ]; then
  jq --slurpfile fork "$TMP/forkprs.json" \
    '[ .[] | select( .headRefName as $h | ([ $fork[0][].headRefName ] | index($h)) | not ) ]' \
    "$TMP/upstream.json" > "$TMP/upstream.missing.json"
  mv "$TMP/upstream.missing.json" "$TMP/upstream.json"
  UCOUNT=$(jq length "$TMP/upstream.json")
fi

echo
say "PLAN"
if [ "$ONLY_MISSING" = 1 ]; then
  echo "  1. (only-missing) KEEP all $FCOUNT open fork PR(s) as-is -- reuse their CI."
else
  echo "  1. Close $FCOUNT open fork PR(s):"
  jq -r '.[] | "       - #\(.number) [\(.headRefName)] \(.title)"' "$TMP/forkprs.json"
fi
echo "  2. Set fork main := upstream main."
echo "  3. Mirror $UCOUNT upstream dependabot PR(s) onto the fork:"
jq -r '.[] | "       - \(.title)\n         (upstream #\(.number), \(.headRefName)@\(.headRefOid[0:8]))"' "$TMP/upstream.json"
echo

if [ "$EXECUTE" != 1 ]; then
  say "Plan only -- nothing changed. Re-run with --execute to apply."
  exit 0
fi

# ------------------------------------------------------- 1. close fork PRs ---
if [ "$ONLY_MISSING" = 1 ]; then
  say "only-missing: leaving the $FCOUNT existing fork PR(s) open (reusing their CI)."
else
  say "Closing $FCOUNT open fork PR(s)..."
  for n in $(jq -r '.[].number' "$TMP/forkprs.json"); do
    gh pr close "$n" --repo "$FORK" \
      --comment "Closing to refresh the test bed against upstream's current open dependabot PRs." \
      && echo "   closed #$n" || echo "   !! could not close #$n"
  done
fi

# ------------------------------------------- 2. local clone + refresh main ---
say "Preparing local mirror clone at $CLONE_DIR..."
if [ -d "$CLONE_DIR/.git" ]; then
  git -C "$CLONE_DIR" remote set-url origin "git@github.com:$UPSTREAM.git"
else
  git clone --filter=blob:none --no-checkout "git@github.com:$UPSTREAM.git" "$CLONE_DIR"
fi
git -C "$CLONE_DIR" remote remove fork 2>/dev/null || true
git -C "$CLONE_DIR" remote add fork "git@github.com:$FORK.git"

say "Fetching upstream main + dependabot branches (blobless)..."
git -C "$CLONE_DIR" fetch --filter=blob:none --prune origin \
  "+refs/heads/main:refs/remotes/origin/main" \
  "+refs/heads/dependabot/*:refs/remotes/origin/dependabot/*"

# Build _patched_main = upstream main + testbed patches applied on top.
# This uses index manipulation (read-tree / apply --cached / write-tree /
# commit-tree) so no working-tree checkout is needed — only the blobs for
# patched files are fetched (lazily, on demand from origin).
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PATCHES_DIR="$SCRIPT_DIR/patches"

patched_parent=$(git -C "$CLONE_DIR" rev-parse "refs/remotes/origin/main")
PATCH_COUNT=0

for patch_file in $(ls -1 "$PATCHES_DIR"/*.patch 2>/dev/null | sort); do
  [ -f "$patch_file" ] || continue
  pname="$(basename "$patch_file")"
  say "  testbed patch: $pname"
  git -C "$CLONE_DIR" read-tree "$patched_parent"
  if git -C "$CLONE_DIR" apply --cached "$patch_file" 2>/dev/null; then
    new_tree=$(git -C "$CLONE_DIR" write-tree)
    msg=$(grep '^Subject:' "$patch_file" | head -1 | sed 's/.*\[PATCH\] //')
    [ -z "$msg" ] && msg="testbed: $pname"
    patched_parent=$(GIT_AUTHOR_NAME="dependabot-sweeper testbed" \
      GIT_AUTHOR_EMAIL="noreply@example.com" \
      GIT_COMMITTER_NAME="dependabot-sweeper testbed" \
      GIT_COMMITTER_EMAIL="noreply@example.com" \
      git -C "$CLONE_DIR" commit-tree "$new_tree" -p "$patched_parent" -m "$msg")
    PATCH_COUNT=$((PATCH_COUNT + 1))
  else
    say "    WARN: $pname didn't apply cleanly -- skipping (may need updating after upstream sync)"
  fi
done

git -C "$CLONE_DIR" update-ref "refs/heads/_patched_main" "$patched_parent"
say "Forcing fork main := upstream main + $PATCH_COUNT testbed patch(es)..."
git -C "$CLONE_DIR" push fork "_patched_main:refs/heads/main" --force

# -------------------------------------------------- 3. mirror dependabot PRs ---
say "Mirroring $UCOUNT dependabot PR(s)..."
mirror_one() {
  local i="$1" num branch sha title
  num=$(jq -r ".[$i].number"     "$TMP/upstream.json")
  branch=$(jq -r ".[$i].headRefName" "$TMP/upstream.json")
  sha=$(jq -r ".[$i].headRefOid" "$TMP/upstream.json")
  title=$(jq -r ".[$i].title"    "$TMP/upstream.json")

  say "  [$((i+1))/$UCOUNT] upstream #$num  $branch@${sha:0:8}"

  # Rebase the dependabot branch onto _patched_main. The dep branch may be
  # many commits behind upstream/main (dependabot doesn't always rebase), so
  # we must NOT start from its tree wholesale — that would make the fork PR diff
  # include all the upstream progress it missed. Instead:
  #   1. Find where the dep branch actually diverged from upstream/main.
  #   2. Compute only the dep-specific changes (dep_base → dep_tip).
  #   3. Start from _patched_main's tree and apply those changes on top.
  # Result: fork PR diff = same files as the upstream PR diff (just the bump).
  dep_base=$(git -C "$CLONE_DIR" merge-base \
    "refs/remotes/origin/main" "refs/remotes/origin/$branch")
  git -C "$CLONE_DIR" read-tree "refs/heads/_patched_main"
  while IFS=$'\t' read -r status fpath; do
    case "$status" in
      M|A)
        entry=$(git -C "$CLONE_DIR" ls-tree "refs/remotes/origin/$branch" "$fpath")
        if [ -n "$entry" ]; then
          mode=$(printf '%s' "$entry" | awk '{print $1}')
          blob=$(printf '%s' "$entry" | awk '{print $3}')
          git -C "$CLONE_DIR" update-index --cacheinfo "$mode,$blob,$fpath"
        fi
        ;;
      D) git -C "$CLONE_DIR" update-index --remove "$fpath" ;;
    esac
  done < <(git -C "$CLONE_DIR" diff-tree -r --name-status \
    "$dep_base" "refs/remotes/origin/$branch")
  merged_tree=$(git -C "$CLONE_DIR" write-tree)
  dep_msg=$(git -C "$CLONE_DIR" log -1 --format="%B" "refs/remotes/origin/$branch")
  rebased_sha=$(git -C "$CLONE_DIR" commit-tree "$merged_tree" \
    -p "refs/heads/_patched_main" -m "$dep_msg")
  git -C "$CLONE_DIR" push fork "${rebased_sha}:refs/heads/$branch" --force

  # Build the mirror body: a neutral header (no linking #ref to upstream) plus
  # the upstream body minus the @dependabot command footer.
  {
    printf 'Mirrored from upstream PR %s for test-bed dry runs.\n\n' "$num"
    jq -r ".[$i].body" "$TMP/upstream.json" | sanitize_body
  } > "$TMP/body.txt"

  # GitHub caps PR bodies at 65536 chars (grouped dependabot PRs blow past it).
  if [ "$(wc -c < "$TMP/body.txt")" -gt 60000 ]; then
    head -c 60000 "$TMP/body.txt" > "$TMP/body.trim"
    printf '\n\n_(body truncated to fit the GitHub PR length limit)_\n' >> "$TMP/body.trim"
    mv "$TMP/body.trim" "$TMP/body.txt"
  fi

  jq -n --arg t "$title" --arg h "$branch" --arg b main --rawfile body "$TMP/body.txt" \
    '{title:$t, head:$h, base:$b, body:$body}' > "$TMP/payload.json"

  gh api "repos/$FORK/pulls" --input "$TMP/payload.json" \
    --jq '"       -> opened fork PR #\(.number)"'
}

for ((i=0; i<UCOUNT; i++)); do
  mirror_one "$i" || echo "       !! failed to mirror upstream #$(jq -r ".[$i].number" "$TMP/upstream.json") (may already have an open PR)"
  sleep 2
done

git -C "$CLONE_DIR" update-ref -d "refs/heads/_patched_main" 2>/dev/null || true

echo
say "Done. $FORK now mirrors $UPSTREAM's $UCOUNT open dependabot PR(s)."
say "Run the tool against the mirror, e.g.:"
echo "   # Terminal 1 — worker"
echo "   DEPENDABOT_REVIEWER_TOKEN=\"\$(gh auth token)\" ./dependabot-sweeper worker --repos $FORK --accept-author $FORK_OWNER --interval 5m --db /tmp/sweeper.db"
echo "   # Terminal 2 — web dashboard"
echo "   ./dependabot-sweeper web --db /tmp/sweeper.db --listen-addr localhost:8080"
