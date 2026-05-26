---
title: "Repo Layout"
description: "Where each concern lives in the repo and which directories you can usually ignore."
order: 1
---

The repo is a Go workspace (`go.work` at the root) plus a handful of supporting trees for the docs, the local webapp, and deployment configuration. The directory tree is shallow on purpose; once you know which top-level folder owns a concern, navigation is straightforward.

## Top-level directories

```
cmd/              The four binaries Aileron ships (see Binary Architecture).
internal/         Almost all of Aileron's Go code. Every package here is
                  internal to this module — no third party imports it.
sdk/              Public Go SDK surface. Consumed by external callers
                  (currently small; grows when the API stabilizes).
test/             Cross-cutting integration tests that don't fit under
                  internal/.
docs/             This documentation site. Astro + Svelte + Markdown.
webapp/           The local-mode webapp (Svelte). Embedded into the
                  daemon binary at build time via internal/app/webapp_dist.
ui/               (Frozen) The cloud-tier UI. Not embedded into the
                  daemon; new work belongs in webapp/.
deploy/           Production deployment configs (Kubernetes, GCP). Not
                  used in the default local install.
saas/             SaaS-specific code paths. Off the default critical
                  path; see internal CLAUDE.md for scope.
```

## Inside `internal/`

`internal/` is where you spend most of your time. The packages cluster by concern:

| Package | What it owns |
|---|---|
| `internal/cstore` | The content-addressed connector store. Manifest schema, install pipeline, content hashing, FQN parsing, keyring trust. The source of truth for "what is a connector?" |
| `internal/action` | Action manifest parsing and the action executor. Loads `~/.aileron/actions/`, validates capability subsets, drives the sandbox per step. |
| `internal/sandbox` | The WASM runtime. `SpawnPolicy`, `HostPolicy`, host-function ABI, audit emission. |
| `internal/sandbox/sandboxtest` | Reusable test helpers for spawn-primitive integration tests. Connector repos consume this. |
| `internal/binding` | Capability bindings between connectors and vault entries. The user-visible link from "this connector wants OAuth2" to "this Google account." |
| `internal/credential` | Credential resolution at runtime. Sealed between the daemon and the connector; never leaves the daemon's address space. |
| `internal/vault` | The encrypted credential vault (per [ADR-0011](/adr/0011-local-credential-vault)). Argon2id + AES-256-GCM envelope. |
| `internal/oauth` | OAuth2 dance (PKCE, loopback redirect, refresh) per the publisher-owned model. |
| `internal/server` | The daemon's HTTP API. Auto-generated server interface from `internal/api/openapi.yaml`. |
| `internal/api` | OpenAPI spec plus its generated Go types. The spec is the source of truth; never hand-edit the generated files. |
| `internal/app` | The webapp embed and HTTP handlers. |
| `internal/launch` | `aileron launch` wiring: spawns the agent host with the MCP transport pointed at the daemon (per [ADR-0015](/adr/0015-launch-audit-scope)). Per-agent definitions live under `internal/launch/agents/`. |
| `internal/audit` | OTel-shaped audit event schema. Per-event attribute keys. |
| `internal/sessions` | Session records for `aileron launch` runs. |
| `internal/observability` | OTel exporters and trace propagation. |
| `internal/mcp`, `internal/mcpclient`, `internal/mcpremote` | MCP server, client, and remote-MCP pieces. |
| `internal/daemon` | Daemon lifecycle (auto-spawn, port discovery, graceful shutdown). |
| `internal/keyring` *(in `cstore`)* | Publisher signing-key trust. |

The full list is intentionally not exhaustive here. Run `ls internal/` for the current set; each package has a doc comment in its primary file.

## Generated code

A few directories contain generated code. Don't hand-edit:

- **`internal/api/gen/`** — Generated from `internal/api/openapi.yaml` by `task generate:api`. The OpenAPI spec is the source of truth (per the project's CLAUDE.md). Any API change starts in the spec.

## What you can usually ignore

- **`saas/`** — Off the default local install's critical path. Documented in its own CLAUDE.md.
- **`ui/`** — Frozen. The cloud-tier UI. New work belongs in `webapp/`.
- **`deploy/`** — Production Kubernetes / GCP configs. Not used for local development.
- **`docs/archive/`** — Historical content from before the post-pivot rewrite. Off-site; not part of the live site.

## See also

- [Binary Architecture](/development/binary-architecture/)
- [Building from Source](/development/building-from-source/)
- The project's root `CLAUDE.md` for the OpenAPI-spec-is-truth and Conventional-Commits rules.
