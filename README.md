# Aileron

**A programmable control plane for agentic AI systems.**

Aileron sits between an AI agent and the real world. When an agent wants to make a payment, open a pull request, send an email, or call an API, Aileron intercepts that action, evaluates it against policy, routes it for human approval if needed, and executes it through a secure abstraction layer — without the agent ever touching credentials directly.

---

## The Problem

AI agents are becoming capable enough to act on your behalf: booking meetings, paying invoices, merging code. The problem isn't capability — it's trust. Granting an agent real authority means accepting real risk.

The current workarounds are inadequate:

- Hardcoded rules buried inside agents, invisible to the people they affect
- Manual approvals handled out-of-band in Slack or email, disconnected from execution
- Over-permissioned API keys that agents hold directly
- Static payment credentials exposed to systems that shouldn't see them

The result is a forced choice: give the agent enough permission to be useful, or restrict it enough to feel safe. Neither is satisfying.

## The Solution

Aileron introduces a missing layer in the agentic stack: a **control plane that mediates execution**.

Agents remain autonomous in planning. Humans retain authority over execution. Aileron enforces the boundary between the two.

```
Agent
  │
  ▼
Aileron Control Plane
  ├── Policy Engine       evaluates rules, assesses risk
  ├── Approval Orchestrator  routes to humans when required
  ├── Execution Layer     runs the action via secure connectors
  └── Audit Store         immutable record of every decision
  │
  ▼
Payments / APIs / Services
```

## How It Works

**1. An agent submits an intent**

Instead of calling Stripe or GitHub directly, the agent submits a structured intent to Aileron describing what it wants to do, why, and on behalf of whom.

**2. The policy engine decides**

Aileron evaluates the intent against your configured rules — spend limits, vendor allowlists, environment constraints, time-of-day controls. The engine returns one of five dispositions: `allow`, `allow_modified`, `require_approval`, `deny`, or `require_more_evidence`.

**3. Humans approve high-risk actions**

If approval is required, Aileron notifies the right people (via Slack or email) with a link to the approval UI. Approvers can approve, deny, or modify the action — for example, approving a payment but reducing the amount. The agent waits.

**4. Aileron executes, not the agent**

Once approved, Aileron executes the action through a connector — injecting credentials from the vault at execution time. The agent never sees the API key, the card number, or the OAuth token.

**5. Everything is logged**

Every intent, decision, approval action, and execution outcome is written to an immutable audit trail. You have a verifiable record of what every agent did, who approved it, and when.

## Connectors

Aileron uses a connector model to integrate with external systems. Each connector implements a common interface and handles one provider.

| Domain | MVP Connector |
|---|---|
| Payments | Stripe |
| Calendar | Google Calendar |
| Git | GitHub |

Additional connectors can be implemented independently without modifying the core.

## Current Status

The core control plane lifecycle is implemented end-to-end:

- **Policy engine** evaluates rules against intents (allow, deny, require approval)
- **Approval orchestrator** manages human-in-the-loop workflows with approve/deny/modify
- **Execution layer** runs approved actions via connectors with injected credentials
- **Audit store** records every event in an immutable trace
- **GitHub connector** creates, merges, and closes PRs via the GitHub REST API
- **MCP server** exposes Aileron tools to Claude Code (submit_intent, check_approval, execute_action)
- **Approval UI** provides a web interface for reviewing and acting on pending approvals

Three seed policies ship by default:
1. Require approval for PRs targeting `main`, `master`, or `production`
2. Allow PRs to feature branches
3. Deny force pushes

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

This builds everything: Docker containers, Go server binary, MCP server binary, and the UI.

To build individual components:

```sh
task build:docker   # Docker containers (server, UI, database)
task build:server   # Go server binary
task build:ui       # SvelteKit UI
task build:mcp      # MCP server binary for Claude Code
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
{"status":"ok","service":"aileron","version":"0.0.1","timestamp":"2026-03-31T09:00:00Z"}
```

### Connect Claude Code via MCP

Build the MCP server and add it to your project's `.mcp.json`:

```sh
task build:mcp
```

```json
{
  "mcpServers": {
    "aileron": {
      "command": "./aileron-mcp",
      "env": {
        "AILERON_API_URL": "http://localhost:8080"
      }
    }
  }
}
```

Claude Code can then use `submit_intent`, `check_approval`, `execute_action`, and `list_pending_approvals` as tools.

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
├── core/               Control plane — policy, approval, connectors, vault, audit
│   ├── api/            OpenAPI specification and generated code
│   ├── server/         HTTP server entry point and API handlers
│   ├── policy/         Policy engine SPI, rule-based implementation, seed policies
│   ├── approval/       Approval orchestrator SPI and implementation
│   ├── connector/      Connector SPI and MVP implementations (GitHub, Stripe, Google Calendar)
│   ├── store/          Persistence interfaces and in-memory implementations
│   ├── vault/          Credential vault SPI and in-memory implementation
│   ├── notify/         Notification SPI (log, Slack, email)
│   ├── audit/          Immutable audit store SPI
│   └── model/          Shared domain types
├── cmd/
│   └── aileron-mcp/    MCP server for Claude Code integration
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

## Architecture Principles

**SPIs throughout.** Every major subsystem — the policy engine, approval orchestrator, connectors, vault, and notifiers — is defined as a Go interface. Built-in implementations cover the common cases. Alternative implementations can be swapped in without modifying the core.

**Credentials never reach agents.** The vault resolves secrets at execution time and injects them transiently into connectors. Agents interact with Aileron using intent descriptions, not credentials.

**The audit trail is append-only.** Every event is written once and never modified. The trail is the ground truth for what happened, not a log that can be cleaned up.

**OSS core, SaaS overlay.** Everything in `core/`, `sdk/`, and `ui/` is open source. The `saas/` layer adds multi-tenancy and billing on top without forking the core.

## License

See [LICENSE](LICENSE).
