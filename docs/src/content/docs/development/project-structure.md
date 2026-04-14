---
title: "Project Structure"
description: "How the Aileron codebase is organized"
---

```
aileron/
├── cmd/
│   ├── aileron/         CLI entry point: aileron launch, aileron version
│   ├── aileron-sh/      Shell shim: intercepts agent commands for policy evaluation
│   ├── aileron-mcp/     MCP server exposing tools to agent hosts
│   └── aileron-enclave/ TEE enclave binary (confidential computing)
├── core/                Core library: policy, launch, vault, auth, connectors
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
├── docs/                Documentation site (Astro)
├── test/
│   └── integration/     Integration tests with OpenAPI spec validation
└── deploy/
    └── docker-compose.yml  Self-hosted deployment
```

## Key design principles

**One shim catches everything.** `aileron-sh` is the agent's shell. Every command flows through it. For agents that don't respect `$SHELL` directly, Aileron installs a wrapper script that delegates to `aileron-sh`.

**Policy as code.** `aileron.yaml` lives in the repo, is reviewable in PRs, and is version-controlled.

**Deterministic enforcement.** No LLM in the policy loop. Rules are evaluated against command patterns and structured fields.

**Credential isolation.** Secrets live in an encrypted vault. The agent uses them through a broker and never sees raw credentials.

**Append-only audit trail.** Every decision is written once and never modified.

**Agent-portable.** Policy, credentials, and audit trail are agent-agnostic.
