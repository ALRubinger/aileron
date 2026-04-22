---
title: "CI/CD Pipeline"
description: "Configure GitHub Actions workflows for continuous integration and enclave deployment"
---

Aileron uses GitHub Actions for CI, enclave image publishing, and releases. The CI workflow runs automatically with no special configuration. The Enclave Publish and Release workflows require one-time setup of GCP Workload Identity Federation and GitHub repository secrets.

## Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| **CI** | Push/PR to `main` | Lint, test, build Docker images, integration tests |
| **Enclave Publish** | Push to `main` (enclave paths) or manual | Build enclave image, push to Artifact Registry, update Railway, restart Confidential Space VM |
| **Release** | Tag `v*` | Build and publish binaries via GoReleaser |

## CI workflow

The CI workflow (`.github/workflows/ci.yml`) runs on every push and pull request to `main`. It verifies generated code, runs linting, unit tests (Go and UI), builds all Docker images, and runs integration tests against a full stack.

**Required secret:**

| Secret | Description |
|--------|-------------|
| `CODECOV_TOKEN` | [Codecov](https://codecov.io) upload token for coverage reporting |

No other configuration is needed. The workflow uses the `GITHUB_TOKEN` automatically provided by GitHub Actions.

## Enclave Publish workflow

The Enclave Publish workflow (`.github/workflows/enclave-publish.yml`) triggers on pushes to `main` that touch enclave-related paths (`cmd/aileron-enclave/**`, `enclave/**`, `core/**`, `go.work`, `go.work.sum`), or via manual dispatch. It:

1. Builds the enclave container image.
2. Pushes it to GCP Artifact Registry.
3. Captures the image digest.
4. Updates `AILERON_ENCLAVE_IMAGE_DIGEST` on Railway via the Railway API.
5. Restarts the Confidential Space VM to pick up the new image.

### Prerequisites

Before this workflow can run, you need:

- A GCP project with billing enabled
- GCP Workload Identity Federation configured for GitHub Actions
- A GCP Artifact Registry repository (see [TEE Enclave — Create an Artifact Registry repository](/deployment/tee-enclave#2-create-an-artifact-registry-repository))
- A Confidential Space VM (see [TEE Enclave — Production setup](/deployment/tee-enclave#production-google-confidential-space))
- A Railway project with the Aileron server deployed (see [Railway](/deployment/railway))

### Set variables

All commands below reference these shell variables. Set them once before proceeding. Adjust the values to match your GitHub account, repository, and GCP project:

```sh
export GITHUB_OWNER="ALRubinger"                        # GitHub account or organization
export GITHUB_REPO="${GITHUB_OWNER}/aileron"             # owner/repo on GitHub
export PROJECT_ID=$(gcloud config get-value project)    # your GCP project ID
export SA_NAME="aileron-enclave"                        # service account name
export SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
export WIF_POOL="github"                               # Workload Identity Pool name
export WIF_PROVIDER="aileron"                           # OIDC provider name in the pool
```

### 1. Enable required GCP APIs

```sh
gcloud services enable \
  iamcredentials.googleapis.com \
  secretmanager.googleapis.com \
  artifactregistry.googleapis.com \
  compute.googleapis.com \
  --project="${PROJECT_ID}"
```

### 2. Create a Workload Identity Pool

This allows GitHub Actions to authenticate to GCP without a service account key.

```sh
gcloud iam workload-identity-pools create "${WIF_POOL}" \
  --location="global" \
  --project="${PROJECT_ID}" \
  --display-name="GitHub Actions Pool"
```

### 3. Create an OIDC Provider in the pool

```sh
gcloud iam workload-identity-pools providers create-oidc "${WIF_PROVIDER}" \
  --workload-identity-pool="${WIF_POOL}" \
  --location="global" \
  --project="${PROJECT_ID}" \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --display-name="Aileron GitHub Repo Provider" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repository_owner=assertion.repository_owner,attribute.actor=assertion.actor" \
  --attribute-condition="assertion.repository == '${GITHUB_REPO}'"
```

The attribute condition scopes the provider to this specific repository. Using `assertion.repository_owner == '${GITHUB_OWNER}'` would allow any repo under the account — scope to the specific repo for tighter security.

### 4. Create a service account

Create (or reuse) a service account that the workflow will impersonate:

```sh
gcloud iam service-accounts create "${SA_NAME}" \
  --display-name="Aileron Enclave CI" \
  --project="${PROJECT_ID}"
```

### 5. Grant IAM roles to the service account

The service account needs permission to push images and manage the Confidential Space VM:

```sh
# Push images to Artifact Registry
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
  --member="serviceAccount:${SA_EMAIL}" \
  --role="roles/artifactregistry.writer"

# Reset the Confidential Space VM
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
  --member="serviceAccount:${SA_EMAIL}" \
  --role="roles/compute.instanceAdmin.v1"
```

> For tighter security, replace `roles/compute.instanceAdmin.v1` with a custom role that only grants `compute.instances.reset` and `compute.instances.setMetadata`.

### 6. Bind the identity pool to the service account

This allows GitHub Actions tokens from this repo to impersonate the service account:

```sh
export PROJECT_NUMBER=$(gcloud projects describe "${PROJECT_ID}" --format="value(projectNumber)")

gcloud iam service-accounts add-iam-policy-binding \
  "${SA_EMAIL}" \
  --project="${PROJECT_ID}" \
  --role="roles/iam.workloadIdentityUser" \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${WIF_POOL}/attribute.repository/${GITHUB_REPO}"
```

If you need to look up the project number separately:

```sh
gcloud projects describe "${PROJECT_ID}" --format="value(projectNumber)"
```

### 7. Configure GitHub repository secrets

All values are stored as **secrets** (not variables) to avoid exposing infrastructure details in a public repository. Set these at **Settings → Secrets and variables → Actions → Secrets**.

| Secret | Description | Example |
|--------|-------------|---------|
| `GCP_PROJECT` | GCP project ID | Value of `$PROJECT_ID` |
| `GCP_REGION` | Artifact Registry region | `us-central1` |
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | Full provider resource name | `projects/$PROJECT_NUMBER/locations/global/workloadIdentityPools/$WIF_POOL/providers/$WIF_PROVIDER` |
| `GCP_SERVICE_ACCOUNT` | Service account email | Value of `$SA_EMAIL` |
| `GCP_ENCLAVE_ZONE` | Confidential Space VM zone | `us-central1-a` |
| `GCP_ENCLAVE_INSTANCE` | Confidential Space VM instance name | `aileron-enclave` |
| `RAILWAY_PROJECT_ID` | Railway project UUID | *(see below)* |
| `RAILWAY_SERVICE_ID` | Railway server service UUID | *(see below)* |
| `RAILWAY_ENVIRONMENT_ID` | Railway production environment UUID | *(see below)* |
| `RAILWAY_TOKEN` | Railway API token (account-level) | *(see below)* |

#### Finding the Workload Identity Provider name

```sh
gcloud iam workload-identity-pools providers describe "${WIF_PROVIDER}" \
  --workload-identity-pool="${WIF_POOL}" \
  --location="global" \
  --project="${PROJECT_ID}" \
  --format="value(name)"
```

#### Finding Railway IDs

Railway secrets require **UUIDs**, not display names.

**Railway token:** Project-scoped tokens do not work for the `variableUpsert` mutation. Use an **account-level token**: click your avatar (bottom-left) → Account Settings → Tokens → Create Token.

**Project ID:**

```sh
railway status
```

Or query the API:

```sh
curl -X POST https://backboard.railway.com/graphql/v2 \
  -H "Authorization: Bearer $RAILWAY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"query":"{ me { projects { edges { node { id name }}}}}"}'
```

**Service ID and Environment ID:**

```sh
curl -X POST https://backboard.railway.com/graphql/v2 \
  -H "Authorization: Bearer $RAILWAY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"query":"{ project(id:\"YOUR_PROJECT_UUID\"){ services{ edges{ node{ id name }}} environments{ edges{ node{ id name }}}}}"}'
```

Use the `id` field (UUID) from the `server` service and `production` environment.

### Verify the workflow

Trigger a manual run from the [Actions tab](https://github.com/ALRubinger/aileron/actions/workflows/enclave-publish.yml) or via CLI:

```sh
gh workflow run enclave-publish.yml
```

The workflow run summary shows the registry, tag, and image digest on success.

### Troubleshooting

**"must specify exactly one of workload_identity_provider or credentials_json"** — The `GCP_WORKLOAD_IDENTITY_PROVIDER` secret is not set or is empty. Verify it at Settings → Secrets and variables → Actions.

**Docker login fails with empty access token** — The `google-github-actions/auth` step must include `token_format: access_token`. This is already configured in the workflow file.

**Railway variableUpsert returns errors** — Ensure `RAILWAY_TOKEN` is an account-level token, not a project-scoped token. Project tokens lack permission for the `variableUpsert` mutation.

**Diagnostic commands:**

```sh
# List workload identity pools
gcloud iam workload-identity-pools list \
  --location="global" --project="${PROJECT_ID}"

# List providers in a pool
gcloud iam workload-identity-pools providers list \
  --workload-identity-pool="${WIF_POOL}" \
  --location="global" --project="${PROJECT_ID}"

# Check service account roles
gcloud projects get-iam-policy "${PROJECT_ID}" \
  --flatten="bindings[].members" \
  --filter="bindings.members:${SA_EMAIL}" \
  --format="table(bindings.role)"
```

## Release workflow

The Release workflow (`.github/workflows/release.yml`) triggers when a version tag (`v*`) is pushed. It uses [GoReleaser](https://goreleaser.com) to build and publish release binaries.

No additional secrets are needed — it uses the `GITHUB_TOKEN` automatically provided by GitHub Actions.

```sh
git tag v0.0.42
git push origin v0.0.42
```
