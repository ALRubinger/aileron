# ADR-0005: GitHub OAuth Provider and Auth SPI Evolution

**Status:** Accepted
**Date:** 2026-04-03
**Supersedes:** [ADR-0004](0004-enterprise-auth-sso.md) (Enterprise Account Model, SSO/OAuth, and Email/Password Auth)

## Context

ADR-0004 established the auth SPI, enterprise model, and Google OAuth as the first provider. The system was designed for pluggable providers, but only one existed. Adding a second provider (GitHub) surfaced two gaps in the SPI and one unnecessary Google-specific coupling in the handler configuration.

Additionally, X.com (Twitter) was evaluated as a third provider. Its OAuth 2.0 API requires PKCE and does not provide user email — a field the system relies on for cross-provider deduplication, enterprise creation, and domain enforcement. X.com was deferred, but the SPI needed to evolve to support PKCE for when it is eventually added.

## Decision

### SPI evolution: AuthorizationResult and ExtraState

The `AuthProvider.AuthorizationURL` method now returns `*AuthorizationResult` instead of a bare URL string:

```go
type AuthorizationResult struct {
    URL        string
    ExtraState string // opaque, persisted by handler across redirect
}

type CallbackRequest struct {
    Code        string
    State       string
    RedirectURL string
    ExtraState  string // from AuthorizationResult, if any
}
```

`ExtraState` is opaque provider data that the handler persists in an `oauth_extra` httpOnly cookie across the OAuth redirect, then supplies back to the provider in `CallbackRequest.ExtraState`. This supports providers that require PKCE (where the `code_verifier` must survive the redirect) without the SPI leaking PKCE-specific concepts. Providers that don't need it (Google, GitHub) leave `ExtraState` empty.

This is a breaking change to the `AuthProvider` interface. All implementors (Google provider, test fakes) were updated atomically.

### GitHub OAuth provider

GitHub is the second `AuthProvider` implementation (`core/auth/github/`). It uses `golang.org/x/oauth2` with GitHub's OAuth 2.0 endpoints (not OpenID Connect — GitHub does not support it).

**Scopes:** `user:email`, `read:user`

**Email resolution:** Many GitHub users set their email to private, causing the `GET /user` endpoint to return `email: null`. The provider handles this with a two-step lookup:

1. `GET https://api.github.com/user` — if `email` is present, use it.
2. If `email` is null, `GET https://api.github.com/user/emails` — find the entry with `primary: true` and `verified: true`.
3. If no primary verified email exists, the provider returns an error. Email is required by the system for cross-provider deduplication and enterprise assignment (as established in ADR-0004).

**Subject:** GitHub's numeric user `id`, converted to string.

**Display name:** The `name` field, falling back to `login` (GitHub username) when `name` is empty.

**Environment variables:** `GITHUB_OAUTH_CLIENT_ID` and `GITHUB_OAUTH_CLIENT_SECRET`. The `GITHUB_OAUTH_` prefix (rather than `GITHUB_`) avoids collision with the GitHub connector's `GITHUB_TOKEN` environment variable.

### Removal of OAuthRedirectURL override

ADR-0004's handler accepted an `OAuthRedirectURL` config field (mapped from `GOOGLE_REDIRECT_URL`) as a static override for the OAuth callback URL. This was Google-specific and unnecessary — the `callbackURL()` method already derives the callback URL dynamically from the incoming request's `Host`, `X-Forwarded-Host`, and `X-Forwarded-Proto` headers. This dynamic derivation works correctly per-provider for any deployment topology (localhost, Railway branch deploys, custom domains behind a reverse proxy).

The static override has been removed. The `GoogleRedirectURL` config field and `GOOGLE_REDIRECT_URL` environment variable are no longer used.

### X.com (Twitter) deferred

X.com was evaluated and deferred because its OAuth 2.0 API does not provide user email through standard scopes. Six code paths in the auth handler assume `Identity.Email` is populated:

- Cross-provider dedup (`GetByEmail`)
- Enterprise auto-creation (email domain determines personal vs. organizational)
- Enterprise slug generation (from email username)
- Billing email assignment
- Enforcer domain restriction checks
- JWT claims

Supporting X.com would require a "complete your profile" post-login flow to collect and verify email — a significant addition touching the handler, UI, and user model. The SPI changes in this ADR (PKCE support via `ExtraState`) lay the groundwork so that X.com can be added later without further SPI evolution.

### UI changes

The login and signup pages now show both "Sign in with Google" and "Sign in with GitHub" buttons. The buttons link to `/auth/google/login` and `/auth/github/login` respectively — the same `{provider}` path parameter pattern established in ADR-0004.

## Consequences

### What this enables

- Users can sign in with GitHub in addition to Google and email/password.
- The SPI supports providers requiring PKCE (e.g. X.com) without further interface changes.
- Callback URL derivation is fully dynamic — no per-provider URL configuration needed.
- The pattern for adding new OAuth providers is proven: implement `AuthProvider`, add config fields, register in `app.go`, add a UI button.

### What changed from ADR-0004

| Area | ADR-0004 | ADR-0005 |
|------|----------|----------|
| `AuthorizationURL` return type | `(string, error)` | `(*AuthorizationResult, error)` |
| `CallbackRequest` fields | `Code`, `State`, `RedirectURL` | Added `ExtraState` |
| OAuth providers | Google only | Google + GitHub |
| `OAuthRedirectURL` config | Static override from `GOOGLE_REDIRECT_URL` | Removed; dynamic derivation only |
| Login/signup UI | Google button only | Google + GitHub buttons |

### What remains to be built

| Area | Status | Notes |
|------|--------|-------|
| **X.com (Twitter) OAuth** | Deferred | Requires post-login email collection flow; PKCE support is ready in SPI |
| **Okta OIDC provider** | Not started | New `AuthProvider` implementation |
| **Generic SAML 2.0 provider** | Not started | New `AuthProvider` implementation |
| **Provider discovery API** | Not started | `GET /auth/providers` endpoint returning enabled providers, so UI can dynamically show/hide buttons instead of hardcoding them |
