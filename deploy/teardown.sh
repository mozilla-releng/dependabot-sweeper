#!/bin/bash
# teardown.sh — delete GCE resources created by provision.sh.
#
# By default:
#   - The VM instance is DELETED.
#   - The persistent DATA DISK is RETAINED (SQLite DB + logs are preserved).
#   - Everything else (project, VPC, firewalls, secrets, AR repo, SA) is left alone.
#     Pass --delete-project to remove the whole project and all resources at once.
#
# Pass --delete-disk to also delete the data disk (DESTROYS ALL DATA).
# Pass --delete-project to also delete the entire GCP project.
#
# Usage:
#   bash deploy/teardown.sh [options]

set -euo pipefail

ZONE="${ZONE:-europe-west1-b}"
VM_NAME="${VM_NAME:-sweeper-vm}"
DISK_NAME="${DISK_NAME:-sweeper-data}"
PROJECT_ID="${PROJECT_ID:-}"

DELETE_DISK=false
DELETE_PROJECT=false

while [[ $# -gt 0 ]]; do
    case $1 in
        --delete-disk)    DELETE_DISK=true; shift ;;
        --delete-project) DELETE_PROJECT=true; shift ;;
        --zone)           ZONE="$2"; shift 2 ;;
        --vm)             VM_NAME="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

if [ -z "$PROJECT_ID" ]; then
    PROJECT_ID=$(gcloud config get-value project 2>/dev/null)
fi

echo "╔══════════════════════════════════════════════════════════════╗"
echo "║              dependabot-sweeper teardown                     ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""
echo "  VM           : $VM_NAME (zone $ZONE)   → WILL BE DELETED"
echo "  Data disk    : $DISK_NAME              → $([ "$DELETE_DISK" = true ] && echo 'WILL BE DELETED' || echo 'will be RETAINED')"
echo "  Project      : $PROJECT_ID             → $([ "$DELETE_PROJECT" = true ] && echo 'WILL BE DELETED' || echo 'will be retained')"
echo ""
read -r -p "  Continue? [y/N] " confirm
[[ "$confirm" =~ ^[Yy]$ ]] || { echo "Aborted."; exit 0; }

# Delete VM
echo ""
echo "▶ Deleting VM $VM_NAME..."
if gcloud compute instances describe "$VM_NAME" --zone="$ZONE" &>/dev/null; then
    gcloud compute instances delete "$VM_NAME" \
        --zone="$ZONE" \
        --keep-disks=all \
        --quiet
    echo "  ✓ VM deleted."
else
    echo "  VM not found, skipping."
fi

# Static IP is retained so it can be reattached to a new VM (and so the URL stays stable).
echo "  Static IP 'sweeper-web' retained — release manually only if no longer needed:"
echo "    gcloud compute addresses delete sweeper-web --region=europe-west1 --project=$PROJECT_ID"

# Optionally delete data disk
if [ "$DELETE_DISK" = true ]; then
    echo "▶ Deleting data disk $DISK_NAME..."
    if gcloud compute disks describe "$DISK_NAME" --zone="$ZONE" &>/dev/null; then
        gcloud compute disks delete "$DISK_NAME" --zone="$ZONE" --quiet
        echo "  ✓ Data disk deleted."
    else
        echo "  Disk not found, skipping."
    fi
else
    echo "  Data disk retained (re-attach to a new VM to recover data)."
fi

# Optionally delete the entire project
if [ "$DELETE_PROJECT" = true ]; then
    echo "▶ Deleting project $PROJECT_ID..."
    gcloud projects delete "$PROJECT_ID" --quiet
    echo "  ✓ Project deleted."
fi

echo ""
echo "  Teardown complete."
