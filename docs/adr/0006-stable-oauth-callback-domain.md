# ADR-0006: Stable OAuth Callback Domain for Branch Deployments

**Status:** Accepted
**Date:** 2026-04-03
**Supersedes:** ADR-0005 (specifically the "Removal of OAuthRedirectURL override" decision)

## Context

ADR-0005 removed the static `OAuthRedirectURL` config field and `GOOGLE_REDIRECT_URL` environment variable, arguing that fully dynamic callback URL derivation from the incoming request host was sufficient for all deployment topologies.

This assumption holds for single-domain deployments and local development, but breaks under Railway's branch deployment model. Railway gives every branch deploy a unique, unpredictable hostname (e.g. `feat-foo-abc123.up.railway.app`). OAuth providers (Google, GitHub) require all callback URLs to be pre-registered in their developer consoles. The dynamic derivation produces a new URL per branch, requiring the operator to manually add each branch hostname to the provider's allowed redirect URIs — not scalable for ephemeral branches.

The root constraint is not in Aileron's code but in the OAuth provider's callback URL whitelist. The solution must reduce the set of registered URIs to one stable URL while preserving full auth functionality on any number of branch deployments.

## Decision

### Stable callback domain as OAuth relay

A single stable domain (e.g. `https://auth.withaileron.ai`) is designated to receive all OAuth callbacks. It is configured as the only registered redirect URI in each OAuth provider's console. Branch deployments route their OAuth flows through this stable domain via a relay mechanism.

This is a configuration-driven approach: no new binary, no new service, no infrastructure change. The stable domain is simply an Aileron deployment — the same Docker image, the same code — running on a domain that never changes.

### OAuth state encodes the originating host

When a branch deployment initiates an OAuth login and `AILERON_OAUTH_CALLBACK_BASE_URL` is set to the stable domain, the auth handler encodes the originating branch hostname into the OAuth state parameter:

```
state = "{32-hex-csrf-token}.{base64url(originating_host)}"
```

The state cookie (used for CSRF validation) stores this full compound value. The CSRF guarantee is unchanged: the provider echoes the state back unchanged, and the handler compares it against the cookie.

When `AILERON_OAUTH_CALLBACK_BASE_URL` is not set, or when login is initiated from the stable domain itself, the state contains only the CSRF token (backwards-compatible with existing flow).

### Relay on the stable domain

The stable domain's callback handler inspects the state parameter on every incoming callback:

1. Parse the state: split on `.` and base64url-decode the suffix to extract `originating_host`.
2. If `originating_host` is non-empty and differs from the current host, enter **relay mode**:
   a. Validate `originating_host` against `AILERON_TRUSTED_ORIGINS` (reject if not trusted).
   b. Redirect to `https://{originating_host}/auth/{provider}/callback?code={code}&state={state}`.
3. If `originating_host` is absent or matches the current host, proceed normally (CSRF cookie check → token exchange → session creation).

The relay carries the OAuth `code` and `state` unchanged. The CSRF check is deferred to the originating deployment, which holds the matching `oauth_state` cookie.

### Token exchange completes on the originating deployment

After the relay redirect, the originating branch deployment receives the callback:
- It validates `state` against its `oauth_state` cookie (CSRF check passes — the cookie was set during login initiation and has not crossed domain boundaries).
- It calls `callbackURL(r, provider)`, which returns the stable domain URL (because `AILERON_OAUTH_CALLBACK_BASE_URL` is configured on the branch too). This `redirect_uri` matches what was registered with the provider, so the token exchange succeeds.
- It creates a session and sets cookies on its own domain.

This design keeps sessions domain-scoped — each deployment maintains independent auth state. The stable domain is stateless with respect to individual user logins (except for direct logins to the stable domain itself).

### Open redirect mitigation

The relay validates `originating_host` against `AILERON_TRUSTED_ORIGINS` before redirecting. This prevents an attacker from crafting a state that relays to an arbitrary host. Patterns support an exact match or a leading `*.` wildcard for subdomain matching (e.g. `*.up.railway.app`).

### New environment variables

| Variable | Required | Description |
|----------|----------|-------------|
| `AILERON_OAUTH_CALLBACK_BASE_URL` | No | Stable domain used as the OAuth `redirect_uri` for all providers. Must include scheme (e.g. `https://auth.withaileron.ai`). Set on every deployment that participates in the relay flow. |
| `AILERON_TRUSTED_ORIGINS` | No | Comma-separated list of hostname patterns allowed as relay targets. Supports `*.subdomain` wildcard prefix (e.g. `*.up.railway.app,withaileron.ai`). Required when the stable domain is handling relay requests. |

Both variables are optional. Without them, callback URL derivation is fully dynamic — the existing behavior is preserved.

### What changed from ADR-0005

| Area | ADR-0005 | ADR-0006 |
|------|----------|----------|
| Callback URL derivation | Always dynamic (from request host) | Dynamic by default; stable base URL when `AILERON_OAUTH_CALLBACK_BASE_URL` is set |
| OAuth state format | 32-hex CSRF token | 32-hex CSRF token, optionally suffixed with `.{base64url(originating_host)}` |
| `OAuthRedirectURL` / `GoogleRedirectURL` | Removed | Re-introduced as provider-agnostic `AILERON_OAUTH_CALLBACK_BASE_URL` |
| Relay logic | None | Stable domain relays callback to originating host when state encodes a different host |
| Open redirect protection | N/A | `AILERON_TRUSTED_ORIGINS` patterns guard relay targets |

## Consequences

### What this enables

- OAuth providers need only one registered redirect URI, ever.
- Branch deployments on Railway (or any platform with dynamic hostnames) work with OAuth out of the box.
- Production, staging, and ephemeral branch deploys all authenticate through the same callback URI.
- No manual per-branch OAuth app configuration.

### What remains to be built

| Area | Status | Notes |
|------|--------|-------|
| **HTTP scheme in relay** | `https` only | Relay always uses `https` for the originating host redirect. HTTP-only deployments cannot participate as relay targets. Acceptable for Railway/cloud; can be addressed by storing scheme in state if needed. |
| **Provider discovery API** | Not started | `GET /auth/providers` endpoint; see ADR-0005 |
| **X.com OAuth** | Deferred | PKCE support ready; blocked by missing email scope |
| **Okta OIDC provider** | Not started | New `AuthProvider` implementation |
| **Generic SAML 2.0 provider** | Not started | New `AuthProvider` implementation |

### Risks and trade-offs

- **Relay adds a round-trip.** The user experiences one extra redirect (stable domain → originating host) during the OAuth callback. This is invisible in practice (sub-100ms HTTP redirect).
- **`AILERON_TRUSTED_ORIGINS` must be kept accurate.** If a trusted pattern is too broad (e.g. `*.railway.app` instead of `*.up.railway.app`), an attacker running a Railway app could craft a relay redirect to their app. Use the most specific patterns that cover your deployments.
- **Both stable and branch deployments must share `AILERON_OAUTH_CALLBACK_BASE_URL`.** The branch deploy needs it to produce the correct `redirect_uri` during token exchange. This is a Railway environment variable shared across services, not a per-deployment secret.
- **The stable domain does not complete the auth flow for branch deployments.** It only relays. Each branch has its own database and session state. This is intentional — it preserves deployment isolation — but means the stable domain's user database does not accumulate branch-deploy users.
