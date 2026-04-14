---
title: "Railway"
description: "Deploy Aileron on Railway"
---

This covers Railway-specific setup. Refer to [Cloud Deployment](/deployment/cloud) for the full list of services, environment variables, and domains.

## 1. Create services

In the Railway dashboard, create three services and one database:

| Service | Dockerfile Path | Root Directory |
|---------|----------------|----------------|
| **server** | `core/server/Dockerfile` | `/` (repo root) |
| **ui** | `ui/Dockerfile` | `ui/` |
| **docs** | `docs/Dockerfile` | `docs/` |
| **Postgres** | -- (Railway-managed plugin) | -- |

Link the Postgres plugin to the server service.

## 2. Set environment variables

**Server service:**

| Variable | Value |
|----------|-------|
| `AILERON_DATABASE_URL` | `${{Postgres.DATABASE_URL}}` (Railway variable reference) |
| `AILERON_JWT_SIGNING_KEY` | Generate with `openssl rand -hex 32` |
| `GOOGLE_CLIENT_ID` | From Google Cloud Console (optional) |
| `GOOGLE_CLIENT_SECRET` | From Google Cloud Console (optional) |
| `GITHUB_OAUTH_CLIENT_ID` | From GitHub Developer Settings (optional) |
| `GITHUB_OAUTH_CLIENT_SECRET` | From GitHub Developer Settings (optional) |

**UI service:**

| Variable | Value |
|----------|-------|
| `PUBLIC_API_BASE` | `https://api.withaileron.ai` |
| `PUBLIC_POSTHOG_KEY` | PostHog project API key (optional) |

Branch deploys inherit service variables automatically. OAuth is not available on branch deploys (use email/password login instead).

## 3. Configure domains

Add custom domains in each service's **Settings > Networking > Custom Domain**. Create matching DNS records on Cloudflare (DNS only, not proxied, so Railway can issue TLS certificates).

| Domain | Railway Service | DNS Record |
|--------|----------------|------------|
| `api.withaileron.ai` | server | CNAME to Railway target |
| `app.withaileron.ai` | ui | CNAME to Railway target |

## 4. Register OAuth callback URLs

- **Google:** `https://api.withaileron.ai/auth/google/callback`
- **GitHub:** `https://api.withaileron.ai/auth/github/callback`

## 5. Deploy

Push to the branch Railway is watching. The Dockerfile builds the image, and on startup the entrypoint applies schema migrations automatically.

## 6. Verify

```sh
curl https://api.withaileron.ai/v1/health
```
