#!/bin/bash
# provision.sh — one-shot GCE provisioning script.
#
# Run this once on your Mac after `gcloud auth login`.  It creates:
#   - A GCP project (or uses an existing one)
#   - Artifact Registry repo + Cloud Build to push the image
#   - Two GCP Secrets (ANTHROPIC_API_KEY, DEPENDABOT_REVIEWER_TOKEN)
#   - A dedicated service account for the VM
#   - A persistent SSD data disk (SQLite + agent logs)
#   - An e2-small VM with the startup script wired up
#   - A firewall rule allowing SSH only from the IAP range
#
# Required env vars (set these before running, or export them):
#   ANTHROPIC_API_KEY         — your Anthropic key
#   DEPENDABOT_REVIEWER_TOKEN — a GitHub token with repo scope on the target org
#   REPO                      — owner/repo to watch, e.g. "myorg/myrepo"
#   ACCEPT_AUTHOR             — GitHub login to accept (space-separated for multiple)
#
# Optional env vars (sensible defaults provided):
#   PROJECT_ID    — GCP project ID to create/use  (default: dependabot-sweeper)
#   REGION        — GCP region                     (default: europe-west1)
#   ZONE          — GCP zone                       (default: europe-west1-b)
#   VM_NAME       — VM instance name               (default: sweeper-vm)
#   VM_MACHINE    — Machine type                   (default: e2-small)
#   DISK_NAME     — Persistent disk name           (default: sweeper-data)
#   DISK_SIZE_GB  — Data disk size in GB           (default: 10)
#   AR_REPO       — Artifact Registry repo name    (default: sweeper)
#   SA_NAME       — Service account name           (default: sweeper-vm-sa)

set -euo pipefail

# ── Helpers ───────────────────────────────────────────────────────────────────
info()  { echo "▶ $*"; }
ok()    { echo "  ✓ $*"; }
err()   { echo "  ✗ ERROR: $*" >&2; exit 1; }
ask()   { read -r -p "  ? $1: " "$2"; }

require_env() {
    local var="$1"
    if [ -z "${!var:-}" ]; then
        echo ""
        echo "  Missing required variable: $var"
        ask "Enter $var" "$var"
        export "${var?}"="${!var}"
    fi
}

gcloud_exists() {
    # Returns 0 if the gcloud describe/list command succeeds (resource exists).
    "$@" &>/dev/null
}

wait_for() {
    # wait_for <description> <max_attempts> <check_command...>
    # Retries the check command every 5s until it succeeds or we give up.
    local desc="$1" max="$2"; shift 2
    local i=0
    echo -n "  Waiting for $desc"
    while ! "$@" &>/dev/null; do
        i=$((i+1))
        [ "$i" -ge "$max" ] && echo " timed out." && return 1
        echo -n "."
        sleep 5
    done
    echo " ready."
}

# ── Configuration ─────────────────────────────────────────────────────────────
PROJECT_ID="${PROJECT_ID:-dependabot-sweeper}"
REGION="${REGION:-europe-west1}"
ZONE="${ZONE:-europe-west1-b}"
VM_NAME="${VM_NAME:-sweeper-vm}"
VM_MACHINE="${VM_MACHINE:-e2-small}"
DISK_NAME="${DISK_NAME:-sweeper-data}"
DISK_SIZE_GB="${DISK_SIZE_GB:-10}"
AR_REPO="${AR_REPO:-sweeper}"
SA_NAME="${SA_NAME:-sweeper-vm-sa}"
SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
AR_HOST="${REGION}-docker.pkg.dev"
IMAGE="${AR_HOST}/${PROJECT_ID}/${AR_REPO}/dependabot-sweeper:latest"

echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║          dependabot-sweeper GCE provisioning                 ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""
echo "  Project : $PROJECT_ID"
echo "  Region  : $REGION  /  Zone: $ZONE"
echo "  VM      : $VM_NAME ($VM_MACHINE)"
echo "  Image   : $IMAGE"
echo ""

# ── Check prerequisites ───────────────────────────────────────────────────────
info "Checking prerequisites..."
command -v gcloud  >/dev/null || err "gcloud not found — install Google Cloud SDK first."
command -v git     >/dev/null || err "git not found."
ok "gcloud and git present."

# ── Credentials ───────────────────────────────────────────────────────────────
info "Authenticating with Google Cloud..."
# This opens a browser for OAuth.  Re-runs are no-ops if already authenticated.
gcloud auth login --quiet
gcloud auth application-default login --quiet
ok "Authenticated."

