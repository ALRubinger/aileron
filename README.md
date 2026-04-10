# Aileron
_Let your agents fly._

![GitHub License](https://img.shields.io/github/license/ALRubinger/aileron?style=for-the-badge)
![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/ALRubinger/aileron/ci.yml?style=for-the-badge&logo=github)
![Codecov](https://img.shields.io/codecov/c/github/ALRubinger/aileron?style=for-the-badge)

**Aileron is the execution layer between your AI coding agent and the real world.** It holds your credentials, enforces your policy, brokers access to external services, and logs every decision. `aileron launch claude` and the agent operates within your project's policy-as-code — less time blocking on permission prompts, more time with the agent working.

---

## The Problem

We rely on our coding agents to write code and run tests, but they can't push a branch or call an API without us in the loop. We rubber-stamp access requests. Our agents wait on approval while we're in a meeting or getting coffee. We alt-tab to Slack to relay information the agent already has. We keep useful API keys out of reach because it's too dangerous.

Today's agent hosts give you two modes: approve every command individually (50 prompts per session — you stop reading) or auto-approve everything (no guardrails). Neither is satisfying. The developer trains themselves to approve on autopilot, which is worse than no security because it builds a false sense of oversight.

## The Solution

`aileron launch claude` and the agent operates within your project's policy-as-code. Sane defaults mean safe commands auto-approve, dangerous ones are blocked, and ambiguous ones ask you once and learn your answer.

- **Policy-enforced shell.** Every command the agent runs goes through `aileron-sh`. An `aileron.yaml` in your repo defines three buckets: allow (auto-approve), deny (hard block), ask (prompt the developer). The policy lives in version control, is reviewable in PRs, and is shared with the team.
- **The policy learns from use.** `[a] allow always` teaches the policy. First session: 8 prompts. Allow-always on 5 patterns. Second session: 3 prompts. End of session, Aileron offers to persist learned patterns to `aileron.yaml`.
- **Credential brokering.** Secrets stored in a local encrypted vault. The agent uses them but never sees them — no risk of leaking secrets into context.
- **Bidirectional communication.** Slack and Discord messages arrive in your terminal. The agent drafts replies using its codebase context and you approve with one keypress.
- **Audit trail.** Every decision is logged — what the agent did, what the policy decided, what you authorized. `aileron log` shows the complete record.
- **Agent-portable.** Switch from Claude Code to OpenCode to Goose and your policy, credentials, and audit trail come with you.

```
┌────────────────────────────────────────────────────────┐
│  aileron launch claude                                 │
│                                                        │
│  ┌──────────────┐  ┌──────────────┐  ┌─────────────┐  │
│  │ aileron-sh   │  │ aileron-mcp  │  │ listeners   │  │
│  │ (SHELL shim) │  │ (MCP server) │  │ (Slack/     │  │
│  │              │  │              │  │  Discord)   │  │
│  │ cmd string → │  │ http_request │  │             │  │
│  │ policy eval  │  │ send_message │  │ inbound →   │  │
│  │ exec | deny  │  │ read_messages│  │ /dev/tty    │  │
│  └──────┬───────┘  └──────┬───────┘  └──────┬──────┘  │
│         │                 │                  │         │
│         ▼                 ▼                  ▼         │
│  ┌─────────────────────────────────────────────────┐   │
│  │              Policy Engine                      │   │
│  │  OS profile + lang profile + aileron.yaml       │   │
│  │  + built-in structural deny rules               │   │
│  └─────────────────────┬───────────────────────────┘   │
│                        │                               │
│                        ▼                               │
│  ┌─────────────────────────────────────────────────┐   │
│  │          Audit Trail (local JSONL)              │   │
│  └─────────────────────────────────────────────────┘   │
│                                                        │
│  ┌─────────────────────────────────────────────────┐   │
│  │  Agent (claude, codex, goose, opencode, ...)    │   │
│  │  Inherits: SHELL=aileron-sh, scrubbed env,      │   │
│  │            aileron-mcp registered               │   │
│  └─────────────────────────────────────────────────┘   │
│                                                        │
│  ┌─────────────────────────────────────────────────┐   │
│  │          Local Vault (KEK-encrypted)            │   │
│  │  Bot tokens, API keys, webhook URLs             │   │
│  └─────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────┘
```

## How It Works

**1. Launch your agent**

```sh
aileron launch claude
```

Aileron spawns the agent as a child process with a policy-enforced shell. Every command the agent runs flows through `aileron-sh` and the policy engine before reaching the real shell. Aileron handles agent-specific quirks (shell validation, command wrapping) so the policy rules stay clean.

**2. Policy evaluates every command**

Safe commands (tests, builds, reads) auto-approve silently. Dangerous commands (force push, recursive delete) are hard-denied. Ambiguous commands (commit, push, deploy) prompt you once with context:

```
  ⏸ aileron: agent wants to run `git push origin feature/auth`
    matched rule: ask (git push)
    [y] allow  [n] deny  [a] allow always  [s] show details
```

**3. The policy writes itself through use**

Hit `[a] allow always` and the pattern is saved for the session. End of session, Aileron offers to persist learned patterns to `aileron.yaml`. Community-maintained profiles (`lang/go`, `lang/node`, `os/darwin`) provide sensible defaults per language and platform.

**4. Teammates message you — the agent drafts a reply**

Slack and Discord messages arrive in your terminal. On channels configured with `auto_draft: true`, the agent drafts a reply using its live codebase context. You approve with one keypress:

```
  📝 aileron: agent drafted a reply to Sarah in #backend
    "No, the claims structure isn't changing. The refactor
     only affects validation logic in middleware.go."
    [y] send  [e] edit and send  [n] discard
```

**5. Credentials are brokered, never exposed**

Secrets stored in a local encrypted vault. The agent provides a URL and headers; Aileron matches the URL to a configured secret, injects the credential, makes the call, and returns the response. The agent sees the result, never the secret.

**6. Everything is logged**

Every command, policy decision, approval, message sent, and credential used is recorded in an append-only local audit log.

```sh
aileron log
```

## Supported Agents

| Agent | Shell policy | MCP tools | Full experience |
|-------|-------------|-----------|-----------------|
| Claude Code | Yes | Yes | Yes |
| OpenCode | Yes | Yes | Yes |
| Goose | Yes | Yes | Yes |
| Amp | Yes | Yes | Yes |
| Aider | Yes | In progress | Shell policy now |
| Codex CLI | Yes | Not yet | Shell policy only |
| Cline (VS Code) | Yes | Yes | Yes |

## Zero-Knowledge Vault

Aileron's credential vault uses a zero-knowledge architecture: your secrets are encrypted with a key derived from a passphrase that only you know. Aileron stores the encrypted ciphertext and the Argon2id salt — never the key itself.

**How it works:**

1. You set a vault passphrase. Aileron derives a 256-bit Key Encryption Key (KEK) using Argon2id, stores only the salt and a verification blob, then discards the KEK from memory.
2. When you connect an external account (Gmail, Calendar, etc.), the OAuth refresh token is encrypted with your KEK before storage. The database holds only ciphertext.
3. To execute actions, you verify your passphrase to unlock a time-limited session (default: 24 hours). The KEK is held in memory only for the session duration.
4. When TEE is active, verifying your passphrase automatically escrows all connected account credentials into the hardware-isolated enclave. Escrowed credentials persist for 7 days (configurable), independent of the KEK session — so your agents run autonomously without interruption.
5. When an agent triggers an execution, Aileron uses the escrowed credential (TEE mode) or decrypts with the session KEK (direct mode), calls the external API, and discards the plaintext.

**What this means:**

- A database breach yields only ciphertext — useless without your passphrase.
- Aileron operators cannot read your credentials, even with full database access.
- Hosting providers (AWS, Railway, etc.) see only encrypted data.

**Confidential computing (Stage 2):** Credential decryption and connector execution can run inside a hardware-isolated enclave, so plaintext credentials never exist on the host — even in memory. The TEE layer uses a provider SPI with pluggable backends. Set `AILERON_TEE_PROVIDER=local` for development (in-process, no hardware isolation) or `AILERON_TEE_PROVIDER=confidential-space` for production on Google Confidential Space (AMD SEV-SNP). See [ADR-0010](docs/adr/0010-zero-knowledge-vault-trust-model.md) for the trust model, [ADR-0011](docs/adr/0011-tee-provider-spi-and-confidential-space.md) for TEE provider decisions, and [ADR-0012](docs/adr/0012-auto-escrow-and-decoupled-session-lifetimes.md) for auto-escrow and session lifetime design.

## Current Status

Aileron is pivoting from a cloud-hosted execution plane to a **local-first CLI tool** centered on `aileron launch`. The core infrastructure (policy engine, zero-knowledge vault, audit trail) is being reoriented to power the connected coding session.

**Built:**
- `aileron launch <agent>` CLI and `aileron-sh` shell shim — agents run with a policy-enforced shell
- Policy engine with deterministic rule evaluation (allow, deny, require approval)
- Zero-knowledge credential vault (Argon2id + AES-256-GCM, passphrase-derived keys)
- Confidential computing / TEE support (Google Confidential Space, AMD SEV-SNP)

**In progress:**
- Policy schema and loader for `aileron.yaml` ([#64](https://github.com/ALRubinger/aileron/issues/64))
- Terminal UX, pty proxy, and bidirectional communication ([#65](https://github.com/ALRubinger/aileron/issues/65))
- Community policy profiles (`lang/go`, `lang/node`, `os/darwin`)

See the full roadmap in [#63](https://github.com/ALRubinger/aileron/issues/63) and product vision in [#66](https://github.com/ALRubinger/aileron/issues/66).

## Getting Started

### Prerequisites

- [Go](https://go.dev/dl/) 1.25 or later
- [go-task](https://taskfile.dev/) task runner
- An AI coding agent installed (e.g., [Claude Code](https://claude.ai/code))

For the full stack (server, UI, docs):
- [Node.js](https://nodejs.org/) 24 (see `.nvmrc`)
- [pnpm](https://pnpm.io/) package manager
- [Docker](https://docs.docker.com/get-docker/) and Docker Compose

### Build

Build the CLI and shell shim:

```sh
task build:cli       # → build/aileron
task build:sh        # → build/aileron-sh
```

Both binaries must be available together — the CLI looks for `aileron-sh` next to itself, then on PATH.

To build everything (including server, MCP, enclave, UI, docs):

```sh
task build
```

Individual components:

```sh
task build:server    # Go server binary
task build:mcp       # MCP server binary
task build:enclave   # TEE enclave binary
task build:ui        # SvelteKit UI
task build:docs      # Documentation site
task build:docker    # Docker containers
```

### Launch an agent

```sh
./build/aileron launch claude
```

This spawns Claude Code with the policy-enforced shell. Every command the agent runs flows through `aileron-sh`, which evaluates it against your `aileron.yaml` rules before allowing execution. Aileron is the single approval layer — Claude Code's native Bash approval is suppressed.

### Slack notifications

Aileron can receive Slack messages in your terminal while you work. Incoming messages appear in the status bar; press **Ctrl-A** to open the full notification overlay.

**1. Create a Slack app** with [Socket Mode](https://api.slack.com/apis/socket-mode) enabled. You need:
- An **App-Level Token** (`xapp-...`) with `connections:write` scope
- A **Bot Token** (`xoxb-...`) with `channels:history`, `channels:read`, `chat:write`, `users:read` scopes
- Subscribe to the `message.channels` event

**2. Add the tokens to your `aileron.yaml`:**

```yaml
notifications:
  slack:
    app_token: xapp-1-A0123456789-...
    bot_token: xoxb-...
    channels:
      - name: "#backend"
        show: all
        auto_draft: true
      - name: "#incidents"
        show: all
        priority: high
    ignore:
      - "#random"
```

**3. Launch as usual** — Aileron starts the Slack listener automatically:

```sh
./build/aileron launch claude
```

Messages from configured channels appear in the notification bar. Press **Ctrl-A** to view the full queue, navigate with **j/k** or arrow keys, press **a** to ask the agent to draft a reply, **d** to dismiss, and **Escape** to return. On channels with `auto_draft: true`, the agent drafts replies automatically when messages arrive.

### Discord notifications

Aileron can also receive Discord messages in your terminal. The setup mirrors Slack — incoming messages appear in the status bar and the notification overlay.

**1. Create a Discord bot** in the [Discord Developer Portal](https://discord.com/developers/applications):
- Create a new application, then add a **Bot** under the Bot tab
- Copy the **Bot Token**
- Under **Privileged Gateway Intents**, enable **Message Content Intent**
- Generate an invite link under **OAuth2 → URL Generator** with the `bot` scope and these permissions: `Read Messages/View Channels`, `Send Messages`, `Read Message History`
- Invite the bot to your server using the generated URL

**2. Get channel IDs.** In Discord, enable Developer Mode (User Settings → Advanced → Developer Mode). Right-click a channel and **Copy Channel ID**.

**3. Add the config to your `aileron.yaml`:**

```yaml
notifications:
  discord:
    bot_token: "your-bot-token"
    channels:
      - name: "1234567890123456789"   # channel ID
        show: all
        auto_draft: true
      - name: "9876543210987654321"
        show: all
        priority: high
    ignore:
      - "1111111111111111111"          # channel ID to ignore
```

**4. Launch as usual** — Aileron starts the Discord listener automatically:

```sh
./build/aileron launch claude
```

Discord messages appear alongside Slack messages in the same notification bar and overlay. Both listeners can run simultaneously.

### Run tests

```sh
task test:go          # Unit tests (all Go packages)
task test:integration # Integration tests (requires running server)
```

### Run locally with Docker Compose

For the full server/UI stack:

```sh
task up
```

This starts the API server, management UI, API documentation, and PostgreSQL. The API is available at `http://localhost:8080`, the UI at `http://localhost:3000`.

#### Auth environment variables for Docker Compose

Docker Compose connects to PostgreSQL, which enables auth and requires a JWT signing key. Locally, `task up` auto-creates `deploy/.env` from `deploy/.env.example` with safe defaults. In CI, the workflow (`.github/workflows/ci.yml`) sets its own values directly. For other environments, create `deploy/.env` with at minimum:

```
AILERON_JWT_SIGNING_KEY=<any 32+ character string>
```

### Stop

```sh
task down
```

## Documentation

Project documentation is available at:

- **Live:** [docs.withaileron.ai](https://docs.withaileron.ai)
- **Local:** `http://localhost:3001` when running `task up`, or `task dev:docs` for the dev server
- **API Reference:** at `/api` within the docs site
- **Architecture Decision Records:** at `/adr` within the docs site
- **Server-embedded:** `http://localhost:8080/docs` on the running API server

The docs site is a SvelteKit app in `docs/`. Markdown content is rendered via mdsvex, and ADRs from `docs/adr/` are served dynamically.

The OpenAPI spec at `core/api/openapi.yaml` is the source of truth. Go types and the server interface are generated from it using [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen):

```sh
task generate:api
```

## Project Structure

```
aileron/
├── cmd/
│   ├── aileron/         CLI entry point — `aileron launch`, `aileron version`
│   ├── aileron-sh/      Shell shim — intercepts agent commands for policy evaluation
│   ├── aileron-mcp/     MCP server exposing tools to agent hosts
│   └── aileron-enclave/ TEE enclave binary (confidential computing)
├── core/                Core library — policy, launch, vault, auth, connectors
│   ├── launch/          Agent launcher (resolve binary, env setup, process management)
│   │   └── agents/      Agent definitions (claude, codex, goose, etc.)
│   ├── policy/          Policy engine SPI, rule-based implementation, seed policies
│   ├── vault/           Zero-knowledge credential vault
│   ├── api/             OpenAPI specification and generated code
│   ├── app/             HTTP handler wiring and service composition
│   ├── auth/            OAuth providers (Google, GitHub), JWT, session management
│   ├── connector/       Connector SPI and implementations (git, calendar, payments)
│   ├── store/           Persistence interfaces
│   │   ├── mem/         In-memory implementations (dev/test)
│   │   └── postgres/    PostgreSQL implementations (production)
│   └── model/           Shared domain types
├── enclave/             TEE provider SPI and implementations
│   ├── local/           In-process provider for dev/test
│   └── gcs/             Google Confidential Space provider
├── sdk/
│   └── go/              Go client SDK
├── ui/                  Management and approval UI (SvelteKit)
├── docs/                Documentation site (SvelteKit + mdsvex)
├── test/
│   └── integration/     Integration tests with OpenAPI spec validation
└── deploy/
    └── docker-compose.yml  Self-hosted deployment
```

## Installation

Download the latest release for your platform from [GitHub Releases](https://github.com/ALRubinger/aileron/releases).

| Platform | CLI | Shell shim | Archive |
|----------|-----|-----------|---------|
| macOS (Apple Silicon) | `aileron` | `aileron-sh` | `aileron_*_darwin_arm64.tar.gz` |
| macOS (Intel) | `aileron` | `aileron-sh` | `aileron_*_darwin_amd64.tar.gz` |
| Linux (x86_64) | `aileron` | `aileron-sh` | `aileron_*_linux_amd64.tar.gz` |
| Windows (x86_64) | `aileron.exe` | — | `aileron_*_windows_amd64.zip` |

Place both `aileron` and `aileron-sh` in the same directory on your PATH. The CLI looks for the shim next to itself first.

Each release also includes `aileron-server` and `aileron-mcp` archives.

Verify downloads against the `checksums.txt` file included in each release.

## Releasing

Releases are automated with [GoReleaser](https://goreleaser.com/) and GitHub Actions. Pushing a version tag builds cross-platform binaries and creates a GitHub Release with notes grouped by conventional commit type.

```sh
git tag -a v0.0.3 -m "Release v0.0.3"
git push origin v0.0.3
```

This produces:
- Binaries for `aileron`, `aileron-sh`, `aileron-server`, and `aileron-mcp` across Linux and macOS (Intel + Apple Silicon). `aileron` and `aileron-server` also build for Windows.
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

Authentication is enforced on all endpoints when auth is enabled. When auth is disabled (no database configured), endpoints fall back to unscoped behavior suitable for local development.

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
| **enclave** | `cmd/aileron-enclave/Dockerfile` | 8443 | TEE enclave binary (optional — only when using confidential computing) |
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
| `AILERON_KEK_SESSION_TTL` | No | `24h` | How long a verified vault passphrase session remains active. Controls interactive vault operations (UI, connecting accounts). After expiry, the user must re-verify. |
| `AILERON_ESCROW_TTL` | No | `168h` | How long auto-escrowed credentials persist in TEE memory for autonomous agent execution. Independent of the KEK session. Re-verification refreshes the escrow window. |
| `AILERON_TEE_PROVIDER` | No | *(empty)* | TEE provider: `local` (dev/test, in-process), `confidential-space` (production), or empty (disabled). When disabled, credentials are decrypted in the server process. |
| `AILERON_ENCLAVE_URL` | No | | Base URL of the enclave binary (e.g. `https://enclave.internal:8443`). Required when `AILERON_TEE_PROVIDER=confidential-space`. |
| `AILERON_ENCLAVE_IMAGE_DIGEST` | No | | Expected container image digest (`sha256:...`) for attestation verification. Ensures only the expected enclave binary is trusted. |
| `AILERON_GCP_PROJECT_ID` | No | | Expected GCP project ID for attestation verification. Ensures the enclave is running in the expected project. |

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
   docker build -f cmd/aileron-enclave/Dockerfile -t your-registry/aileron-enclave .  # optional, for TEE
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

### TEE Enclave (Confidential Computing)

The enclave service is **optional**. When `AILERON_TEE_PROVIDER` is empty or unset, the server decrypts credentials in-process (Stage 1 behavior). When enabled, the server delegates credential decryption and connector execution to the enclave binary running inside a hardware-isolated TEE.

#### Local development

Set `AILERON_TEE_PROVIDER=local` on the **server** service. No enclave binary is needed — the local provider executes connectors in-process with the same ECDH session protocol but no hardware isolation. Useful for testing the attestation and session flow end-to-end.

#### Production (Google Confidential Space)

Google Confidential Space runs containers inside AMD SEV-SNP confidential VMs where memory is hardware-encrypted. The enclave binary runs as a container on this VM, and the GCE metadata service provides OIDC attestation tokens that prove the workload identity to the Aileron server.

##### Prerequisites

- A GCP project with billing enabled
- `gcloud` CLI installed and authenticated
- Docker (for building and pushing the enclave image)
- A container registry (Artifact Registry or Container Registry)

##### 1. Enable required GCP APIs

```sh
export GCP_PROJECT=your-project-id

gcloud services enable \
  compute.googleapis.com \
  artifactregistry.googleapis.com \
  confidentialcomputing.googleapis.com \
  --project=$GCP_PROJECT
```

##### 2. Create an Artifact Registry repository

```sh
export REGION=us-central1

gcloud artifacts repositories create aileron-enclave \
  --repository-format=docker \
  --location=$REGION \
  --project=$GCP_PROJECT
```

##### 3. Build and push the enclave container image

```sh
export REGISTRY=$REGION-docker.pkg.dev/$GCP_PROJECT/aileron-enclave

docker build -f cmd/aileron-enclave/Dockerfile -t $REGISTRY/aileron-enclave:latest .
docker push $REGISTRY/aileron-enclave:latest
```

Record the image digest from the push output — you'll need it for attestation verification:

```sh
export IMAGE_DIGEST=$(gcloud artifacts docker images describe \
  $REGISTRY/aileron-enclave:latest \
  --format='value(image_summary.digest)' \
  --project=$GCP_PROJECT)

echo "Image digest: $IMAGE_DIGEST"
```

##### 4. Create a service account for the enclave VM

The enclave VM needs a service account that can pull container images and access the GCE metadata service for attestation tokens.

```sh
gcloud iam service-accounts create aileron-enclave \
  --display-name="Aileron Enclave" \
  --project=$GCP_PROJECT

export ENCLAVE_SA=aileron-enclave@$GCP_PROJECT.iam.gserviceaccount.com

# Grant permission to pull images from Artifact Registry.
gcloud artifacts repositories add-iam-policy-binding aileron-enclave \
  --location=$REGION \
  --member="serviceAccount:$ENCLAVE_SA" \
  --role="roles/artifactregistry.reader" \
  --project=$GCP_PROJECT

# Grant permission to generate attestation tokens.
gcloud projects add-iam-policy-binding $GCP_PROJECT \
  --member="serviceAccount:$ENCLAVE_SA" \
  --role="roles/confidentialcomputing.workloadUser"
```

##### 5. Create the Confidential Space VM

```sh
gcloud compute instances create aileron-enclave \
  --project=$GCP_PROJECT \
  --zone=$REGION-a \
  --machine-type=n2d-standard-2 \
  --confidential-compute \
  --min-cpu-platform="AMD Milan" \
  --image-family=confidential-space \
  --image-project=confidential-space-images \
  --service-account=$ENCLAVE_SA \
  --scopes=cloud-platform \
  --metadata="tee-image-reference=$REGISTRY/aileron-enclave:latest,tee-container-log-redirect=true,tee-env-AILERON_TEE_PROVIDER=confidential-space,tee-env-AILERON_ENCLAVE_PORT=8443" \
  --tags=aileron-enclave
```

The `tee-image-reference` metadata tells Confidential Space which container to run. Environment variables are passed via `tee-env-*` metadata keys.

##### 6. Configure firewall rules

Allow the Aileron server to reach the enclave on port 8443:

```sh
gcloud compute firewall-rules create allow-aileron-enclave \
  --project=$GCP_PROJECT \
  --allow=tcp:8443 \
  --target-tags=aileron-enclave \
  --source-ranges=0.0.0.0/0 \
  --description="Allow traffic to Aileron enclave"
```

For production, restrict `--source-ranges` to the IP range of your Aileron server.

##### 7. Get the enclave VM's internal IP

```sh
export ENCLAVE_IP=$(gcloud compute instances describe aileron-enclave \
  --zone=$REGION-a \
  --format='value(networkInterfaces[0].networkIP)' \
  --project=$GCP_PROJECT)

echo "Enclave URL: http://$ENCLAVE_IP:8443"
```

If the Aileron server runs outside the same VPC, use the external IP or set up a load balancer with TLS.

##### 8. Configure the Aileron server

Set these environment variables on the Aileron server service:

| Variable | Value |
|----------|-------|
| `AILERON_TEE_PROVIDER` | `confidential-space` |
| `AILERON_ENCLAVE_URL` | `http://<ENCLAVE_IP>:8443` (or internal DNS) |
| `AILERON_ENCLAVE_IMAGE_DIGEST` | The `sha256:...` digest from step 3 |
| `AILERON_GCP_PROJECT_ID` | Your GCP project ID |

##### 9. Verify

Check the enclave health:

```sh
curl http://$ENCLAVE_IP:8443/health
```

Check TEE status from the Aileron server:

```sh
curl https://api.yourdomain.com/v1/tee/status
```

Expected response:

```json
{"enabled":true,"provider":"confidential-space","attested":false,"session_active":false}
```

##### Network requirements

| From | To | Port | Purpose |
|------|----|------|---------|
| Aileron server | Enclave VM | 8443 | Attestation, session, execution requests |
| Enclave VM | External APIs | 443 | Gmail, Stripe, Google Calendar, etc. |
| Enclave VM | `metadata.google.internal` | 80 | GCE metadata service (attestation tokens) |
| Aileron server | `accounts.google.com` | 443 | OIDC discovery + JWKS for attestation verification |

The GCE metadata service is always accessible from within a GCP VM — no firewall rule needed.

##### How attestation works

1. The Aileron server calls `POST /v1/tee/attestation` which sends a random nonce to the enclave.
2. The enclave fetches an OIDC JWT from the GCE metadata service at `http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity`. This token is signed by Google and contains claims about the workload: container image digest, GCP project ID, and hardware model.
3. The server verifies the JWT signature against Google's JWKS (fetched from the OIDC discovery document at `https://accounts.google.com/.well-known/openid-configuration`).
4. The server validates: the issuer is `https://confidentialcomputing.googleapis.com`, the token is not expired, the nonce matches, and the image digest and project ID match the expected values.
5. On success, the server and enclave perform an ECDH key exchange (`POST /v1/tee/session`) to establish an encrypted channel.
6. Subsequent execution requests encrypt credentials with the session key before sending them to the enclave. The enclave decrypts inside its hardware-isolated memory, executes the connector, and returns only the structured result.

##### Updating the enclave

When you push a new enclave image:

1. Build and push the new image.
2. Record the new image digest.
3. Update `AILERON_ENCLAVE_IMAGE_DIGEST` on the Aileron server.
4. Restart the Confidential Space VM (it pulls the image reference from metadata on boot):
   ```sh
   gcloud compute instances stop aileron-enclave --zone=$REGION-a --project=$GCP_PROJECT
   gcloud compute instances start aileron-enclave --zone=$REGION-a --project=$GCP_PROJECT
   ```
5. The server will re-attest against the new image digest on its next attestation request.

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

**One shim catches everything.** `aileron-sh` is the agent's shell. Every command flows through it — no per-command wrappers. For agents that don't respect `$SHELL` directly (e.g. Claude Code), Aileron installs a wrapper script that satisfies the agent's shell validation and delegates to `aileron-sh`. Agent-specific command normalization (like unwrapping Claude Code's `eval` template) is gated on `AILERON_AGENT`.

**Policy as code.** `aileron.yaml` lives in the repo, is reviewable in PRs, and is version-controlled. Three buckets (allow, deny, ask) eliminate both rubber-stamping and alert fatigue.

**Deterministic enforcement.** No LLM in the policy loop. Rules are evaluated against command patterns and structured fields. The policy engine is predictable and auditable.

**Credential isolation.** Secrets live in a local encrypted vault. The agent uses them through a broker — it never sees raw credentials. Prompt injection can't leak what the agent doesn't have.

**The audit trail is append-only.** Every decision is written once and never modified. The log is the ground truth for what happened, not a narrative that can be cleaned up.

**Agent-portable.** Policy, credentials, and audit trail are agent-agnostic. Switch agents and everything carries over.

## License

See [LICENSE](LICENSE).
