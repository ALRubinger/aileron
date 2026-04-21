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

- A GCP project with billing enabled
- `gcloud` CLI installed and authenticated
- Docker (for building and pushing the enclave image)
- A container registry (Artifact Registry or Container Registry)

### 1. Enable required GCP APIs

```sh
export GCP_PROJECT=your-project-id

gcloud services enable \
  compute.googleapis.com \
  artifactregistry.googleapis.com \
  confidentialcomputing.googleapis.com \
  --project=$GCP_PROJECT
```

### 2. Create an Artifact Registry repository

```sh
export REGION=us-central1

gcloud artifacts repositories create aileron-enclave \
  --repository-format=docker \
  --location=$REGION \
  --project=$GCP_PROJECT
```

### 3. Build and push the enclave container image

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
  --confidential-compute \
  --min-cpu-platform="AMD Milan" \
  --image-family=confidential-space \
  --image-project=confidential-space-images \
  --service-account=$ENCLAVE_SA \
  --scopes=cloud-platform \
  --metadata="tee-image-reference=$REGISTRY/aileron-enclave:latest,tee-container-log-redirect=true,tee-env-AILERON_TEE_PROVIDER=confidential-space,tee-env-AILERON_ENCLAVE_PORT=8443" \
  --tags=aileron-enclave
```

### 6. Configure firewall rules

```sh
gcloud compute firewall-rules create allow-aileron-enclave \
  --project=$GCP_PROJECT \
  --allow=tcp:8443 \
  --target-tags=aileron-enclave \
  --source-ranges=0.0.0.0/0 \
  --description="Allow traffic to Aileron enclave"
```

For production, restrict `--source-ranges` to the IP range of your Aileron server.

### 7. Get the enclave VM's internal IP

```sh
export ENCLAVE_IP=$(gcloud compute instances describe aileron-enclave \
  --zone=$REGION-a \
  --format='value(networkInterfaces[0].networkIP)' \
  --project=$GCP_PROJECT)

echo "Enclave URL: http://$ENCLAVE_IP:8443"
```

If the Aileron server runs outside the same VPC, use the external IP or set up a load balancer with TLS.

### 8. Configure the Aileron server

Set these environment variables on the Aileron server service:

| Variable | Value |
|----------|-------|
| `AILERON_TEE_PROVIDER` | `confidential-space` |
| `AILERON_ENCLAVE_URL` | `http://<ENCLAVE_IP>:8443` (or internal DNS) |
| `AILERON_ENCLAVE_IMAGE_DIGEST` | The `sha256:...` digest from step 3 |
| `AILERON_GCP_PROJECT_ID` | Your GCP project ID |

### 9. Verify

```sh
curl http://$ENCLAVE_IP:8443/health
curl https://api.yourdomain.com/v1/tee/status
```

Expected response:

```json
{"enabled":true,"provider":"confidential-space","attested":false,"session_active":false}
```

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

## Hybrid topology: Railway + GCP

The recommended production topology keeps the Aileron server, UI, docs, and Postgres on Railway (preserving per-branch preview deployments and managed infrastructure), with only the enclave running on GCP Confidential Space.

```
Browser
  │
  ▼
Railway (app.withaileron.ai)
  ├─ server (API, auth, draft pipeline)
  ├─ ui (SvelteKit)
  ├─ docs (Astro)
  └─ Postgres
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

### Firewall configuration

Restrict the enclave VM to accept traffic only from Railway's egress IPs:

```sh
# Get Railway's egress IPs from their docs or support.
# As of 2026, Railway uses a set of static egress IPs per region.

gcloud compute firewall-rules update allow-aileron-enclave \
  --project=$GCP_PROJECT \
  --source-ranges=<RAILWAY_EGRESS_IP_1>/32,<RAILWAY_EGRESS_IP_2>/32 \
  --allow=tcp:8443
```

Contact Railway support or check their [networking docs](https://docs.railway.com/reference/networking) for current egress IP ranges.

### Railway server configuration

Set these environment variables on the Railway server service:

| Variable | Value |
|----------|-------|
| `AILERON_TEE_PROVIDER` | `confidential-space` |
| `AILERON_ENCLAVE_URL` | `http://<ENCLAVE_INTERNAL_IP>:8443` |
| `AILERON_ENCLAVE_IMAGE_DIGEST` | `sha256:...` (from step 3 above) |
| `AILERON_GCP_PROJECT_ID` | Your GCP project ID |

If the enclave VM is in a different VPC or region than Railway's egress, use the external IP with TLS (terminate at the enclave or use a load balancer).

For design rationale, see [ADR-0010: Zero-Knowledge Vault](/adr/0010-zero-knowledge-vault-trust-model), [ADR-0011: TEE Provider SPI](/adr/0011-tee-provider-spi-and-confidential-space), and [ADR-0012: Auto-Escrow & Session Lifetimes](/adr/0012-auto-escrow-and-decoupled-session-lifetimes).