require_env ANTHROPIC_API_KEY
require_env DEPENDABOT_REVIEWER_TOKEN
require_env REPO
require_env ACCEPT_AUTHOR

# ── Project ───────────────────────────────────────────────────────────────────
info "Setting up GCP project '$PROJECT_ID'..."
if ! gcloud_exists gcloud projects describe "$PROJECT_ID"; then
    echo "  Creating project $PROJECT_ID..."
    if ! gcloud projects create "$PROJECT_ID" --name="dependabot-sweeper" 2>&1; then
        echo ""
        echo "  Project creation failed (this is normal under org policy at mozilla.com)."
        echo "  Please enter an existing GCP project ID you have Owner/Editor access to:"
        ask "Existing project ID" PROJECT_ID
        SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
        IMAGE="${AR_HOST}/${PROJECT_ID}/${AR_REPO}/dependabot-sweeper:latest"
    fi
fi
gcloud config set project "$PROJECT_ID"
ok "Project: $PROJECT_ID"

# ── Align ADC quota project ─────────────────────────────────────────────────────
# `gcloud auth application-default login` stamps the ADC quota project from
# whatever core/project happened to be active at login time — which may be a
# stale or even deleted project. Client-library calls (notably `gcloud builds
# submit`, which uploads source via the storage client) then fail with
# PERMISSION_DENIED. Pin the quota project to the one we just settled on so the
# build is robust regardless of the prior gcloud state.
info "Aligning ADC quota project with '$PROJECT_ID'..."
gcloud auth application-default set-quota-project "$PROJECT_ID" --quiet \
    || echo "  (warning: could not set ADC quota project; if 'builds submit' fails with PERMISSION_DENIED, run: gcloud auth application-default set-quota-project $PROJECT_ID)"
ok "ADC quota project aligned."

# ── Billing ───────────────────────────────────────────────────────────────────
info "Linking billing account..."
BILLING_ACCOUNTS=$(gcloud billing accounts list --format='value(name)' --filter=open=true)
BILLING_COUNT=$(echo "$BILLING_ACCOUNTS" | grep -c . || true)

if [ "$BILLING_COUNT" -eq 1 ]; then
    BILLING_ACCOUNT="$BILLING_ACCOUNTS"
    ok "Single billing account found: $BILLING_ACCOUNT"
elif [ "$BILLING_COUNT" -gt 1 ]; then
    echo "  Multiple billing accounts found:"
    gcloud billing accounts list
    ask "Enter billing account ID (e.g. 0X0X0X-AAAAAA-BBBBBB)" BILLING_ACCOUNT
else
    echo "  No open billing accounts found — you may need to set one up in the console."
    echo "  Skipping billing link; re-run this script after linking manually."
    BILLING_ACCOUNT=""
fi

if [ -n "$BILLING_ACCOUNT" ]; then
    gcloud billing projects link "$PROJECT_ID" \
        --billing-account="$BILLING_ACCOUNT" --quiet || true
    ok "Billing linked."
fi

# ── Enable APIs ───────────────────────────────────────────────────────────────
info "Enabling required APIs (this takes ~60s first time)..."
gcloud services enable \
    compute.googleapis.com \
    cloudbuild.googleapis.com \
    artifactregistry.googleapis.com \
    secretmanager.googleapis.com \
    iam.googleapis.com \
    --quiet
ok "APIs enabled."
# AR API can take a moment to become usable after enablement.
wait_for "Artifact Registry API" 12 \
    gcloud artifacts repositories list --location="$REGION" --project="$PROJECT_ID"

# ── Artifact Registry ─────────────────────────────────────────────────────────
info "Setting up Artifact Registry repo '$AR_REPO' in $REGION..."
if ! gcloud_exists gcloud artifacts repositories describe "$AR_REPO" \
        --location="$REGION"; then
    gcloud artifacts repositories create "$AR_REPO" \
        --repository-format=docker \
        --location="$REGION" \
        --description="dependabot-sweeper container images" \
        --quiet
    ok "AR repo created."
else
    ok "AR repo already exists."
fi

# ── Secrets ───────────────────────────────────────────────────────────────────
info "Creating/updating secrets in Secret Manager..."

create_or_update_secret() {
    local name="$1" value="$2"
    if ! gcloud_exists gcloud secrets describe "$name"; then
        echo -n "$value" | gcloud secrets create "$name" --data-file=- --quiet
        ok "Secret '$name' created."
    else
        echo -n "$value" | gcloud secrets versions add "$name" --data-file=- --quiet
        ok "Secret '$name' updated."
    fi
}

