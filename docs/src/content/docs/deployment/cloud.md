---
title: "Cloud Deployment"
description: "Deploy Aileron to any container platform"
---

Aileron is a set of standard Docker containers with no infrastructure-specific assumptions. It runs on any platform that supports containers and PostgreSQL.

## Services

| Service | Dockerfile | Port | Description |
|---------|-----------|------|-------------|
| **server** | `core/server/Dockerfile` | 8080 | API server and auth handler |
| **enclave** | `cmd/aileron-enclave/Dockerfile` | 8443 | TEE enclave binary (optional, only for confidential computing) |
| **ui** | `ui/Dockerfile` | 3000 | SvelteKit management UI |
| **docs** | `docs/Dockerfile` | 80 | API documentation (Scalar) |
| **PostgreSQL** | -- | 5432 | Database (any managed Postgres 16+ works) |

## Domains

Each service needs a domain or URL. The auth domain points to the **server** service; it is not a separate service.

| Domain | Points to | Purpose |
|--------|-----------|---------|
| `api.yourdomain.com` | server | API endpoints (`/v1/*`) |
| `auth.yourdomain.com` | server | OAuth callbacks (`/auth/*`) |
| `app.yourdomain.com` | ui | Management UI |
| `docs.yourdomain.com` | docs | API documentation |

## Environment variables

### Server service

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `AILERON_DATABASE_URL` | Yes | | PostgreSQL connection string |
| `AILERON_JWT_SIGNING_KEY` | Yes | | HMAC signing key for access tokens. Generate with `openssl rand -hex 32` |
| `AILERON_JWT_ISSUER` | No | `aileron` | `iss` claim in issued JWTs |
| `AILERON_ACCESS_TOKEN_TTL` | No | `15m` | Access token lifetime |
| `AILERON_REFRESH_TOKEN_TTL` | No | `168h` | Refresh token lifetime (7 days) |
| `AILERON_UI_REDIRECT_URL` | No | `/` | Redirect destination after successful login |
| `AILERON_AUTO_VERIFY_EMAIL` | No | `false` | Skip email verification on signup. **Never enable in production.** |
| `GOOGLE_CLIENT_ID` | No | | Google OAuth 2.0 client ID |
| `GOOGLE_CLIENT_SECRET` | No | | Google OAuth 2.0 client secret |
| `GITHUB_OAUTH_CLIENT_ID` | No | | GitHub OAuth 2.0 client ID |
| `GITHUB_OAUTH_CLIENT_SECRET` | No | | GitHub OAuth 2.0 client secret |
| `RESEND_API_KEY` | No | | [Resend](https://resend.com) API key for sending verification emails. When unset, codes are printed to the log. |
| `MAIL_FROM` | No | `noreply@withaileron.ai` | Sender address for transactional emails (requires `RESEND_API_KEY`) |
| `AILERON_KEK_SESSION_TTL` | No | `24h` | How long a verified vault passphrase session remains active |
| `AILERON_ESCROW_TTL` | No | `168h` | How long auto-escrowed credentials persist in TEE memory |
| `AILERON_TEE_PROVIDER` | No | *(empty)* | TEE provider: `local` (dev/test), `confidential-space` (production), or empty (disabled) |
| `AILERON_ENCLAVE_URL` | No | | Base URL of the enclave binary. Required when `AILERON_TEE_PROVIDER=confidential-space` |
| `AILERON_ENCLAVE_IMAGE_DIGEST` | No | | Expected container image digest (`sha256:...`) for attestation verification |
| `AILERON_GCP_PROJECT_ID` | No | | Expected GCP project ID for attestation verification |

### UI service

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `PUBLIC_API_BASE` | Yes | `http://localhost:8080` | URL of the server service |
| `PUBLIC_POSTHOG_KEY` | No | | PostHog project API key (passed as Docker build arg) |
| `PUBLIC_POSTHOG_HOST` | No | `https://us.i.posthog.com` | PostHog ingest endpoint (passed as Docker build arg) |

### Docs service

No configuration required.

## Setup steps

1. **Provision PostgreSQL.** Any managed Postgres 16+ service works (AWS RDS, GCP Cloud SQL, Supabase, Neon, Railway, etc.).

2. **Build the container images:**
   ```sh
   docker build -f core/server/Dockerfile -t your-registry/aileron-server .
   docker build -f ui/Dockerfile -t your-registry/aileron-ui ui/
   docker build -f docs/Dockerfile -t your-registry/aileron-docs docs/
   docker build -f cmd/aileron-enclave/Dockerfile -t your-registry/aileron-enclave .  # optional
   ```

3. **Configure environment variables** on each service as listed above.

4. **Configure domains.** Point each domain to its service and provision TLS certificates.

5. **Deploy.** The server entrypoint automatically runs [Atlas](https://atlasgo.io) schema migrations against `AILERON_DATABASE_URL` before starting. Migrations are declarative and idempotent.

6. **Configure OAuth providers** (optional, each is independent):

   **Google** (in the [Google Cloud Console](https://console.cloud.google.com/apis/credentials)):
   - Create an OAuth 2.0 Client ID (Web application type)
   - Add `https://auth.yourdomain.com/auth/google/callback` as an authorized redirect URI

   **GitHub** (in [GitHub Developer Settings](https://github.com/settings/developers)):
   - Create a new OAuth App
   - Set the authorization callback URL to `https://auth.yourdomain.com/auth/github/callback`

7. **Verify:**
   ```sh
   curl https://api.yourdomain.com/v1/health
   ```

## Authentication

Authentication is **opt-in**. When no database is configured, the server runs without auth (suitable for local development).

When `AILERON_DATABASE_URL` is set, the server enables:

- Email/password signup with email verification
- Google and GitHub OAuth sign-in (with Okta and SAML planned)
- Enterprise account model (auto-created on first sign-in)
- JWT-based session management with refresh token rotation
- Cross-provider deduplication (same email resolves to the same account)
