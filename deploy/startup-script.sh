#!/bin/bash
# VM startup script — installed as instance metadata and run on every boot.
#
# Responsibilities:
#   1. Install Docker + compose plugin (idempotent)
#   2. Mount the persistent data disk at /var/lib/sweeper (format on first use)
#   3. Add a 2 GB swapfile if absent (Node.js spikes on e2-small)
#   4. Fetch secrets from GCP Secret Manager → /var/lib/sweeper/.env
#   5. Pull the container image and start/restart both services
#
# The script is re-runnable: if Docker and the disk are already set up it just
# refreshes the .env and restarts the compose stack — so a VM reboot self-heals.
#
# Environment (set via instance metadata by provision.sh):
#   IMAGE          — full AR image reference
#   REPO           — owner/repo watched by the worker
#   ACCEPT_AUTHOR  — space-separated list of accepted GitHub logins
#   PROJECT_ID     — GCP project (for Secret Manager access)

set -euo pipefail

log() { echo "[startup] $*" | tee -a /var/log/sweeper-startup.log; }

# ── 1. Docker ─────────────────────────────────────────────────────────────────
if ! command -v docker &>/dev/null; then
    log "Installing Docker..."
    curl -fsSL https://get.docker.com | sh
    systemctl enable --now docker
    log "Docker installed."
else
    log "Docker already present: $(docker --version)"
fi

# Docker Compose plugin (v2)
if ! docker compose version &>/dev/null; then
    log "Installing Docker Compose plugin..."
    apt-get install -y docker-compose-plugin
    log "Docker Compose plugin installed."
fi

# ── 2. Data disk ──────────────────────────────────────────────────────────────
DATA_DISK_DEV="/dev/disk/by-id/google-sweeper-data"
DATA_DIR="/var/lib/sweeper"

mkdir -p "$DATA_DIR"

if [ -e "$DATA_DISK_DEV" ]; then
    REAL_DEV=$(readlink -f "$DATA_DISK_DEV")
    # Format only if the disk is blank (no filesystem signature).
    if ! blkid "$REAL_DEV" &>/dev/null; then
        log "Formatting data disk $REAL_DEV..."
        mkfs.ext4 -F -L sweeper-data "$REAL_DEV"
        log "Formatted."
    fi

    # Mount if not already mounted.
    if ! mountpoint -q "$DATA_DIR"; then
        log "Mounting $REAL_DEV at $DATA_DIR..."
        mount "$REAL_DEV" "$DATA_DIR"
        # Persist across reboots.
        if ! grep -q "$DATA_DIR" /etc/fstab; then
            echo "LABEL=sweeper-data  $DATA_DIR  ext4  defaults,nofail  0  2" >> /etc/fstab
        fi
        log "Mounted."
    else
        log "Data disk already mounted at $DATA_DIR."
    fi
else
    log "WARNING: data disk device $DATA_DISK_DEV not found — using ephemeral storage."
fi

mkdir -p "$DATA_DIR/agent-logs"
# The container runs as sweeper (UID 1000). Chown so it can write the SQLite DB
# and agent logs onto the bind-mounted volume.
chown -R 1000:1000 "$DATA_DIR"

# ── 3. Swapfile ───────────────────────────────────────────────────────────────
SWAPFILE=/swapfile
if [ ! -f "$SWAPFILE" ]; then
    log "Creating 2 GB swapfile..."
    fallocate -l 2G "$SWAPFILE" || dd if=/dev/zero of="$SWAPFILE" bs=1M count=2048
    chmod 600 "$SWAPFILE"
    mkswap "$SWAPFILE"
    swapon "$SWAPFILE"
    echo "$SWAPFILE none swap sw 0 0" >> /etc/fstab
    log "Swapfile created and enabled."
else
    log "Swapfile already present."
    swapon "$SWAPFILE" 2>/dev/null || true
fi