create_or_update_secret "sweeper-anthropic-api-key"  "$ANTHROPIC_API_KEY"
create_or_update_secret "sweeper-reviewer-token"     "$DEPENDABOT_REVIEWER_TOKEN"

# ── Build image ───────────────────────────────────────────────────────────────
info "Building and pushing container image via Cloud Build..."
echo "  Image: $IMAGE"
# Run from the repo root so the build context includes all Go sources.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

gcloud builds submit "$REPO_ROOT" \
    --config="$REPO_ROOT/deploy/cloudbuild.yaml" \
    --substitutions="_IMAGE=${IMAGE}" \
    --quiet
ok "Image built and pushed: $IMAGE"

# ── Service account ───────────────────────────────────────────────────────────
info "Setting up VM service account '$SA_NAME'..."
if ! gcloud_exists gcloud iam service-accounts describe "$SA_EMAIL"; then
    gcloud iam service-accounts create "$SA_NAME" \
        --display-name="dependabot-sweeper VM" \
        --quiet
    # SA takes a few seconds to propagate before IAM bindings can reference it.
    wait_for "service account propagation" 12 \
        gcloud iam service-accounts describe "$SA_EMAIL"
    ok "Service account created."
else
    ok "Service account already exists."
fi

# Grant access to the two secrets.
for secret in sweeper-anthropic-api-key sweeper-reviewer-token; do
    gcloud secrets add-iam-policy-binding "$secret" \
        --member="serviceAccount:${SA_EMAIL}" \
        --role="roles/secretmanager.secretAccessor" \
        --quiet >/dev/null
done

# Grant pull access from AR.
gcloud artifacts repositories add-iam-policy-binding "$AR_REPO" \
    --location="$REGION" \
    --member="serviceAccount:${SA_EMAIL}" \
    --role="roles/artifactregistry.reader" \
    --quiet >/dev/null

ok "Service account permissions set."

# ── VPC network ───────────────────────────────────────────────────────────────
# The mozilla.com org disables default network creation, so we create one explicitly.
info "Setting up VPC network 'sweeper-vpc'..."
if ! gcloud_exists gcloud compute networks describe sweeper-vpc --project="$PROJECT_ID"; then
    gcloud compute networks create sweeper-vpc \
        --subnet-mode=auto \
        --project="$PROJECT_ID" \
        --quiet
    ok "VPC network created."
else
    ok "VPC network already exists."
fi

# ── Static external IP ───────────────────────────────────────────────────────
info "Reserving static IP 'sweeper-web' in $REGION..."
if ! gcloud_exists gcloud compute addresses describe sweeper-web --region="$REGION" --project="$PROJECT_ID"; then
    gcloud compute addresses create sweeper-web \
        --region="$REGION" \
        --project="$PROJECT_ID" \
        --quiet
    ok "Static IP reserved."
else
    ok "Static IP already exists."
fi
STATIC_IP=$(gcloud compute addresses describe sweeper-web \
    --region="$REGION" \
    --project="$PROJECT_ID" \
    --format='get(address)')
ok "IP: $STATIC_IP"

# ── Persistent data disk ──────────────────────────────────────────────────────
info "Creating persistent data disk '$DISK_NAME' (${DISK_SIZE_GB}GB)..."
if ! gcloud_exists gcloud compute disks describe "$DISK_NAME" --zone="$ZONE"; then
    gcloud compute disks create "$DISK_NAME" \
        --size="${DISK_SIZE_GB}GB" \
        --type=pd-ssd \
        --zone="$ZONE" \
        --quiet
    ok "Disk created."
else
    ok "Disk already exists."
fi

# ── Compose file on VM ────────────────────────────────────────────────────────
# We embed the compose file content into the startup script via a heredoc so the
# VM always has the current version without needing a separate transfer step.
COMPOSE_CONTENT=$(cat "$REPO_ROOT/deploy/compose.yaml")

# ── VM ────────────────────────────────────────────────────────────────────────
info "Creating VM '$VM_NAME' ($VM_MACHINE, zone $ZONE)..."

# Build the startup script with the compose file embedded.
STARTUP_TMP=$(mktemp)
cat > "$STARTUP_TMP" <<STARTUP_SCRIPT
#!/bin/bash
# Write compose file to /opt/sweeper/
mkdir -p /opt/sweeper
cat > /opt/sweeper/compose.yaml <<'COMPOSE_EOF'
${COMPOSE_CONTENT}
COMPOSE_EOF

