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
├── core/               Control plane — policy, approval, vault, audit
│   ├── api/            OpenAPI specification and generated code
│   ├── app/            Application wiring (handlers, middleware) — importable library
│   ├── server/         HTTP server entry point
│   ├── policy/         Policy engine SPI, rule-based implementation, seed policies
│   ├── approval/       Approval orchestrator SPI and implementation
│   ├── config/         YAML configuration loading and validation
│   ├── mcpclient/      MCP client for downstream server connections
│   ├── connector/      Connector SPI and implementations
│   ├── store/          Persistence interfaces and in-memory implementations
│   ├── vault/          Credential vault SPI and in-memory implementation
│   ├── notify/         Notification SPI (log, Slack, email)
│   ├── audit/          Immutable audit store SPI
│   └── model/          Shared domain types
├── cmd/
│   └── aileron-mcp/    MCP gateway that aggregates and proxies downstream MCP servers
├── sdk/
│   └── go/             Go client SDK
├── ui/                 Management and approval UI (SvelteKit)
│   └── src/routes/     Pages: approvals, traces, policies
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

## Architecture Principles

**Structural enforcement.** Aileron is the only MCP server the agent connects to. Governance is enforced by construction, not by cooperation. The agent cannot bypass policy because there is no alternative path to the tools.

**SPIs throughout.** Every major subsystem — the policy engine, approval orchestrator, vault, and notifiers — is defined as a Go interface. Built-in implementations cover the common cases. Alternative implementations can be swapped in without modifying the core.

**Credentials never reach agents.** The vault resolves secrets at launch time and injects them into downstream MCP servers. Agents interact with tools through Aileron, never with credentials.

**The audit trail is append-only.** Every event is written once and never modified. The trail is the ground truth for what happened, not a log that can be cleaned up.

**OSS core, SaaS overlay.** Everything in `core/`, `sdk/`, and `ui/` is open source. The `saas/` layer adds multi-tenancy and billing on top without forking the core.

## License

See [LICENSE](LICENSE).
