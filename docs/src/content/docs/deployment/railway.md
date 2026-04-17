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
| `GOOGLE_SIGNIN_CLIENT_ID` | Google sign-in OAuth app (optional) |
| `GOOGLE_SIGNIN_CLIENT_SECRET` | Google sign-in OAuth app (optional) |
| `GOOGLE_CONNECTOR_CLIENT_ID` | Google connected accounts OAuth app (optional) |
| `GOOGLE_CONNECTOR_CLIENT_SECRET` | Google connected accounts OAuth app (optional) |
| `GITHUB_SIGNIN_CLIENT_ID` | GitHub sign-in OAuth app (optional) |
| `GITHUB_SIGNIN_CLIENT_SECRET` | GitHub sign-in OAuth app (optional) |
| `GITHUB_CONNECTOR_CLIENT_ID` | GitHub connected accounts OAuth app (optional) |
| `GITHUB_CONNECTOR_CLIENT_SECRET` | GitHub connected accounts OAuth app (optional) |
| `SLACK_CLIENT_ID` | From Slack app Basic Information (optional) |
| `SLACK_CLIENT_SECRET` | From Slack app Basic Information (optional) |
| `SLACK_SIGNING_SECRET` | From Slack app Basic Information (optional) |
| `ANTHROPIC_API_KEY` | Anthropic API key for draft generation (optional) |
| `AILERON_LLM_MODEL` | LLM model (default: `claude-sonnet-4-6`) (optional) |

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

**Sign-in apps:**
- **Google sign-in:** `https://api.withaileron.ai/auth/google/callback`
- **GitHub sign-in:** `https://api.withaileron.ai/auth/github/callback`

**Connected account apps:**
- **Google connector:** `https://api.withaileron.ai/v1/connect/gmail/callback` and `https://api.withaileron.ai/v1/connect/google_calendar/callback`
- **GitHub connector:** `https://api.withaileron.ai/v1/connect/github_repos/callback`

## 5. Deploy

Push to the branch Railway is watching. The Dockerfile builds the image, and on startup the entrypoint applies schema migrations automatically.

## 6. Verify

```sh
curl https://api.withaileron.ai/v1/health
```
