# Aileron
_Stay on course. The missing protection layer between your agents and the real world._

![GitHub License](https://img.shields.io/github/license/ALRubinger/aileron?style=for-the-badge)
![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/ALRubinger/aileron/ci.yml?style=for-the-badge&logo=github)
![Codecov](https://img.shields.io/codecov/c/github/ALRubinger/aileron?style=for-the-badge)

**Aileron is a deterministic execution plane for AI agents.** It owns your identity, enforces policy, and executes irreversible actions — so agents never hold your credentials or act without authorization.

Agents decide what to do. Aileron decides whether to do it, and executes it safely.

---

## The Problem

AI agents are acting on our behalf: sending emails, booking meetings, paying invoices. The problem isn't capability. It's trust. An agent that's useful is an agent that's risky.

Existing "control planes" for agents (Fiddler, Galileo, Unleash) monitor and block — but they still rely on the agent holding credentials and executing actions. Prompt injection, context compression, or model errors can bypass safety rules because the enforcement layer is advisory, not structural.

The result is a forced choice: give the agent enough permission to be powerful, or restrict it enough to feel safe. Neither is satisfying.

## The Solution

Aileron separates **intent** from **execution**. Agents submit structured intents (send this email, schedule this meeting, make this purchase). Aileron owns the credentials, evaluates deterministic policy, and executes the action itself — returning only safe, structured results to the agent.

The agent never holds your Gmail token, calendar credentials, or payment instruments. Aileron executes on your behalf, but your secrets are encrypted with a key only you know.

```
Agent Host (Claude Code, OpenClaw, etc.)
  │
  │  MCP (stdio)
  │
  ▼
Aileron Execution Plane
  ├── Intent Tools           list_inbox_briefs, send_email_intent, request_purchase, ...
  ├── Policy Engine          deterministic rules per action (no LLM in enforcement)
  ├── Approval Orchestrator  routes to humans when required
  ├── Credential Vault       zero-knowledge encrypted storage (user-derived keys)
  └── Audit Store            immutable record of every decision and execution
  │
  ├──► Gmail API         (Aileron sends the email)
  ├──► Google Calendar   (Aileron creates the event)
  └──► Stripe Issuing    (Aileron mints the virtual card)
```

## How It Works

**1. Connect your accounts**

Open the Protected Actions catalog and connect your Gmail, Google Calendar, or payment accounts via OAuth. Aileron stores refresh tokens in its zero-knowledge vault — encrypted with a key only you control. Agents never see them. Neither does Aileron.

**2. Agents submit intents**

The agent calls Aileron's intent tools: `list_inbox_briefs`, `draft_reply`, `send_email_intent`, `create_event_intent`, `request_purchase`. These are the only tools the agent sees.

**3. Policy evaluates every irreversible action**

Read-only operations (list inbox, check calendar) flow freely. Irreversible actions (send email, create invite, issue payment) are evaluated against deterministic policies. No LLM in the enforcement loop.

**4. Humans approve high-risk actions**

If policy requires approval, Aileron holds the action and notifies approvers. Defaults: internal emails auto-send, external emails require review, payments above threshold require approval.

**5. Aileron executes**

Once approved (or auto-approved by policy), Aileron executes the action itself using the connected account's credentials. The agent receives a structured result — never raw credentials.

**6. Everything is logged**

Every intent, policy decision, approval, and execution is recorded in an immutable audit trail. The execution graph becomes indispensable for compliance and trust.

## For Organizations

Aileron gives organizations centralized control over agent activity across teams.

- **Protected Actions catalog.** A curated set of irreversible actions (email, calendar, payments) that Aileron owns and executes. Connect once, and all agents benefit.
- **Identity ownership.** OAuth tokens and payment instruments live in the zero-knowledge vault, encrypted with user-derived keys. Teams use agents without handling credentials — and Aileron can't access them either.
- **Policy governance.** Define rules that apply across all agent activity — internal/external recipient controls, spend limits, vendor allowlists, time-of-day rules.
- **Multi-agent hub.** Multiple agents share the same identity, policies, and audit trail. No per-agent credential management.
- **Compliance.** An immutable execution graph records every action, policy decision, and approval for review and export.

## Zero-Knowledge Vault

Aileron's credential vault uses a zero-knowledge architecture: your secrets are encrypted with a key derived from a passphrase that only you know. Aileron stores the encrypted ciphertext and the Argon2id salt — never the key itself.

**How it works:**

1. You set a vault passphrase. Aileron derives a 256-bit Key Encryption Key (KEK) using Argon2id, stores only the salt and a verification blob, then discards the KEK from memory.
2. When you connect an external account (Gmail, Calendar, etc.), the OAuth refresh token is encrypted with your KEK before storage. The database holds only ciphertext.
3. To execute actions, you verify your passphrase to unlock a time-limited session (default: 30 minutes). The KEK is held in memory only for the session duration.
4. When an agent triggers an execution, Aileron decrypts the credential with the session KEK, calls the external API, and discards the plaintext.

**What this means:**

- A database breach yields only ciphertext — useless without your passphrase.
- Aileron operators cannot read your credentials, even with full database access.
- Hosting providers (AWS, Railway, etc.) see only encrypted data.

**What's next:** [Confidential computing](https://github.com/ALRubinger/aileron/issues/52) will move credential decryption and connector execution into a hardware-isolated enclave (AWS Nitro Enclaves), so plaintext credentials never exist on the host — even in memory. See [ADR-0010](docs/adr/0010-zero-knowledge-vault-trust-model.md) for the full trust model.

## Configuration

Aileron is configured with an `aileron.yaml` file that declares downstream MCP servers, credential references, and policy mappings.

```yaml
version: "1"
downstream_servers:
  - name: "github"
    command: ["npx", "-y", "@modelcontextprotocol/server-github"]
    env:
      GITHUB_PERSONAL_ACCESS_TOKEN: "vault://connectors/github/default"
    policy_mapping:
      tool_prefix: "git"
  - name: "filesystem"
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/workspace"]
```

Each downstream server entry specifies the command to launch it, environment variables (with optional vault references for secrets), and policy mapping configuration.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AILERON_ADDR` | `:8080` | Address the control plane server listens on |
| `AILERON_CONFIG` | `aileron.yaml` | Path to the configuration file |
| `REGISTRY_REFRESH_INTERVAL` | `15m` | How often the MCP Registry server list is refreshed in the background. Accepts any Go duration string (e.g. `5m`, `1h`). The server prefetches the full registry on startup and refreshes on this interval. |
| `GITHUB_TOKEN` | | GitHub personal access token, seeded into the vault at startup |

## Current Status

The execution plane architecture is being built incrementally:

- **Connected accounts** — users can connect external services (Gmail, Google Calendar) via OAuth. Tokens stored in vault, agents never see them.
- **Policy engine** evaluates deterministic rules per action (allow, deny, require approval, allow with modifications)
- **Approval orchestrator** manages human-in-the-loop workflows with approve/deny/modify
- **Zero-knowledge credential vault** — user secrets are encrypted with a passphrase-derived key (Argon2id + AES-256-GCM). Aileron operators and hosting providers cannot access plaintext credentials. Users unlock the vault per session; encrypted secrets are decrypted only at execution time. See [ADR-0010](docs/adr/0010-zero-knowledge-vault-trust-model.md).
- **Audit store** records every event in an immutable trace
- **Approval UI** provides a web interface for reviewing and acting on pending approvals
- **Enterprise auth** with Google and GitHub OAuth, email/password signup, SSO enforcement
- **Protected Actions catalog** (in progress) — replaces the MCP server marketplace with a curated set of actions Aileron owns

Next up: Gmail connector, email intent tools, and confidential computing (TEE) execution layer ([#52](https://github.com/ALRubinger/aileron/issues/52)).

## Getting Started

### Prerequisites

- [Go](https://go.dev/dl/) 1.24 or later
- [Node.js](https://nodejs.org/) 24 (see `.nvmrc`)
- [pnpm](https://pnpm.io/) package manager
- [go-task](https://taskfile.dev/) task runner
- [Docker](https://docs.docker.com/get-docker/) and Docker Compose

### Build

```sh
task build
```

This builds everything: Docker containers, Go server binary, MCP gateway binary, and the UI.

To build individual components:

```sh
task build:docker   # Docker containers (server, UI, database)
task build:server   # Go server binary
task build:ui       # SvelteKit UI
task build:mcp      # MCP gateway binary
```

### Run locally with Docker Compose

```sh
task up
```

To run in detached mode:

```sh
task up -- -d
```

This starts the control plane API server, the management UI, API documentation, and a PostgreSQL database. The API is available at `http://localhost:8080`, the UI at `http://localhost:3000`, and the API docs at `http://localhost:3001`.

### Verify

```sh
task health
```

```json
{"status":"ok","service":"aileron","version":"dev","timestamp":"2026-03-31T09:00:00Z"}
```

### Connect Claude Code via MCP

Register Aileron as the MCP gateway for Claude Code:

```sh
task mcp:setup
```

This builds the MCP gateway binary, adds your `aileron.yaml` configuration, and registers Aileron with Claude Code as its MCP server. The gateway discovers downstream servers, re-exposes their tools, and intercepts every tool call for policy evaluation.

To connect to an existing Aileron server instead:

```sh
task build:mcp
claude mcp add --scope project aileron \
  -e AILERON_API_URL=http://localhost:8080 -- ./build/aileron-mcp
```

### Run tests

```sh
task test:go          # Unit tests
task test:integration # Integration tests (requires running server)
```

#### Auth environment variables for Docker Compose

Docker Compose connects to PostgreSQL, which enables auth and requires a JWT signing key. Locally, `task up` auto-creates `deploy/.env` from `deploy/.env.example` with safe defaults. In CI, the workflow (`.github/workflows/ci.yml`) sets its own values directly. For other environments, create `deploy/.env` with at minimum:

```
AILERON_JWT_SIGNING_KEY=<any 32+ character string>
```

### Stop

```sh
task down
```

## API Documentation

Interactive API documentation is available at:

- **Live:** [docs.withaileron.ai](https://docs.withaileron.ai)
- **Local:** `http://localhost:3001` when running `task up`
- **Server-embedded:** `http://localhost:8080/docs` on the running API server

The OpenAPI spec at `core/api/openapi.yaml` is the source of truth. Go types and the server interface are generated from it using [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen):

```sh
task generate:api
```

## Project Structure

```
aileron/
├── core/               Execution plane — policy, approval, vault, audit, auth, connectors
│   ├── api/            OpenAPI specification and generated code
│   ├── account/        Connected accounts SPI and Google OAuth service
│   ├── app/            Application wiring (handlers, middleware) — importable library
│   ├── auth/           Auth SPI, enforcer, JWT, middleware, and provider implementations
│   │   ├── google/     Google OAuth 2.0 provider (Aileron login)
│   │   └── github/     GitHub OAuth 2.0 provider (Aileron login)
│   ├── server/         HTTP server entry point and entrypoint script
│   ├── schema/         Atlas declarative database schema (HCL)
│   ├── policy/         Policy engine SPI, rule-based implementation, seed policies
│   ├── approval/       Approval orchestrator SPI and implementation
│   ├── config/         Configuration
│   ├── connector/      Connector SPI and implementations (email, calendar, payments)
│   ├── store/          Persistence interfaces
│   │   ├── mem/        In-memory implementations (dev/test)
│   │   └── postgres/   PostgreSQL implementations (production)
│   ├── vault/          Credential vault SPI and in-memory implementation
│   ├── notify/         Notification SPI (log, Slack, email)
│   ├── audit/          Immutable audit store SPI
│   └── model/          Shared domain types (Enterprise, User, ConnectedAccount, intents)
├── cmd/
│   └── aileron-mcp/    MCP server exposing intent tools to agent hosts
├── sdk/
│   └── go/             Go client SDK
├── ui/                 Management and approval UI (SvelteKit)
│   └── src/routes/     Pages: approvals, traces, policies, marketplace (Protected Actions), settings
├── docs/               API documentation site (Scalar)
├── test/
│   └── integration/    Integration tests with OpenAPI spec validation
├── deploy/
│   └── docker-compose.yml  Self-hosted deployment
└── saas/               Proprietary SaaS overlay (billing, multi-tenancy)
```

## Installation

Download the latest release for your platform from [GitHub Releases](https://github.com/ALRubinger/aileron/releases).

| Platform | Binary | Archive |
|----------|--------|---------|
| macOS (Apple Silicon) | `aileron-mcp` | `aileron-mcp_*_darwin_arm64.tar.gz` |
| macOS (Intel) | `aileron-mcp` | `aileron-mcp_*_darwin_amd64.tar.gz` |
| Linux (x86_64) | `aileron-mcp` | `aileron-mcp_*_linux_amd64.tar.gz` |
| Windows (x86_64) | `aileron-mcp.exe` | `aileron-mcp_*_windows_amd64.zip` |

Each release also includes `aileron-server` archives for running the control plane server standalone.

```sh
# Example: macOS Apple Silicon
curl -LO https://github.com/ALRubinger/aileron/releases/latest/download/aileron-mcp_0.0.1_darwin_arm64.tar.gz
tar xzf aileron-mcp_0.0.1_darwin_arm64.tar.gz
./aileron-mcp --help
```

Verify the download against the `checksums.txt` file included in each release.

## Releasing

Releases are automated with [GoReleaser](https://goreleaser.com/) and GitHub Actions. Pushing a version tag builds cross-platform binaries and creates a GitHub Release with notes grouped by conventional commit type.

```sh
git tag -a v0.0.3 -m "Release v0.0.3"
git push origin v0.0.3
```

This produces:
- Binaries for `aileron-server` and `aileron-mcp` across Linux, macOS (Intel + Apple Silicon), and Windows
- `.tar.gz` archives (unix) and `.zip` archives (Windows)
- SHA256 checksums (`checksums.txt`)
- Release notes generated from conventional commits since the last tag

To test the release pipeline locally without publishing:

```sh
task release:snapshot
```

## Authentication

Aileron supports SSO and OAuth for the hosted control plane. Authentication is **opt-in** — when no database is configured, the server runs without auth (suitable for local development and the MCP gateway use case).

When `AILERON_DATABASE_URL` is set, the server enables:
- **Email/password signup** with email verification (bcrypt-hashed passwords, 6-digit verification codes)
- **Google and GitHub OAuth sign-in** (with Okta and SAML planned)
- Enterprise account model (auto-created on first sign-in or signup)
- JWT-based session management with refresh token rotation
- Enterprise-level SSO enforcement (provider restrictions, email domain restrictions)
- Cross-provider deduplication — signing in via different providers with the same email resolves to the same account

MCP server management endpoints enforce authentication when auth is enabled. When auth is disabled (no database configured), these endpoints fall back to unscoped behavior suitable for local development.

Auth environment variables are listed in the [Cloud deployment](#environment-variables) section below.

## Deployment

### Local (Docker Compose)

The quickest way to run the full stack locally:

```sh
task up
```

This starts PostgreSQL, the API server (with auto-migration), the UI, and API docs. On first run, `task up` copies `deploy/.env.example` to `deploy/.env` with safe local defaults (including `AILERON_JWT_SIGNING_KEY`). No manual setup needed. Email verification is disabled by default locally (`AILERON_AUTO_VERIFY_EMAIL=true`), so new accounts are activated immediately after signup — no confirmation email required.

To customize, edit `deploy/.env` (gitignored). For example, to enable OAuth providers locally:

```sh
# deploy/.env
AILERON_JWT_SIGNING_KEY=local-dev-signing-key-not-for-production
GOOGLE_CLIENT_ID=your-google-client-id
GOOGLE_CLIENT_SECRET=your-google-client-secret
GITHUB_OAUTH_CLIENT_ID=your-github-client-id
GITHUB_OAUTH_CLIENT_SECRET=your-github-client-secret
```

Verification codes for email/password signup are printed to the server log when `RESEND_API_KEY` is not set (dev mode). To send real emails, add `RESEND_API_KEY` (and optionally `MAIL_FROM`) to your `.env`. Each OAuth provider is independently optional — configure whichever you need.

### Cloud

Aileron is a set of standard Docker containers with no infrastructure-specific assumptions. It runs on any platform that supports containers and PostgreSQL.

#### Services

| Service | Dockerfile | Port | Description |
|---------|-----------|------|-------------|
| **server** | `core/server/Dockerfile` | 8080 | API server and auth handler |
| **ui** | `ui/Dockerfile` | 3000 | SvelteKit management UI |
| **docs** | `docs/Dockerfile` | 80 | OpenAPI documentation (Scalar) |
| **PostgreSQL** | — | 5432 | Database (any managed Postgres 16+ works) |

#### Domains

Each service needs a domain or URL. The auth domain points to the **server** service — it is not a separate service.

| Domain | Points to | Purpose |
|--------|-----------|---------|
| `api.yourdomain.com` | server | API endpoints (`/v1/*`) |
| `auth.yourdomain.com` | server | OAuth callbacks (`/auth/*`) |
| `app.yourdomain.com` | ui | Management UI |
| `docs.yourdomain.com` | docs | API documentation |

#### Environment variables

**Server service:**

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `AILERON_DATABASE_URL` | Yes | | PostgreSQL connection string |
| `AILERON_JWT_SIGNING_KEY` | Yes | | HMAC signing key for access tokens. Generate with `openssl rand -hex 32` |
| `AILERON_JWT_ISSUER` | No | `aileron` | `iss` claim in issued JWTs |
| `AILERON_ACCESS_TOKEN_TTL` | No | `15m` | Access token lifetime |
| `AILERON_REFRESH_TOKEN_TTL` | No | `168h` | Refresh token lifetime (7 days) |
| `AILERON_UI_REDIRECT_URL` | No | `/` | Redirect destination after successful login |
| `AILERON_AUTO_VERIFY_EMAIL` | No | `false` | Skip email verification on signup — accounts are activated immediately. Set to `true` in `deploy/.env.example` for local installs. **Never enable in production.** |
| `GOOGLE_CLIENT_ID` | No | | Google OAuth 2.0 client ID |
| `GOOGLE_CLIENT_SECRET` | No | | Google OAuth 2.0 client secret |
| `GITHUB_OAUTH_CLIENT_ID` | No | | GitHub OAuth 2.0 client ID |
| `GITHUB_OAUTH_CLIENT_SECRET` | No | | GitHub OAuth 2.0 client secret |
| `RESEND_API_KEY` | No | | [Resend](https://resend.com) API key. When set, verification emails are delivered via Resend. When unset, codes are printed to the log (dev/CI mode). |
| `MAIL_FROM` | No | `noreply@withaileron.ai` | Sender address for transactional emails (requires `RESEND_API_KEY`) |
| `AILERON_KEK_SESSION_TTL` | No | `30m` | How long a verified vault passphrase session remains active. After expiry, the user must re-verify their passphrase to unlock encrypted credentials. |

**UI service:**

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `PUBLIC_API_BASE` | Yes | `http://localhost:8080` | URL of the server service (e.g. `https://api.yourdomain.com`) |
| `PUBLIC_POSTHOG_KEY` | No | | PostHog project API key. When set, enables analytics. Passed as a Docker build arg. |
| `PUBLIC_POSTHOG_HOST` | No | `https://us.i.posthog.com` | PostHog ingest endpoint. Passed as a Docker build arg. |

**Docs service:** No configuration required.

#### Setup steps

1. **Provision PostgreSQL.** Any managed Postgres 16+ service works (AWS RDS, GCP Cloud SQL, Supabase, Neon, Railway, etc.).

2. **Build the container images:**
   ```sh
   docker build -f core/server/Dockerfile -t your-registry/aileron-server .
   docker build -f ui/Dockerfile -t your-registry/aileron-ui ui/
   docker build -f docs/Dockerfile -t your-registry/aileron-docs docs/
   ```

3. **Configure environment variables** on each service as listed above.

4. **Configure domains.** Point each domain to its service and provision TLS certificates.

5. **Deploy.** The server entrypoint automatically runs [Atlas](https://atlasgo.io) schema migrations against `AILERON_DATABASE_URL` before starting. Migrations are declarative and idempotent — safe to run on every deploy.

6. **Configure OAuth providers** (optional — each is independent):

   **Google** — in the [Google Cloud Console](https://console.cloud.google.com/apis/credentials):
   - Create an OAuth 2.0 Client ID (Web application type)
   - Add `https://auth.yourdomain.com/auth/google/callback` as an authorized redirect URI

   **GitHub** — in [GitHub Developer Settings](https://github.com/settings/developers):
   - Create a new OAuth App
   - Set the authorization callback URL to `https://auth.yourdomain.com/auth/github/callback`

7. **Verify:**
   ```sh
   curl https://api.yourdomain.com/v1/health
   ```

### Railway

This section covers Railway-specific setup. Refer to the [Cloud](#cloud) section above for the full list of services, environment variables, and domains.

#### 1. Create services

In the Railway dashboard, create three services and one database:

| Service | Dockerfile Path | Root Directory |
|---------|----------------|----------------|
| **server** | `core/server/Dockerfile` | `/` (repo root) |
| **ui** | `ui/Dockerfile` | `ui/` |
| **docs** | `docs/Dockerfile` | `docs/` |
| **Postgres** | — (Railway-managed plugin) | — |

Link the Postgres plugin to the server service.

#### 2. Set environment variables

**Server service** — in the Railway dashboard under the server service's Variables tab:

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

#### 3. Configure domains

Add custom domains in each service's **Settings → Networking → Custom Domain**. Create matching DNS records on Cloudflare (DNS only, not proxied, so Railway can issue TLS certificates).

| Domain | Railway Service | DNS Record |
|--------|----------------|------------|
| `api.withaileron.ai` | server | CNAME → Railway target |
| `app.withaileron.ai` | ui | CNAME → Railway target |

#### 4. Register OAuth callback URLs

Register the API domain with each provider:

- **Google:** `https://api.withaileron.ai/auth/google/callback`
- **GitHub:** `https://api.withaileron.ai/auth/github/callback`

#### 5. Deploy

Push to the branch Railway is watching. The Dockerfile builds the image, and on startup the entrypoint applies schema migrations automatically.

#### 6. Verify

```sh
curl https://api.withaileron.ai/v1/health
```

## Architecture Principles

**Agents decide; Aileron acts.** Agents handle planning and research. Aileron owns credentials, evaluates deterministic policy, and executes irreversible actions. The separation is structural, not advisory.

**Identity ownership.** Aileron holds OAuth tokens and payment instruments in its vault. Agents submit intents and receive structured results — they never see raw credentials. This prevents prompt injection or context compression from bypassing safety rules.

**Deterministic enforcement.** No LLM in the policy enforcement loop. Rules are evaluated against structured intent fields. The policy engine is predictable and auditable.

**SPIs throughout.** Every major subsystem — connectors, policy engine, approval orchestrator, vault, and notifiers — is defined as a Go interface. Built-in implementations cover the common cases. Alternative implementations can be swapped in without modifying the core.

**The audit trail is append-only.** Every event is written once and never modified. The execution graph is the ground truth for what happened, not a log that can be cleaned up.

**OSS core, SaaS overlay.** Everything in `core/`, `sdk/`, and `ui/` is open source. The `saas/` layer adds multi-tenancy and billing on top without forking the core.

## License

See [LICENSE](LICENSE).
