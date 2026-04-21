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

> These are the manual steps for initial setup. For ongoing deployments, this should be automated in CI on merge to main — see [issue #211](https://github.com/ALRubinger/aileron/issues/211).

Configure Docker to authenticate with Artifact Registry for pushing:

```sh
gcloud auth configure-docker $REGION-docker.pkg.dev
```

Then build and push:

```sh
export REGISTRY=$REGION-docker.pkg.dev/$GCP_PROJECT/aileron-enclave

docker build -f cmd/aileron-enclave/Dockerfile -t $REGISTRY/aileron-enclave:latest .
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

### 5. Create the Confidential Space VM

```sh
gcloud compute instances create aileron-enclave \
  --project=$GCP_PROJECT \
  --zone=$REGION-a \
  --machine-type=n2d-standard-2 \
  --confidential-compute-type=SEV \
  --image-family=confidential-space \
  --image-project=confidential-space-images \
  --service-account=$ENCLAVE_SA \
  --scopes=cloud-platform \
  --metadata="tee-image-reference=$REGISTRY/aileron-enclave:latest,tee-container-log-redirect=true,tee-env-AILERON_TEE_PROVIDER=confidential-space,tee-env-AILERON_ENCLAVE_PORT=8443" \
  --tags=aileron-enclave
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

### 7. Assign a static external IP

Reserve a static IP so the enclave address survives reboots:

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

Assign it to the VM:

```sh
# Remove the existing (empty) access config
gcloud compute instances delete-access-config aileron-enclave \
  --zone=$REGION-a \
  --access-config-name="external-nat" \
  --project=$GCP_PROJECT

# Assign the static IP
gcloud compute instances add-access-config aileron-enclave \
  --zone=$REGION-a \
  --access-config-name="external-nat" \
  --address=$ENCLAVE_IP \
  --project=$GCP_PROJECT
```

The `$ENCLAVE_IP` from this step is what you'll use for `AILERON_ENCLAVE_URL` in the next step. If your server runs **inside the same GCP VPC**, you can use the internal IP instead for lower latency and no egress charges:

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

## How attestation works

1. The Aileron server sends a random nonce to the enclave via `POST /v1/tee/attestation`.
2. The enclave fetches an OIDC JWT from the GCE metadata service. This token is signed by Google and contains claims about the workload: container image digest, GCP project ID, and hardware model.
3. The server verifies the JWT signature against Google's JWKS.
4. The server validates: the issuer is `https://confidentialcomputing.googleapis.com`, the token is not expired, the nonce matches, and the image digest and project ID match the expected values.
5. On success, the server and enclave perform an ECDH key exchange to establish an encrypted channel.
6. Subsequent requests encrypt credentials with the session key before sending them to the enclave. The enclave decrypts inside hardware-isolated memory, executes the connector, and returns only the structured result.

## Network requirements

| From | To | Port | Purpose |
|------|----|------|---------|
| Aileron server | Enclave VM | 8443 | Attestation, session, execution requests |
| Enclave VM | External APIs | 443 | Gmail, Stripe, Google Calendar, etc. |
| Enclave VM | `metadata.google.internal` | 80 | GCE metadata service (attestation tokens) |
| Aileron server | `accounts.google.com` | 443 | OIDC discovery + JWKS for attestation verification |

## Updating the enclave

1. Build and push the new image.
2. Record the new image digest.
3. Update `AILERON_ENCLAVE_IMAGE_DIGEST` on the Aileron server.
4. Restart the Confidential Space VM:
   ```sh
   gcloud compute instances stop aileron-enclave --zone=$REGION-a --project=$GCP_PROJECT
   gcloud compute instances start aileron-enclave --zone=$REGION-a --project=$GCP_PROJECT
   ```
5. The server will re-attest against the new image digest on its next attestation request.

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
