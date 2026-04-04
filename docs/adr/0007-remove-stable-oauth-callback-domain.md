# ADR-0007: Remove Stable OAuth Callback Domain

**Status:** Accepted
**Date:** 2026-04-03
**Supersedes:** ADR-0006

## Context

ADR-0006 introduced a stable `auth.withaileron.ai` subdomain to serve as an OAuth callback relay for Railway branch deployments. This allowed branch deploys with unpredictable hostnames to complete OAuth flows through a single registered redirect URI.

In practice, this added significant complexity (state encoding, relay redirects, trusted origins validation) for a feature that didn't work reliably with Railway's generated hostnames. The relay mechanism was removed in commit 83ee28d, and branch deploy users authenticate via email/password instead.

With the relay removed, the `auth.withaileron.ai` subdomain serves no purpose — it points to the same Railway service as `api.withaileron.ai` and has no distinct routing behavior.

## Decision

Remove the `auth.withaileron.ai` subdomain entirely. All API and auth traffic uses `api.withaileron.ai`. OAuth callback URLs registered with providers become:

- **Google:** `https://api.withaileron.ai/auth/google/callback`
- **GitHub:** `https://api.withaileron.ai/auth/github/callback`

The callback URL derivation in `callbackURL()` remains fully dynamic from the incoming request host — no configuration needed.

## Consequences

- One fewer DNS record and Railway custom domain to maintain.
- OAuth provider consoles reference the same domain as the API.
- Branch deployments continue to work for non-OAuth auth (email/password).
- The `AILERON_OAUTH_CALLBACK_BASE_URL` and `AILERON_TRUSTED_ORIGINS` env vars (already removed in code) are no longer documented.
