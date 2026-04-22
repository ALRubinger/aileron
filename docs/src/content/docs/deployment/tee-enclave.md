---
title: "TEE Enclave"
description: "Confidential computing with hardware-isolated enclaves"
---

The enclave service is **optional**. When `AILERON_TEE_PROVIDER` is empty or unset, the server decrypts credentials in-process (Stage 1 behavior). When enabled, the server delegates credential decryption and connector execution to the enclave binary running inside a hardware-isolated TEE.

## Local development

Set `AILERON_TEE_PROVIDER=local` on the **server** service. No enclave binary is needed. The local provider executes connectors in-process with the same ECDH session protocol but no hardware isolation. Useful for testing the attestation and session flow end-to-end.

## Production (Google Confidential Space)

Google Confidential Space runs containers inside AMD SEV-SNP confidential VMs where memory is hardware-encrypted. The enclave binary runs as a container on this VM, and the GCE metadata service provides OIDC attestation tokens that prove the workload identity to the Aileron server.

### Prerequisites

- `gcloud` CLI installed and authenticated with `gcloud init`
- Docker (for building and pushing the enclave image)

### 0. Enable billing on the GCP project

Compute Engine, Artifact Registry, and Confidential Computing APIs all require an active billing account. If you created a new project via `gcloud init`, billing is not enabled by default.