# Run the actual startup logic.
$(cat "$REPO_ROOT/deploy/startup-script.sh")
STARTUP_SCRIPT

if ! gcloud_exists gcloud compute instances describe "$VM_NAME" --zone="$ZONE"; then
    gcloud compute instances create "$VM_NAME" \
        --zone="$ZONE" \
        --machine-type="$VM_MACHINE" \
        --image-family=debian-12 \
        --image-project=debian-cloud \
        --boot-disk-size=20GB \
        --boot-disk-type=pd-ssd \
        --disk="name=${DISK_NAME},device-name=sweeper-data,mode=rw,auto-delete=no" \
        --service-account="$SA_EMAIL" \
        --scopes=cloud-platform \
        --network=sweeper-vpc \
        --address=sweeper-web \
        --metadata-from-file="startup-script=${STARTUP_TMP}" \
        --metadata="sweeper-image=${IMAGE},sweeper-repo=${REPO},sweeper-accept-author=${ACCEPT_AUTHOR}" \
        --tags=sweeper-vm \
        --quiet
    ok "VM created."
else
    ok "VM already exists — updating metadata and restarting..."
    gcloud compute instances add-metadata "$VM_NAME" \
        --zone="$ZONE" \
        --metadata="sweeper-image=${IMAGE},sweeper-repo=${REPO},sweeper-accept-author=${ACCEPT_AUTHOR}" \
        --quiet
    # Replace the startup script and re-run it.
    gcloud compute instances add-metadata "$VM_NAME" \
        --zone="$ZONE" \
        --metadata-from-file="startup-script=${STARTUP_TMP}" \
        --quiet
    gcloud compute ssh "$VM_NAME" --zone="$ZONE" --tunnel-through-iap \
        --command="sudo bash /var/run/google.startup.script" --quiet || true
fi

rm -f "$STARTUP_TMP"

# ── Firewall ──────────────────────────────────────────────────────────────────
info "Setting up firewall rules..."
if ! gcloud_exists gcloud compute firewall-rules describe allow-iap-ssh-sweeper --project="$PROJECT_ID"; then
    gcloud compute firewall-rules create allow-iap-ssh-sweeper \
        --allow=tcp:22 \
        --source-ranges="35.235.240.0/20" \
        --target-tags=sweeper-vm \
        --network=sweeper-vpc \
        --description="Allow IAP SSH to sweeper VMs" \
        --project="$PROJECT_ID" \
        --quiet
    ok "SSH firewall rule created."
else
    ok "SSH firewall rule already exists."
fi
if ! gcloud_exists gcloud compute firewall-rules describe allow-web-sweeper --project="$PROJECT_ID"; then
    # Public by design: the dashboard is a read-only, open-source prototype with
    # no admin interface — every route is a GET and it exposes only PR/CI triage
    # state. 0.0.0.0/0 is intentional, not an oversight.
    gcloud compute firewall-rules create allow-web-sweeper \
        --allow=tcp:8080 \
        --source-ranges="0.0.0.0/0" \
        --target-tags=sweeper-vm \
        --network=sweeper-vpc \
        --description="Public read-only dashboard (intentional; no admin interface)" \
        --project="$PROJECT_ID" \
        --quiet
    ok "Web firewall rule created (port 8080 public)."
else
    ok "Web firewall rule already exists."
fi

# ── Done ──────────────────────────────────────────────────────────────────────
echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║                    Provisioning complete!                    ║"
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""
echo "  VM is starting up — the startup script installs Docker, mounts"
echo "  the disk, fetches secrets, and starts the worker + web services."
echo "  This typically takes 2–3 minutes."
echo ""
echo "  Dashboard  : http://${STATIC_IP}:8080  (stable — survives stop/start)"
echo ""
echo "  To tail logs:"
echo "    gcloud compute ssh $VM_NAME --zone=$ZONE --tunnel-through-iap \\"
echo "      -- 'docker compose -f /opt/sweeper/compose.yaml logs -f'"
echo ""
echo "  Estimated cost: ~\$10–14/month"
echo "    e2-small (sustained-use): ~\$9"
echo "    ${DISK_SIZE_GB}GB pd-ssd data disk:      ~\$0.40"
echo "    20GB pd-ssd boot disk:     ~\$0.80"
echo "    Artifact Registry:         ~\$0 (first 0.5GB free)"
echo ""
