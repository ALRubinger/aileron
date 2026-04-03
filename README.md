# Aileron
_Stay on course. The missing protection layer between your agents and the real world._

![GitHub License](https://img.shields.io/github/license/ALRubinger/aileron?style=for-the-badge)
![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/ALRubinger/aileron/ci.yml?style=for-the-badge&logo=github)

**Aileron is an MCP gateway that enforces governance over every tool call an AI agent makes.**

It sits between an agent host and the MCP servers the agent uses. Aileron aggregates downstream MCP servers, re-exposes their tools, and intercepts every tool call for policy evaluation — without the agent ever touching credentials directly.

---

## The Problem

AI agents are acting on our behalf: booking meetings, paying invoices, merging code. The problem isn't capability. It's trust. An agent that's useful is an agent that's risky.

The current workarounds are inadequate:

- Hardcoded rules buried inside agents, invisible to the people they affect
- Manual approvals handled out-of-band in Slack or email, disconnected from execution
- Over-permissioned API keys that agents hold directly
- Static payment credentials exposed to systems that shouldn't see them
- Agents can bypass governance tools entirely if they have direct access to APIs or other MCP servers

The result is a forced choice: give the agent enough permission to be powerful, or restrict it enough to feel safe. Neither is satisfying.

## The Solution

Aileron is the **single MCP server** that an agent host connects to. It aggregates downstream MCP servers, re-exposes their tools under namespaced names, and intercepts every tool call for policy evaluation. The agent can't bypass Aileron because Aileron IS the tool surface.

Agents remain autonomous in planning. We retain authority over execution. Aileron enforces the boundary between the two — by construction, not by cooperation.

```
Agent Host (Claude Code, OpenClaw, etc.)
  │
  │  MCP (stdio)
  │
  ▼
Aileron MCP Gateway
  ├── Policy Engine         evaluates rules per tool call
  ├── Approval Orchestrator routes to humans when required
  ├── Credential Vault      injects secrets at launch time
  └── Audit Store           immutable record of every decision
  │
  ├──► Downstream MCP Server A (e.g. GitHub)
  ├──► Downstream MCP Server B (e.g. Stripe)
  └──► Downstream MCP Server C (e.g. Slack)
```

## How It Works

**1. Agent host connects to Aileron as its only MCP server**

Claude Code, OpenClaw, or any MCP-compatible agent host is configured with Aileron as the sole MCP server. The agent sees only the tools that Aileron exposes.

**2. Aileron discovers tools from downstream MCP servers**

On startup, Aileron connects to configured downstream MCP servers, discovers their tools, and re-exposes them under namespaced names (e.g. `github__create_pull_request`).

**3. Every tool call passes through policy evaluation**

When the agent calls a tool, Aileron intercepts it, maps it to the policy engine, and evaluates it against configured rules. The disposition is allow, deny, require approval, or allow with modifications.

**4. Humans approve high-risk actions**

If approval is required, Aileron holds the tool call and notifies approvers. The agent can poll with `aileron__check_approval`. When approved, Aileron auto-executes the queued call and returns the real result.

**5. Credentials are injected from the vault**

Downstream MCP servers receive credentials from the Aileron vault at launch time. Agents never see API keys, tokens, or secrets.

**6. Everything is logged**

Every tool call interception, policy decision, approval, and execution is recorded in an immutable audit trail. You have a verifiable record of what every agent did, who approved it, and when.

## For Organizations

Aileron gives organizations centralized control over agent activity across teams.

- **Service catalog.** Configure which MCP servers are available to employees. Agents only see the tools you expose.
- **Credential management.** API keys, tokens, and secrets live in the Aileron vault. Teams use agents without handling credentials directly.
- **Policy governance.** Define rules that apply across all agent activity — spend limits, branch protections, vendor allowlists, time-of-day controls.
- **Compliance.** An immutable audit trail records every tool call, policy decision, and approval for review and export.

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

The MCP gateway architecture is implemented end-to-end:

- **MCP gateway** aggregates downstream MCP servers and re-exposes their tools
- **Policy engine** evaluates rules against every tool call (allow, deny, require approval, allow with modifications)
- **Approval orchestrator** manages human-in-the-loop workflows with approve/deny/modify
- **Credential vault** injects secrets into downstream servers at launch time
- **Audit store** records every event in an immutable trace
- **Approval UI** provides a web interface for reviewing and acting on pending approvals
- **User profile and organization settings** let users manage their account and configure SSO policies (allowed providers, email domains, SSO enforcement)

Five seed policies ship by default:
1. Require approval for PRs targeting `main`, `master`, or `production`
2. Allow PRs to feature branches
3. Deny force pushes
4. Require approval for tool calls with base argument targeting protected branches
5. Deny destructive tool calls (`delete_*`)

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
├── core/               Control plane — policy, approval, vault, audit, auth
│   ├── api/            OpenAPI specification and generated code
│   ├── app/            Application wiring (handlers, middleware) — importable library
│   ├── auth/           Auth SPI, enforcer, JWT, middleware, and provider implementations
│   │   ├── google/     Google OAuth 2.0 provider
│   │   └── github/     GitHub OAuth 2.0 provider
│   ├── server/         HTTP server entry point and entrypoint script
│   ├── schema/         Atlas declarative database schema (HCL)
│   ├── policy/         Policy engine SPI, rule-based implementation, seed policies
│   ├── approval/       Approval orchestrator SPI and implementation
│   ├── config/         YAML and environment-based configuration
│   ├── mcpclient/      MCP client for downstream server connections
│   ├── connector/      Connector SPI and implementations
│   ├── store/          Persistence interfaces
│   │   ├── mem/        In-memory implementations (dev/test)
│   │   └── postgres/   PostgreSQL implementations (production)
│   ├── vault/          Credential vault SPI and in-memory implementation
│   ├── notify/         Notification SPI (log, Slack, email)
│   ├── audit/          Immutable audit store SPI
│   └── model/          Shared domain types (including Enterprise, User, Session)
├── cmd/
│   └── aileron-mcp/    MCP gateway that aggregates and proxies downstream MCP servers
├── sdk/
│   └── go/             Go client SDK
├── ui/                 Management and approval UI (SvelteKit)
│   └── src/routes/     Pages: approvals, traces, policies, settings (profile, organization)
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

### Auth Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `AILERON_DATABASE_URL` | Yes (to enable auth) | | PostgreSQL connection string |
| `AILERON_JWT_SIGNING_KEY` | Yes (when auth enabled) | | HMAC signing key for access tokens (32+ characters) |
| `AILERON_JWT_ISSUER` | No | `aileron` | `iss` claim in issued JWTs |
| `AILERON_ACCESS_TOKEN_TTL` | No | `15m` | Access token lifetime |
| `AILERON_REFRESH_TOKEN_TTL` | No | `168h` | Refresh token lifetime (default 7 days) |
| `AILERON_UI_REDIRECT_URL` | No | `/` | Redirect destination after successful login |
| `GOOGLE_CLIENT_ID` | No | | Google OAuth 2.0 client ID |
| `GOOGLE_CLIENT_SECRET` | No | | Google OAuth 2.0 client secret |
| `GITHUB_OAUTH_CLIENT_ID` | No | | GitHub OAuth 2.0 client ID |
| `GITHUB_OAUTH_CLIENT_SECRET` | No | | GitHub OAuth 2.0 client secret |
| `AILERON_OAUTH_CALLBACK_BASE_URL` | No | | Stable domain for OAuth callbacks (e.g. `https://auth.withaileron.ai`). Required for Railway branch deployments; see below. |
| `AILERON_TRUSTED_ORIGINS` | No | | Comma-separated list of hostname patterns allowed as relay targets (e.g. `*.up.railway.app,withaileron.ai`). Required on the stable domain when relaying. |

By default, OAuth callback URLs are derived dynamically from the incoming request host. For Railway branch deployments — where each branch gets a unique hostname — set `AILERON_OAUTH_CALLBACK_BASE_URL` to a stable domain so OAuth providers only need one registered redirect URI. See the [Railway setup section](#railway) below for configuration details.

## Deployment

### Local (Docker Compose)

The quickest way to run the full stack locally:

```sh
task up
```

This starts PostgreSQL, the API server (with auto-migration), the UI, and API docs. On first run, `task up` copies `deploy/.env.example` to `deploy/.env` with safe local defaults (including `AILERON_JWT_SIGNING_KEY`). No manual setup needed.

To customize, edit `deploy/.env` (gitignored). For example, to enable OAuth providers locally:

```sh
# deploy/.env
AILERON_JWT_SIGNING_KEY=local-dev-signing-key-not-for-production
GOOGLE_CLIENT_ID=your-google-client-id
GOOGLE_CLIENT_SECRET=your-google-client-secret
GITHUB_OAUTH_CLIENT_ID=your-github-client-id
GITHUB_OAUTH_CLIENT_SECRET=your-github-client-secret
```

Verification codes for email/password signup are printed to the server log (dev mailer). Each OAuth provider is independently optional — configure whichever you need.

### Cloud (Provider-Agnostic)

Aileron is a standard Docker container with no infrastructure-specific assumptions. It runs on any platform that supports containers and PostgreSQL.

**Requirements:**
- A PostgreSQL 16+ instance
- A container runtime (Docker, Kubernetes, ECS, Cloud Run, etc.)
- The environment variables listed above

**Steps:**

1. **Provision PostgreSQL.** Any managed Postgres service works (AWS RDS, GCP Cloud SQL, Supabase, Neon, etc.).

2. **Build and push the server image:**
   ```sh
   docker build -f core/server/Dockerfile -t aileron-server .
   docker tag aileron-server your-registry.example.com/aileron-server:latest
   docker push your-registry.example.com/aileron-server:latest
   ```

3. **Set environment variables** on your container runtime:
   ```
   AILERON_DATABASE_URL=postgres://user:pass@host:5432/aileron?sslmode=require
   AILERON_JWT_SIGNING_KEY=<random-32-char-string>
   GOOGLE_CLIENT_ID=<from-google-cloud-console>
   GOOGLE_CLIENT_SECRET=<from-google-cloud-console>
   GITHUB_OAUTH_CLIENT_ID=<from-github-developer-settings>
   GITHUB_OAUTH_CLIENT_SECRET=<from-github-developer-settings>
   ```
   Each OAuth provider is optional. OAuth callback URLs are derived from the request host automatically.

4. **Deploy the container.** The entrypoint automatically runs Atlas schema migrations against `AILERON_DATABASE_URL` before starting the server. Migrations are declarative and idempotent — safe to run on every deploy.

5. **Configure OAuth providers** (optional — each is independent):

   **Google** — in the [Google Cloud Console](https://console.cloud.google.com/apis/credentials):
   - Create an OAuth 2.0 Client ID (Web application type)
   - Add `https://<your-domain>/auth/google/callback` as an authorized redirect URI
   - Set `GOOGLE_CLIENT_ID` and `GOOGLE_CLIENT_SECRET`

   **GitHub** — in [GitHub Developer Settings](https://github.com/settings/developers):
   - Create a new OAuth App
   - Set the authorization callback URL to `https://<your-domain>/auth/github/callback`
   - Set `GITHUB_OAUTH_CLIENT_ID` and `GITHUB_OAUTH_CLIENT_SECRET`

6. **Verify:**
   ```sh
   curl https://api.yourdomain.com/v1/health
   ```

**Schema migrations** are handled automatically. The Docker image includes the [Atlas](https://atlasgo.io) CLI and the schema definition. On each container start, the entrypoint runs `atlas schema apply` to converge the database to the declared state. No manual migration steps are needed.

### Railway

Aileron is currently hosted on [Railway](https://railway.com). The server service is connected to a Railway-managed PostgreSQL instance.

#### Basic Setup

1. **PostgreSQL service** — provision a Railway-managed PostgreSQL instance and link it to the server service.

2. **Server service environment variables** — set these in the Railway dashboard under the server service's Variables tab:

   | Variable | Value |
   |----------|-------|
   | `AILERON_DATABASE_URL` | `${{Postgres.DATABASE_URL}}` (Railway variable reference) |
   | `AILERON_JWT_SIGNING_KEY` | Generate with `openssl rand -hex 32` |
   | `GOOGLE_CLIENT_ID` | From Google Cloud Console (optional) |
   | `GOOGLE_CLIENT_SECRET` | From Google Cloud Console (optional) |
   | `GITHUB_OAUTH_CLIENT_ID` | From GitHub Developer Settings (optional) |
   | `GITHUB_OAUTH_CLIENT_SECRET` | From GitHub Developer Settings (optional) |

3. **Deploy** — push to the branch Railway is watching. The Dockerfile builds the image, and on startup the entrypoint applies schema migrations automatically.

#### Stable OAuth Callback Domain (required for branch deployments)

Railway gives every branch deploy a unique, unpredictable hostname (e.g. `feat-foo-abc123.up.railway.app`). OAuth providers require all callback URLs to be pre-registered, so each new branch URL would need manual registration. The stable callback domain feature solves this: all OAuth flows route through one stable domain, and only that URL is registered with providers.

**Step 1 — Designate a stable callback domain.**

Attach a custom domain to your production Railway service, e.g. `auth.withaileron.ai`. This domain will receive all OAuth callbacks from every deployment (production, staging, branch deploys).

In the Railway dashboard:
- Go to your production server service → **Settings** → **Networking** → **Custom Domain**
- Add your stable domain (e.g. `auth.withaileron.ai`)
- Point the DNS record to the Railway-provided target

**Step 2 — Register the stable callback URL with OAuth providers.**

Register exactly one callback URL per provider — the stable domain:

- **Google** (in [Google Cloud Console](https://console.cloud.google.com/apis/credentials)):
  - OAuth 2.0 Client → Authorized redirect URIs → `https://auth.withaileron.ai/auth/google/callback`

- **GitHub** (in [GitHub Developer Settings](https://github.com/settings/developers)):
  - OAuth App → Authorization callback URL → `https://auth.withaileron.ai/auth/github/callback`

You do not need to add branch deploy URLs to either provider.

**Step 3 — Set shared environment variables across all services.**

In your Railway project, set these as **shared variables** (available to all services and environments) so branch deployments inherit them automatically:

| Variable | Value |
|----------|-------|
| `AILERON_OAUTH_CALLBACK_BASE_URL` | `https://auth.withaileron.ai` |
| `AILERON_TRUSTED_ORIGINS` | `*.up.railway.app,withaileron.ai` |

To set shared variables in Railway:
- Go to your project → **Variables** → **Shared Variables**
- Add both variables with the values above
- Adjust `AILERON_TRUSTED_ORIGINS` to include your exact domain patterns

**Step 4 — Verify the flow.**

After deploying, open a branch deployment and initiate sign-in with Google or GitHub:

1. The branch deploy (`feat-foo-abc123.up.railway.app`) redirects to the provider with `redirect_uri=https://auth.withaileron.ai/auth/github/callback`.
2. After authorization, the provider redirects to `auth.withaileron.ai`.
3. `auth.withaileron.ai` detects the originating host in the state parameter, validates it against `AILERON_TRUSTED_ORIGINS`, and relays the callback to `feat-foo-abc123.up.railway.app`.
4. The branch deployment completes the token exchange and creates a session.

The round-trip is transparent to the user (a single extra redirect).

## Architecture Principles

**Structural enforcement.** Aileron is the only MCP server the agent connects to. Governance is enforced by construction, not by cooperation. The agent cannot bypass policy because there is no alternative path to the tools.

**SPIs throughout.** Every major subsystem — the policy engine, approval orchestrator, vault, and notifiers — is defined as a Go interface. Built-in implementations cover the common cases. Alternative implementations can be swapped in without modifying the core.

**Credentials never reach agents.** The vault resolves secrets at launch time and injects them into downstream MCP servers. Agents interact with tools through Aileron, never with credentials.

**The audit trail is append-only.** Every event is written once and never modified. The trail is the ground truth for what happened, not a log that can be cleaned up.

**OSS core, SaaS overlay.** Everything in `core/`, `sdk/`, and `ui/` is open source. The `saas/` layer adds multi-tenancy and billing on top without forking the core.

## License

See [LICENSE](LICENSE).
