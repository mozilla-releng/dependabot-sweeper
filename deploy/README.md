# deploy/ — GCE deployment runbook

Single-VM deployment on Google Cloud Compute Engine.
One `e2-small` instance (~$10–14/month), Docker Compose, SQLite on a persistent disk.

---

## Prerequisites

- `gcloud` CLI installed and working: `gcloud --version`
- Billing account in GCP
- Two secrets available in your shell:
  - `ANTHROPIC_API_KEY` — your Anthropic API key
  - `DEPENDABOT_REVIEWER_TOKEN` — GitHub token with `repo` scope on the target org
- Know which repo to watch (`owner/repo`) and the GitHub login to accept PRs from

---

## First-time deploy (one command)

```bash
# Set required env vars
export ANTHROPIC_API_KEY="..."
export DEPENDABOT_REVIEWER_TOKEN="ghp_..."
export REPO="myorg/myrepo"
export ACCEPT_AUTHOR="my-bot-login"   # space-separated if multiple

# Optional overrides (all have sensible defaults)
# export PROJECT_ID="my-gcp-project"
# export REGION="europe-west1"
# export ZONE="europe-west1-b"

bash deploy/provision.sh
```

The script will:
1. Open a browser for `gcloud auth login`
2. Create (or reuse) a GCP project
3. Enable APIs, create an Artifact Registry repo, and store secrets in Secret Manager
4. Build the container image via Cloud Build (no local Docker needed)
5. Create a service account, a 10 GB SSD data disk, and an `e2-small` VM
6. Set up a firewall rule that only allows SSH from the IAP range

The VM boots, runs the startup script, and starts worker + web services automatically.
First boot takes ~2–3 minutes.

### If project creation is blocked (mozilla.com org policy)
The script will prompt you to enter an existing project ID.
Just make sure you have Owner/Editor on that project.

---

## View the dashboard

`provision.sh` reserves a static IP (`sweeper-web`) and attaches it to the VM, so the address
is stable across stop/start cycles. The IP is printed at the end of provisioning.

To look it up later:

```bash
gcloud compute addresses describe sweeper-web --region=europe-west1 --format='get(address)'
```

Then open `http://<IP>:8080`.

The dashboard is **intentionally public and unauthenticated**: it is a read-only, open-source
prototype with no admin interface — every route is a GET and it exposes only PR/CI triage state.
The `allow-web-sweeper` firewall rule opening `:8080` to `0.0.0.0/0` is by design.

<!-- Design note: the dashboard is intentionally public, read-only, and unauthenticated — it
     has no mutating endpoints and exposes only PR/CI triage state. Served over plain HTTP
     while it is a prototype; HTTPS is deferred until the deployment model is finalised. -->


---

## Release an update

After pushing code changes to `main`:

```bash
bash deploy/update.sh
```

This rebuilds the image via Cloud Build and restarts the compose stack on the VM.
Takes ~2–3 minutes. The `--dry-run` flag shows what would run without doing it.

---

## Stop / start the service

**Stop (pause — VM keeps running, no tokens spent, no data lost):**
```bash
gcloud compute ssh sweeper-vm --zone=europe-west1-b --tunnel-through-iap \
  -- 'sudo docker stop sweeper-worker-1 sweeper-web-1 && sudo docker rm sweeper-worker-1 sweeper-web-1'
```

**Start again after a pause:**
```bash
bash deploy/update.sh    # pulls latest image and starts both containers
```

**Stop the VM entirely (no compute charges, ~$0.10/day for disks only):**
```bash
gcloud compute instances stop sweeper-vm --zone=europe-west1-b
```

**Start the VM again** (startup script re-fetches secrets and restarts containers automatically):
```bash
gcloud compute instances start sweeper-vm --zone=europe-west1-b
```

---

## Tail logs

```bash
# Worker
gcloud compute ssh sweeper-vm --zone=europe-west1-b --tunnel-through-iap \
  -- 'sudo docker logs -f sweeper-worker-1'

# Web service
gcloud compute ssh sweeper-vm --zone=europe-west1-b --tunnel-through-iap \
  -- 'sudo docker logs -f sweeper-web-1'
```

---

## Tear down

```bash
# Delete VM only (data disk and secrets retained)
bash deploy/teardown.sh

# Delete VM + data disk (DESTROYS ALL DATA)
bash deploy/teardown.sh --delete-disk

# Delete VM + data disk + project
bash deploy/teardown.sh --delete-disk --delete-project
```

---

## Costs (approximate)

| Resource | Cost/month |
|---|---|
| e2-small VM (sustained-use discount ~30%) | ~$9 |
| 10 GB pd-ssd data disk | ~$0.40 |
| 20 GB pd-ssd boot disk | ~$0.80 |
| Artifact Registry (first 0.5 GB free) | ~$0 |
| Cloud Build (first 120 min/day free) | ~$0 |
| Secret Manager | ~$0 (< $0.10) |
| **Total** | **~$10–14** |

---

## Scaling

If the VM runs out of memory (Node.js + a large clone can spike):

```bash
# Stop the VM
gcloud compute instances stop sweeper-vm --zone=europe-west1-b

# Resize
gcloud compute instances set-machine-type sweeper-vm \
  --zone=europe-west1-b --machine-type=e2-medium

# Restart
gcloud compute instances start sweeper-vm --zone=europe-west1-b
```

`e2-medium` (2 vCPU, 4 GB RAM) costs ~$27/month with sustained-use.

---

## Architecture

```
Anyone ──HTTP :8080──► GCE VM (e2-small, Debian 12, static public IP)   [read-only dashboard]
Operator ─gcloud IAP SSH─► (same VM)                                      [admin/ops only]
                          │
                          ├─ Docker Compose
                          │   ├─ worker  (dependabot-sweeper worker)
                          │   └─ web     (dependabot-sweeper web, :8080, public)
                          │
                          ├─ /var/lib/sweeper/  (10 GB persistent SSD)
                          │   ├─ sweeper.db        SQLite database
                          │   └─ agent-logs/       claude agent transcripts
                          │
                          ├─ /run/sweeper/.env  (tmpfs — secrets, never on the persistent disk)
                          │
                          └─ Artifact Registry  (container image)

Secrets live in GCP Secret Manager, fetched at boot into tmpfs (not the retained disk).
The service account has secretAccessor + artifactregistry.reader only.
The dashboard (:8080) is public by design (read-only prototype, no admin interface).
SSH is restricted to the IAP range 35.235.240.0/20.
```
