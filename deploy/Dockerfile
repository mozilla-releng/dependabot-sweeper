# Multi-stage build for dependabot-sweeper.
#
# The runtime image must contain:
#   - the Go binary (dependabot-sweeper)
#   - Node.js  \  the worker subcommand shells out to the `claude` CLI, which
#   - claude CLI /  is an npm package that runs under Node. CGO is not needed.
#   - git         — used for clone/fetch/rebase/push/diff in the impl pipeline
#
# Secrets (ANTHROPIC_API_KEY, DEPENDABOT_REVIEWER_TOKEN) are NEVER baked in;
# they are passed at runtime via an env_file or environment block in compose.

# ── Stage 1: build the embedded SPA ──────────────────────────────────────────
# The Go binary embeds ui/dist via //go:embed, so it must be built from source
# first. Done in its own Node stage; the runtime image carries no build tooling.
FROM node:22-bookworm-slim AS ui

WORKDIR /ui
COPY internal/web/ui/package.json internal/web/ui/package-lock.json ./
RUN npm ci
COPY internal/web/ui/ ./
RUN npm run build

# ── Stage 2: build the Go binary ─────────────────────────────────────────────
FROM golang:1.26 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Use the freshly built SPA from the ui stage, not any stale dist on disk.
COPY --from=ui /ui/dist ./internal/web/ui/dist
# Pure-Go SQLite (modernc.org/sqlite) — no CGO needed.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /dependabot-sweeper ./cmd/dependabot-sweeper

# ── Stage 3: runtime ─────────────────────────────────────────────────────────
FROM node:22-bookworm-slim

# git  — clone / rebase / push operations by the impl pipeline
# ca-certificates — TLS for GitHub + Anthropic APIs
# gh   — the worker opens the replacement PR with `gh pr create --draft`
#        (authenticates via the GH_TOKEN env var injected per worker turn)
# curl/gnupg are needed only to add GitHub's apt repo for gh.
RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        curl \
        gnupg \
    && mkdir -p -m 755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
         -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
         > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# Install the claude CLI globally. The worker invokes it as:
#   claude --print --dangerously-skip-permissions --bare ...
# and authenticates via the ANTHROPIC_API_KEY env var inherited at runtime.
# Pinned for reproducible image builds; bump deliberately.
RUN npm install -g @anthropic-ai/claude-code@2.1.175

COPY --from=builder /dependabot-sweeper /usr/local/bin/dependabot-sweeper

# Runs as root. Accepted trade-off for this single-purpose, single-tenant VM:
# the worker must write the SQLite DB and clone repos onto a root-owned
# bind-mounted disk, so a non-root user would require coordinating host volume
# ownership. Dropping to a non-root user (with a chowned data dir) is a planned
# hardening step to validate during a real deploy, not before.
# The subcommand (worker / web) is chosen per-service in compose.yaml.
ENTRYPOINT ["/usr/local/bin/dependabot-sweeper"]
