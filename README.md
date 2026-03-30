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

## Getting Started

### Prerequisites

- [Go](https://go.dev/dl/) 1.23 or later
- [Docker](https://docs.docker.com/get-docker/) and Docker Compose
- [Node.js](https://nodejs.org/) 22 or later (nvm users: `.nvmrc` is provided)
- [pnpm](https://pnpm.io/installation) — JavaScript package manager
- [Task](https://taskfile.dev/installation/) — task runner

### Common tasks

| Task | Description |
|------|-------------|
| `task build` | Build server binary and UI |
| `task build:server` | Build the Go server binary → `bin/aileron-server` |
| `task build:ui` | Install UI deps and build Next.js |
| `task test` | Run Go tests |
| `task lint` | Run Go vet and UI lint + typecheck |
| `task up` | Start full stack with Docker Compose |
| `task down` | Stop the stack |
| `task logs` | Tail container logs |
| `task dev:server` | Run Go server locally (no Docker) |
| `task dev:ui` | Run Next.js dev server |
| `task clean` | Remove build artifacts |

### Build the server

```sh
task build:server
```

### Run locally with Docker Compose

```sh
task up      # start the full stack (API, UI, Postgres) in the background
task down    # stop and remove containers
task logs    # tail all service logs
```

This starts the control plane API server, the management UI, and a PostgreSQL database. The API is available at `http://localhost:8080` and the UI at `http://localhost:3000`.

### Verify

```sh
curl http://localhost:8080/v1/health
```

```json
{"status":"ok","service":"aileron","version":"0.1.0"}
```

## Project Structure

```
aileron/
├── core/               Control plane — policy, approval, connectors, vault, audit
│   ├── api/            OpenAPI specification
│   ├── server/         HTTP server entry point
│   ├── policy/         Policy engine SPI and built-in implementation
│   ├── approval/       Approval orchestrator SPI and implementation
│   ├── connector/      Connector SPI and MVP implementations
│   ├── vault/          Credential vault SPI
│   ├── notify/         Notification SPI (Slack, email)
│   └── audit/          Immutable audit store SPI
├── sdk/
│   ├── go/             Go client SDK
│   ├── python/         Python client SDK (coming soon)
│   └── typescript/     TypeScript client SDK (coming soon)
├── ui/                 Management and approval UI (TypeScript / Next.js)
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