1. Go to [console.cloud.google.com/billing](https://console.cloud.google.com/billing)
2. Select your project — you'll see **"Set the billing account for project"**
3. Choose or create a billing account and save

Or via CLI:

```sh
# List available billing accounts
gcloud billing accounts list

# Link your project to a billing account
gcloud billing projects link $GCP_PROJECT \
  --billing-account=<BILLING_ACCOUNT_ID>
```

### 1. Enable required GCP APIs

```sh
# Uses the default project set by gcloud init.
# If you skipped gcloud init, set this manually: export GCP_PROJECT=your-project-id
export GCP_PROJECT=$(gcloud config get-value project)

gcloud services enable \
  compute.googleapis.com \
  artifactregistry.googleapis.com \
  confidentialcomputing.googleapis.com \
  --project=$GCP_PROJECT
```

### 2. Create an Artifact Registry repository

We use GCP Artifact Registry (not GitHub Container Registry or Docker Hub) because the Confidential Space VM pulls images natively from Artifact Registry using its service account — no extra auth configuration needed. The attestation token includes the image digest from this registry.

Choose a region close to your Railway deployment. See [available Artifact Registry locations](https://docs.cloud.google.com/artifact-registry/docs/repositories/repo-locations) or run `gcloud artifacts locations list`.

```sh
export REGION=us-central1  # change to your preferred region

gcloud artifacts repositories create aileron-enclave \
  --repository-format=docker \
  --location=$REGION \
  --project=$GCP_PROJECT
```

### 3. Build and push the enclave container image

> **CI handles this automatically.** The [Enclave Publish](https://github.com/ALRubinger/aileron/actions/workflows/enclave-publish.yml) workflow triggers when enclave-related code is merged to main (or via manual dispatch). It builds the image, pushes it to Artifact Registry, updates `AILERON_ENCLAVE_IMAGE_DIGEST` on Railway, and restarts the Confidential Space VM. The image digest is printed in the workflow run summary. The manual steps below are for initial setup or one-off builds.

Configure Docker to authenticate with Artifact Registry for pushing:

```sh
gcloud auth configure-docker $REGION-docker.pkg.dev
```

Then build and push:

```sh
export REGISTRY=$REGION-docker.pkg.dev/$GCP_PROJECT/aileron-enclave

# --platform linux/amd64 is required when building on Apple Silicon (ARM64).
# Confidential Space VMs run on AMD64.
docker build --platform linux/amd64 -f cmd/aileron-enclave/Dockerfile -t $REGISTRY/aileron-enclave:latest .
docker push $REGISTRY/aileron-enclave:latest
```

Record the image digest from the push output:

```sh
export IMAGE_DIGEST=$(gcloud artifacts docker images describe \
  $REGISTRY/aileron-enclave:latest \
  --format='value(image_summary.digest)' \
  --project=$GCP_PROJECT)

echo "Image digest: $IMAGE_DIGEST"
```

### 4. Create a service account for the enclave VM

```sh
gcloud iam service-accounts create aileron-enclave \
  --display-name="Aileron Enclave" \
  --project=$GCP_PROJECT

export ENCLAVE_SA=aileron-enclave@$GCP_PROJECT.iam.gserviceaccount.com

# Grant permission to pull images from Artifact Registry.
gcloud artifacts repositories add-iam-policy-binding aileron-enclave \
  --location=$REGION \
  --member="serviceAccount:$ENCLAVE_SA" \
  --role="roles/artifactregistry.reader" \
  --project=$GCP_PROJECT

# Grant permission to generate attestation tokens.
gcloud projects add-iam-policy-binding $GCP_PROJECT \
  --member="serviceAccount:$ENCLAVE_SA" \
  --role="roles/confidentialcomputing.workloadUser"
```

### 5. Reserve a static IP for the enclave

Reserve a static IP before creating the VM so it's assigned at boot and survives reboots:

```sh
gcloud compute addresses create aileron-enclave-ip \
  --region=$REGION \
  --project=$GCP_PROJECT

export ENCLAVE_IP=$(gcloud compute addresses describe aileron-enclave-ip \
  --region=$REGION \
  --project=$GCP_PROJECT \
  --format='value(address)')

echo "Enclave IP: $ENCLAVE_IP"
```

### 6. Configure firewall rules

Restrict traffic to only your Aileron server's static egress IP. Do not open port 8443 to `0.0.0.0/0` — the enclave should only accept connections from your server.

```sh
gcloud compute firewall-rules create allow-aileron-enclave \
  --project=$GCP_PROJECT \
  --allow=tcp:8443 \
  --target-tags=aileron-enclave \
  --source-ranges=<SERVER_EGRESS_IP>/32 \
  --description="Allow traffic to Aileron enclave from server only"
```

Replace `<SERVER_EGRESS_IP>` with your Aileron server's static egress IP.

> **Railway:** Enable static IPs on the server service (requires Pro plan). Find the IP in Railway dashboard → service → Settings → Networking → Static IPs.

### 7. Create the Confidential Space VM

```sh
gcloud compute instances create aileron-enclave \
  --project=$GCP_PROJECT \
  --zone=$REGION-a \
  --machine-type=n2d-standard-2 \
  --confidential-compute-type=SEV \
  --shielded-secure-boot \
  --image-family=confidential-space \
  --image-project=confidential-space-images \
  --service-account=$ENCLAVE_SA \
  --scopes=cloud-platform \
  --metadata="tee-image-reference=$REGISTRY/aileron-enclave:latest" \
  --tags=aileron-enclave \
  --address=$ENCLAVE_IP
```

If your server runs **inside the same GCP VPC**, you can use the internal IP for `AILERON_ENCLAVE_URL` instead (lower latency, no egress charges):

```sh
gcloud compute instances describe aileron-enclave \
  --zone=$REGION-a \
  --format='value(networkInterfaces[0].networkIP)' \
  --project=$GCP_PROJECT
```

### 8. Configure the Aileron server

Set these environment variables on your Aileron server (wherever it's hosted):

| Variable | Value |
|----------|-------|
| `AILERON_TEE_PROVIDER` | `confidential-space` |
| `AILERON_ENCLAVE_URL` | `http://$ENCLAVE_IP:8443` (the static IP from step 7) |
| `AILERON_ENCLAVE_IMAGE_DIGEST` | `$IMAGE_DIGEST` (captured in step 3) |
| `AILERON_GCP_PROJECT_ID` | `$GCP_PROJECT` |
| `AILERON_ENCLAVE_DATA_DIR` | Directory for persistent escrow data (see [Persistent escrow storage](#persistent-escrow-storage) below) |

### 9. Verify

The enclave is only reachable from the Aileron server (per the firewall rule in step 6), so verify through the server's TEE status endpoint:

```sh
curl https://api.yourdomain.com/v1/tee/status
```

Expected response:

```json
{"enabled":true,"provider":"confidential-space","attested":false,"session_active":false}
```

`attested` and `session_active` become `true` after a user unlocks their vault.

## Persistent escrow storage

Escrowed credentials are persisted to disk so they survive enclave restarts. The enclave writes two files inside its data directory:

| File | Purpose |
|------|---------|
| `escrow.key` | AES-256-GCM data encryption key (DEK), generated on first start |
| `escrow.dat` | Encrypted escrow entries (credentials, grant IDs, expiry times) |

The data directory is controlled by `AILERON_ENCLAVE_DATA_DIR`:

| Provider | Default |
|----------|---------|
| `confidential-space` | `/data/enclave` |
| `local` | `~/.aileron/enclave` |

### When to set `AILERON_ENCLAVE_DATA_DIR`

The defaults work for most deployments. Override the directory when:

- **Mounted persistent disk** — Confidential Space VMs use ephemeral boot disks by default. To survive VM recreation (not just restarts), attach a persistent disk and point `AILERON_ENCLAVE_DATA_DIR` to its mount point (e.g. `/mnt/escrow`).
- **Custom local paths** — If `~/.aileron/enclave` conflicts with another tool or you prefer a different location for local development.
- **Shared filesystem** — If running multiple enclave replicas that need to share escrow state (not typical).

### Security notes

- The DEK (`escrow.key`) is a 32-byte random key stored in plaintext on disk. In production, the Confidential Space VM's memory and disk are hardware-encrypted by AMD SEV-SNP, so the key is protected at rest by the TEE.
- `escrow.dat` is encrypted with AES-256-GCM using the DEK. Even if the file is exfiltrated without the key, the contents are unreadable.
- If either file is corrupt or missing, the enclave starts fresh with an empty escrow store. Existing in-memory entries are not affected.
- Expired entries are evicted on startup and periodically (every 10 minutes).

## How attestation works

1. The Aileron server sends a random nonce to the enclave via `POST /v1/tee/attestation`.
2. The enclave fetches an OIDC JWT from the GCE metadata service. This token is signed by Google and contains claims about the workload: container image digest, GCP project ID, and hardware model.
3. The server verifies the JWT signature against Google's JWKS.
4. The server validates: the issuer is `https://accounts.google.com`, the token is not expired, the nonce matches, and the image digest and project ID match the expected values.
5. The browser also verifies the JWT signature via `GET /v1/tee/jwks`, which proxies Google's JWKS endpoint (the browser cannot fetch directly due to CORS). This is a defense-in-depth check — the ECDH key exchange in step 6 is the primary cryptographic protection.
6. On success, the server and enclave perform an ECDH key exchange to establish an encrypted channel.
7. Subsequent requests encrypt credentials with the session key before sending them to the enclave. The enclave decrypts inside hardware-isolated memory, executes the connector, and returns only the structured result.

## Network requirements

| From | To | Port | Purpose |
|------|----|------|---------|
| Aileron server | Enclave VM | 8443 | Attestation, session, execution requests |
| Enclave VM | External APIs | 443 | Gmail, Stripe, Google Calendar, etc. |
| Enclave VM | `metadata.google.internal` | 80 | GCE metadata service (attestation tokens) |
| Aileron server | `accounts.google.com` | 443 | OIDC discovery + JWKS for attestation verification |

## Updating the enclave

CI automates the full update cycle when enclave-related code is merged to main:

1. Builds and pushes the new image to Artifact Registry.
2. Captures the image digest.
3. Updates `AILERON_ENCLAVE_IMAGE_DIGEST` on Railway via the Railway API.
4. Restarts the Confidential Space VM (`gcloud compute instances reset`).
5. The server re-attests against the new image digest on its next attestation request.

You can also trigger the workflow manually from the [Actions tab](https://github.com/ALRubinger/aileron/actions/workflows/enclave-publish.yml) or via CLI:

```sh
gh workflow run enclave-publish.yml
```

For manual updates (e.g., without CI), the steps are:

1. Build and push the new image (see step 3 above).
2. Update `AILERON_ENCLAVE_IMAGE_DIGEST` on the Aileron server.
3. Restart the Confidential Space VM:
   ```sh
   gcloud compute instances reset aileron-enclave --zone=$REGION-a --project=$GCP_PROJECT
   ```
4. The server will re-attest against the new image digest on its next attestation request.

## Hybrid topology

The enclave runs on GCP Confidential Space while your Aileron server, UI, and database remain on your preferred hosting platform. Only the enclave requires GCP — everything else stays where it is.

```
Browser
  │
  ▼
Your hosting platform (server, UI, database)
        │
        │  HTTPS (attestation, session, execute)
        ▼
GCP Confidential Space (enclave)
  ├─ AMD SEV-SNP hardware isolation
  ├─ KEK storage (in encrypted memory)
  ├─ OAuth token exchange
  ├─ Connector execution
  └─ Credential escrow
        │
        │  HTTPS (external API calls)
        ▼
Google, Slack, GitHub APIs
```

> **Railway note:** Railway supports this topology well — enable static IPs on the server service for the firewall rule, set the 4 TEE env vars, and the server connects to the GCP enclave over the public internet. You keep per-branch preview deployments, managed Postgres, and zero-config deploys.

For design rationale, see [ADR-0010: Zero-Knowledge Vault](/adr/0010-zero-knowledge-vault-trust-model), [ADR-0011: TEE Provider SPI](/adr/0011-tee-provider-spi-and-confidential-space), and [ADR-0012: Auto-Escrow & Session Lifetimes](/adr/0012-auto-escrow-and-decoupled-session-lifetimes).
