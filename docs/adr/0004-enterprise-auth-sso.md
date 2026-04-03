# ADR-0004: Enterprise Account Model, SSO/OAuth, and Email/Password Auth

**Status:** Accepted
**Date:** 2026-04-02

## Context

Aileron had no authentication, no user model, and no persistent database. The OpenAPI spec declared Bearer JWT auth, but no validation existed — all endpoints were open. The control plane used an abstract `ActorRef` (agent/human/service) with no concept of user accounts, organizations, or sessions.

For the hosted cloud offering, Aileron needs:

- **Multi-user accounts** with centralized billing, settings, and user management.
- **SSO and OAuth** so users can sign in with identity providers their organization already uses.
- **Email/password signup** as a baseline auth method that works without any external provider.
- **A real database** to persist users, sessions, and enterprise configuration.

The `saas/multitenancy/`, `saas/billing/`, and `saas/infra/` directories existed as empty placeholders, signaling intent but no implementation.

## Decision

### Enterprise as the top-level account

We introduced **Enterprise** as the top-level organizational entity. Every user belongs to exactly one Enterprise (1:1 relationship). Enterprises own workspaces, billing, SSO configuration, and user management.

**Personal vs. organizational Enterprises:** When a user signs up or signs in with a personal email domain (Gmail, Yahoo, Outlook, Proton, iCloud, etc.), a **personal Enterprise** is auto-created (`personal: true`). Users with organizational email domains (e.g. `alice@acme.com`) get an organizational Enterprise. This distinction allows the UI and billing to treat single-user hobby accounts differently from team accounts without requiring a separate account type.

**Why 1:1 (not many-to-many):** A user belonging to multiple Enterprises (like Slack workspaces) was considered but rejected for simplicity. The added complexity of a membership join table, role-per-enterprise, and enterprise switching UI was not justified by current requirements. This can be revisited when multi-org support is needed.

### Surrogate primary key for users

The user's primary key is an opaque, immutable surrogate (`usr_` + UUID). Email is a unique column with an index, used as the **deduplication key** across OAuth providers.

**Why not email as PK:** Email is mutable (people change emails), leaks into foreign keys and logs, and couples identity to a user-controlled value. Consensus schema design recommends surrogate keys for stability. The `usr_` prefix follows the Stripe convention already used throughout Aileron (`ent_`, `ses_`, `pol_`, `int_`, etc.) — any ID is immediately identifiable by type.

### Cross-provider deduplication by email

When a user signs in via any OAuth provider, the lookup chain is:

1. `GetByProviderSubject(provider, subject_id)` — fast path for returning users on the same provider.
2. `GetByEmail(email)` — catches the case where the same person signs in via a different provider (e.g. first Google, now GitHub).
3. If both miss — create a new Enterprise + User.

This ensures that `alice@acme.com` signing in via Google, then later via GitHub (which shares the same email), resolves to the same account.

### Auth provider SPI

Authentication providers are implemented behind an **SPI** (`core/auth/spi.go`), following the same pattern used for the policy engine, vault, connectors, notifier, and audit store. The interface is:

```go
type AuthProvider interface {
    Provider() string
    AuthorizationURL(ctx context.Context, state string) (string, error)
    HandleCallback(ctx context.Context, req CallbackRequest) (*Identity, error)
}
```

A **Registry** resolves providers by name. An **Enforcer** interface checks enterprise-level SSO policies (allowed providers, required SSO, email domain restrictions). Both are designed for extensibility — adding Okta or SAML is a new implementation of `AuthProvider`, not a change to the auth handler.

**Google OAuth** is the first implementation (`core/auth/google/`). It uses `golang.org/x/oauth2` with Google's OpenID Connect userinfo endpoint.

### Email/password authentication

Email/password signup is supported alongside OAuth:

- **Passwords** are hashed with **bcrypt** (cost 12). No separate salt column — bcrypt embeds the salt in the hash output. This is consensus practice; separate salt storage is a pre-2012 pattern.
- **Email verification** is required before the account can be used. A cryptographically random 6-digit code is generated, SHA-256 hashed before storage, and sent via the **Mailer SPI**. Codes expire after 15 minutes.
- **Account status** progresses: `pending_verification` → `active`. Login is rejected for unverified and suspended accounts.
- **Timing attack mitigation:** Login always runs bcrypt comparison even for nonexistent users, preventing email enumeration via response time.

### Mailer SPI

A `Mailer` interface (`SendVerificationCode`) abstracts email delivery. The built-in `LogMailer` prints codes to the server log for development. Production implementations (Mailgun, SES, SendGrid, SMTP) are deferred.

### JWT + refresh token sessions

