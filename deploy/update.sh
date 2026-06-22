#!/bin/bash
# update.sh — release a new version to the GCE VM.
#
# Workflow:
#   1. Build and push a new image via Cloud Build (runs in the cloud, no local
#      Docker daemon needed).
#   2. SSH into the VM via IAP and restart the compose stack with the new image.
#
# The VM's startup script uses `docker compose pull` on each boot, so even a
# VM restart picks up the latest image automatically.
#
# Usage:
#   bash deploy/update.sh [options]
#
# Options:
#   --zone  ZONE     GCE zone        (default: $ZONE env var, or europe-west1-b)
#   --vm    VM_NAME  VM instance     (default: $VM_NAME env var, or sweeper-vm)
#   --tag   TAG      Image tag       (default: latest)
#   --dry-run        Show what would run; don't actually build or restart.

set -euo pipefail

ZONE="${ZONE:-europe-west1-b}"
VM_NAME="${VM_NAME:-sweeper-vm}"
PROJECT_ID="${PROJECT_ID:-}"
REGION="${REGION:-europe-west1}"
AR_REPO="${AR_REPO:-sweeper}"
TAG="latest"
DRY_RUN=false

while [[ $# -gt 0 ]]; do
    case $1 in
        --zone)    ZONE="$2"; shift 2 ;;
        --vm)      VM_NAME="$2"; shift 2 ;;
        --tag)     TAG="$2"; shift 2 ;;
        --dry-run) DRY_RUN=true; shift ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

if [ -z "$PROJECT_ID" ]; then
    PROJECT_ID=$(gcloud config get-value project 2>/dev/null)
fi
if [ -z "$PROJECT_ID" ]; then
    echo "ERROR: PROJECT_ID not set. Run: export PROJECT_ID=<your-project-id>" >&2
    exit 1
fi

AR_HOST="${REGION}-docker.pkg.dev"
IMAGE="${AR_HOST}/${PROJECT_ID}/${AR_REPO}/dependabot-sweeper:${TAG}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "▶ Building image: $IMAGE"
if [ "$DRY_RUN" = true ]; then
    echo "  [dry-run] gcloud builds submit $REPO_ROOT --config=deploy/cloudbuild.yaml --substitutions=_IMAGE=$IMAGE"
else
    gcloud builds submit "$REPO_ROOT" \
        --config="$REPO_ROOT/deploy/cloudbuild.yaml" \
        --substitutions="_IMAGE=${IMAGE}" \
        --quiet
    echo "  ✓ Image pushed."
fi

echo "▶ Updating sweeper-image metadata on $VM_NAME..."
if [ "$DRY_RUN" = true ]; then
    echo "  [dry-run] gcloud compute instances add-metadata $VM_NAME --zone=$ZONE --metadata=sweeper-image=$IMAGE"
else
    gcloud compute instances add-metadata "$VM_NAME" \
        --zone="$ZONE" \
        --metadata="sweeper-image=${IMAGE}"
    echo "  ✓ Metadata updated (future VM reboots will use this image)."
fi

# Sync compose.yaml to the VM. Without this the deploy only updates the image —
# changes to the compose (e.g. --interval, --ignore-check, --concurrency) would
# silently never take effect, leaving the running worker on stale config. The VM
# reads /opt/sweeper/compose.yaml, so we scp to a tmp path and the restart command
# (which runs as root) moves it into place before `compose up`.
echo "▶ Syncing compose.yaml to $VM_NAME..."
if [ "$DRY_RUN" = true ]; then
    echo "  [dry-run] gcloud compute scp $REPO_ROOT/deploy/compose.yaml $VM_NAME:/tmp/sweeper-compose.yaml"
else
    gcloud compute scp "$REPO_ROOT/deploy/compose.yaml" "$VM_NAME:/tmp/sweeper-compose.yaml" \
        --zone="$ZONE" --tunnel-through-iap
    echo "  ✓ compose.yaml uploaded."
fi

echo "▶ Restarting services on $VM_NAME (zone $ZONE)..."
RESTART_CMD="
    sudo cp /tmp/sweeper-compose.yaml /opt/sweeper/compose.yaml &&
    gcloud auth print-access-token | sudo docker login -u oauth2accesstoken --password-stdin ${IMAGE%%/*} &&
    sudo docker pull ${IMAGE} &&
    REPO=\$(curl -sf -H 'Metadata-Flavor: Google' http://metadata.google.internal/computeMetadata/v1/instance/attributes/sweeper-repo) &&
    ACCEPT_AUTHOR=\$(curl -sf -H 'Metadata-Flavor: Google' http://metadata.google.internal/computeMetadata/v1/instance/attributes/sweeper-accept-author) &&
    [ -n \"\$REPO\" ] || { echo 'ERROR: sweeper-repo metadata missing' >&2; exit 1; } &&
    sudo env IMAGE=${IMAGE} REPO=\$REPO ACCEPT_AUTHOR=\$ACCEPT_AUTHOR docker compose -f /opt/sweeper/compose.yaml up -d
"

if [ "$DRY_RUN" = true ]; then
    echo "  [dry-run] SSH to $VM_NAME: cp compose + docker login + pull + compose up"
else
    gcloud compute ssh "$VM_NAME" \
        --zone="$ZONE" \
        --tunnel-through-iap \
        --command="$RESTART_CMD"
    echo "  ✓ Services restarted."
fi

echo ""
echo "  Update complete. View logs:"
echo "    gcloud compute ssh $VM_NAME --zone=$ZONE --tunnel-through-iap \\"
echo "      -- 'docker compose -f /opt/sweeper/compose.yaml logs -f'"