# ── Ensure gcloud CLI ─────────────────────────────────────────────────────────
# The VM boots from a stock debian-12 image, which may not ship the Google Cloud
# CLI. Both the Secret Manager access (step 4) and the Artifact Registry docker
# login (step 5) need it, so install on demand (no-op if already present).
if ! command -v gcloud &>/dev/null; then
    log "Installing Google Cloud CLI..."
    apt-get update
    apt-get install -y --no-install-recommends apt-transport-https ca-certificates gnupg curl
    curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
        | gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg
    echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" \
        > /etc/apt/sources.list.d/google-cloud-sdk.list
    apt-get update
    apt-get install -y --no-install-recommends google-cloud-cli
    log "Google Cloud CLI installed: $(gcloud --version | head -1)"
else
    log "Google Cloud CLI already present."
fi

# ── 4. Secrets → .env ─────────────────────────────────────────────────────────
# Read metadata-server values injected by provision.sh.
METADATA_ROOT="http://metadata.google.internal/computeMetadata/v1/instance/attributes"
CURL_META() { curl -sf -H "Metadata-Flavor: Google" "$METADATA_ROOT/$1"; }

IMAGE=$(CURL_META sweeper-image || echo "")
REPO=$(CURL_META sweeper-repo || echo "")
ACCEPT_AUTHOR=$(CURL_META sweeper-accept-author || echo "")
PROJECT_ID=$(curl -sf -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/project/project-id" || echo "")

if [ -z "$IMAGE" ] || [ -z "$REPO" ] || [ -z "$PROJECT_ID" ]; then
    log "ERROR: required instance metadata (sweeper-image, sweeper-repo, project-id) missing."
    exit 1
fi

log "Fetching secrets from Secret Manager (project $PROJECT_ID)..."
ANTHROPIC_API_KEY=$(gcloud secrets versions access latest \
    --secret=sweeper-anthropic-api-key --project="$PROJECT_ID" 2>/dev/null)
DEPENDABOT_REVIEWER_TOKEN=$(gcloud secrets versions access latest \
    --secret=sweeper-reviewer-token --project="$PROJECT_ID" 2>/dev/null)

if [ -z "$ANTHROPIC_API_KEY" ] || [ -z "$DEPENDABOT_REVIEWER_TOKEN" ]; then
    log "ERROR: could not fetch secrets from Secret Manager."
    exit 1
fi

# Write secrets to tmpfs (/run is tmpfs on Debian), NOT the persistent data
# disk: the disk is retained by teardown and captured in snapshots, so secrets
# there would outlive the deployment. /run is wiped on reboot and this startup
# script re-fetches and rewrites it on every boot, so the stack self-heals.
ENV_DIR="/run/sweeper"
ENV_FILE="$ENV_DIR/.env"
mkdir -p "$ENV_DIR"
cat > "$ENV_FILE" <<EOF
ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}
DEPENDABOT_REVIEWER_TOKEN=${DEPENDABOT_REVIEWER_TOKEN}
EOF
chmod 600 "$ENV_FILE"
log "Secrets written to $ENV_FILE (tmpfs)."

# ── 5. Compose up ─────────────────────────────────────────────────────────────
COMPOSE_FILE="/opt/sweeper/compose.yaml"

if [ ! -f "$COMPOSE_FILE" ]; then
    log "ERROR: compose file not found at $COMPOSE_FILE."
    exit 1
fi

export IMAGE REPO ACCEPT_AUTHOR

log "Authenticating Docker with Artifact Registry..."
# Use access-token login rather than the credential helper — more reliable on GCE
# because the helper requires gcloud to be on root's PATH when Docker calls it.
gcloud auth print-access-token | \
    docker login -u oauth2accesstoken --password-stdin "${IMAGE%%/*}"

log "Pulling image $IMAGE..."
docker compose -f "$COMPOSE_FILE" pull

log "Starting services..."
docker compose -f "$COMPOSE_FILE" up -d

log "Startup complete."