- **Access tokens:** Short-lived JWTs (15min default) signed with HMAC-SHA256. Claims include `sub` (user ID), `ent_id` (enterprise ID), `email`, and `role`.
- **Refresh tokens:** Cryptographically random, SHA-256 hashed before storage, DB-backed. 7-day default lifetime. Rotated on each refresh — the old session is deleted and a new one created.
- **Cookie transport:** For browser flows, tokens are set as httpOnly cookies (access_token on `/`, refresh_token scoped to `/auth/refresh`). The auth middleware also accepts Bearer headers for API clients.

### Database and migrations

- **PostgreSQL** was chosen as the production database. The store interface pattern (`core/store/store.go`) already existed for in-memory implementations; we added PostgreSQL implementations alongside them.
- **Atlas** (declarative, schema-as-code) manages migrations via an HCL schema file (`core/schema/schema.hcl`). On each container start, the Dockerfile entrypoint runs `atlas schema apply --auto-approve` to converge the database. This is idempotent and safe for every deploy.
- **Auth is opt-in:** Without `AILERON_DATABASE_URL`, the server runs exactly as before with in-memory stores and no auth. This preserves the local MCP gateway use case.

### UI

The SvelteKit UI was updated with:

- **Login page** (`/login`) — email/password form + "Sign in with Google" button.
- **Signup page** (`/signup`) — creates account, redirects to verification.
- **Email verification page** (`/verify-email`) — 6-digit code input.
- **Auth store** (`src/lib/auth.ts`) — reactive state with localStorage persistence, token refresh, and user profile fetching.
- **API client** — attaches Bearer token, auto-refreshes on 401, redirects to login on session expiry.
- **Route protection** — layout-level `$effect` redirects unauthenticated users to `/login`; public paths (login, signup, verify-email, OAuth callback) are excluded.

### OpenAPI as source of truth

All API changes were made in `core/api/openapi.yaml` first, then the Go server interface and types were regenerated with `oapi-codegen`. This is now documented in `CLAUDE.md` as a project-wide rule. The auth handler registers routes directly on the mux (not via the generated interface) because OAuth flows use redirects and cookies rather than JSON request/response — but the endpoints are still declared in the spec for documentation and client generation.

## Consequences

### What this enables

- Users can sign up and sign in to the hosted Aileron control plane.
- Organizations can enforce SSO policies (allowed providers, email domain restrictions).
- The foundation is in place for billing (Enterprise entity), team management (User roles), and workspace scoping.

### What remains to be built

| Area | Status | Notes |
|------|--------|-------|
| **Okta OIDC provider** | Not started | New `AuthProvider` implementation; SSO config admin API needed |
| **Generic SAML 2.0 provider** | Not started | New `AuthProvider` implementation |
| **Production mailer** | SPI defined, stub only | Need Mailgun/SES/SendGrid implementation |
| **Password reset flow** | Not started | Needs "forgot password" endpoint + email with reset token |
| **Resend verification code** | Not started | Endpoint to generate and send a new code |
| **Workspace scoping to Enterprise** | Not started | Existing workspace_id param needs to derive from authenticated Enterprise |
| **Enterprise admin API** | Not started | CRUD for enterprise settings, user invitations, SSO config management |
| **User invitation flow** | Not started | Admin invites user by email → user receives invite → joins Enterprise |
| **Rate limiting on auth endpoints** | Not started | Brute-force protection for login, signup, verification |
| **CSRF protection** | Partial | OAuth state parameter is validated; email/password endpoints rely on SameSite cookies + CORS |
| **Account email change** | Not started | Needs re-verification flow since email is the deduplication key |
| **Multi-enterprise support** | Deferred | 1:1 user-to-enterprise for now; join table needed if this changes |
| **OAuth token linking** | Partial | Cross-provider dedup works via email lookup, but existing users can't explicitly link/unlink providers in a settings page |
| **In-memory auth stores** | Not implemented | Postgres stores exist; in-memory equivalents for dev/test without a database are not yet written |

### Risks and trade-offs

- **Personal email detection** relies on a hardcoded list of consumer domains. This is a heuristic, not a guarantee — some organizations use Gmail (Google Workspace) with custom domains that won't be detected as organizational. The `personal` flag can be corrected later via an admin API.
- **bcrypt cost 12** adds ~250ms per login. This is intentional for security but could become a concern under high concurrent login load. Argon2id is a stronger alternative if we need to tune memory/time tradeoffs independently.
- **`go work sync` in Docker** was replaced with `go mod download` from the `core/` module directly, because workspace resolution was pulling in transitive dependencies requiring Go 1.25+ while CI and the Docker base image run Go 1.24. This means the server Docker build is decoupled from the workspace — it only builds `core/`.
