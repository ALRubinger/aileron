---
title: "CLI Reference"
description: "The aileron command-line interface. Every command is a thin HTTP client over the Aileron server's OpenAPI-spec'd API."
order: 1
---

The `aileron` CLI is a thin HTTP client over the Aileron server's API. The same CLI talks to a locally-running Aileron (the v1 default) or — eventually — a hosted Aileron Cloud, with no client-side change beyond the configured endpoint.

The server's API is defined in [`internal/api/openapi.yaml`](https://github.com/ALRubinger/aileron/blob/main/internal/api/openapi.yaml) and is the **authoritative source** for every endpoint. CLI commands are routed to specific HTTP operations there. New CLI surface lands by extending the OpenAPI spec, regenerating the server stubs (`task generate:api`), implementing the handler, and wiring the CLI command to call it.

This page is the human-readable index of CLI commands grouped by concern. Each command lists its purpose, its options, and the ADR(s) that ratify its behavior.

## Runtime lifecycle

| Command | Purpose | Ratified by |
|---|---|---|
| `aileron launch [--sandbox=off\|auto\|docker\|podman] [--sandbox-build=auto\|always\|never]` | Launch an AI coding agent connected to the Aileron daemon. `--sandbox` prepares the project sandbox image and runs the agent command inside it; `off` preserves direct host launch. | [ADR-0011](/adr/0011-local-credential-vault), [ADR-0017](/adr/0017-sandbox-composition) |
| `aileron status` | Report the running runtime: version, listen address, action count, connector count, binding count, vault state. Read-only; safe to run frequently. | — |

## Sandbox composition

| Command | Purpose | Ratified by |
|---|---|---|
| `aileron sandbox init [--agent=<name>] [--force]` | Scaffold `.devcontainer/devcontainer.json` and `.devcontainer/Dockerfile` for sandbox composition. The Dockerfile extends `aileron/sandbox-base:<version>`, pre-fills the install recipe for `--agent` (defaults to `claude`), and ships additional tool snippets commented out. | [ADR-0017](/adr/0017-sandbox-composition) |
| `aileron sandbox plan` | Inspect the normalized composition tier and image Aileron infers from the current project. | [ADR-0017](/adr/0017-sandbox-composition) |
| `aileron sandbox build` | Build the Tier 0 sandbox-base image or Tier 1 devcontainer image with Docker or Podman. Tier 2 BYO images are reported without build or injection. | [ADR-0017](/adr/0017-sandbox-composition) |
| `aileron sandbox check [--runtime=auto\|docker\|podman] [--build=auto\|always\|never] --agent=<command>` | Validate that the selected sandbox image can launch an agent command before starting a session. | [ADR-0017](/adr/0017-sandbox-composition) |

See [Sandbox Composition](/development/sandbox-composition/) for the full workflow and [Sandbox Agent Images](/development/sandbox-agent-images/) for the support matrix and recipes.

## Actions

| Command | Purpose | Ratified by |
|---|---|---|
| `aileron action add <FQN>@<version>` | Fetch an action template from the named source and copy it into `~/.aileron/actions/`. Walks declared connector dependencies and prompts for each. | [ADR-0003](/adr/0003-action-model), [ADR-0007](/adr/0007-install-consent) |
| `aileron action update <name>` | Fetch the latest version of an installed action's template; show a diff against the local file; apply on confirmation. | [ADR-0003](/adr/0003-action-model) |
| `aileron action list` | List every action in `~/.aileron/actions/` with its source FQN, version, and connector dependencies. | [ADR-0003](/adr/0003-action-model) |
| `aileron action run <name>` | Invoke an installed action directly from the terminal through the same daemon endpoint used by the agent-facing MCP server. Useful for smoke tests, connector diagnostics, and scripts. | [ADR-0003](/adr/0003-action-model), [ADR-0008](/adr/0008-intent-matching), [ADR-0009](/adr/0009-user-channel) |
| `aileron action remove <name>` | Delete an installed action file. Connectors no longer referenced are *not* automatically removed; use `aileron connector gc`. | [ADR-0003](/adr/0003-action-model) |

### `aileron action run`

```bash
aileron action run <name> [--arg k=v ...] [--args <json>] [--json] [--audit-id-out <path>]
```

`<name>` is the installed action name shown by `aileron action list`, such as `sentry-organizations-list` or `linear-issues-create`. The CLI posts to `POST /v1/actions/{name}/run`; there is no separate execution path or weaker approval path for terminal invocations.

Arguments can be supplied in either of two mutually exclusive forms:

```bash
aileron action run linear-issues-create \
  --arg team=ENG \
  --arg title='Smoke test'

aileron action run linear-issues-create \
  --args '{"labels":["bug","triage"],"priority":2}'
```

`--arg k=v` is repeatable and sends string values. `--args <json>` sends a raw JSON object for nested values, arrays, numbers, or booleans.

By default, successful calls print:

```text
wrapped output:
<result>
```

Use `--json` to print the daemon response envelope exactly as returned, including `audit_id` and `result`. Use `--audit-id-out <path>` to write the successful execution's `audit_id` to a file for follow-up audit-log tooling.

Approval-gated actions return an approval-pending message instead of running immediately. The CLI prints the review URL or `aileron approval approve <id>` command and exits with code `75` (`EX_TEMPFAIL`) so scripts can distinguish "approval needed" from ordinary failures.

## Connectors

| Command | Purpose | Ratified by |
|---|---|---|
| `aileron connector install <FQN>@<version>` | Install a connector by FQN: fetch, verify signature, verify hash, store in the content-addressed store at `~/.aileron/store/`. | [ADR-0002](/adr/0002-connector-model), [ADR-0004](/adr/0004-dependency-resolution), [ADR-0007](/adr/0007-install-consent) |
| `aileron connector check` | Walk every connector referenced by installed actions; query upstream sources for newer versions; print a per-connector update report. Network-dependent. Pre-release versions excluded by default. | [ADR-0004](/adr/0004-dependency-resolution) |
| `aileron connector update <FQN>` | Bump the version reference for a connector across all action files that use it, after explicit confirmation. | [ADR-0003](/adr/0003-action-model), [ADR-0004](/adr/0004-dependency-resolution) |
| `aileron connector list` | List every connector in the local store with its FQN, version, hash, and which installed actions reference it. | [ADR-0004](/adr/0004-dependency-resolution) |

## Bindings

| Command | Purpose | Ratified by |
|---|---|---|
| `aileron binding setup [<connector-FQN>]` | Pre-bind every capability the connector declares (or every capability across all installed connectors when run without an argument). Used for headless setup and proactive setup. | [ADR-0006](/adr/0006-capability-binding-ux) |
| `aileron binding list` | List every binding on this machine: kind, scope, identity, last-used timestamp, refresh status. | [ADR-0006](/adr/0006-capability-binding-ux) |
| `aileron binding inspect <name>` | Show one binding's metadata: capability type, scope, account, created/last-used/last-refresh timestamps, connectors that use it. | [ADR-0006](/adr/0006-capability-binding-ux) |
| `aileron binding rebind <name>` | Replace an existing binding with a fresh credential (after rotation, after revocation). Same flow as first-use binding. | [ADR-0006](/adr/0006-capability-binding-ux) |
| `aileron binding revoke <name>` | Remove a binding entirely. Subsequent invocations of any connector that would have used it trigger first-use binding. | [ADR-0006](/adr/0006-capability-binding-ux) |

## Sync and verify

| Command | Purpose | Ratified by |
|---|---|---|
| `aileron sync` | Walk `~/.aileron/actions/`; install any missing connectors into the local store; surface unbound capability requirements. Idempotent. | [ADR-0004](/adr/0004-dependency-resolution) |
| `aileron sync --bind-all` | Like `sync`, but additionally pre-binds every required capability before exiting. Useful for fresh-machine setup. | [ADR-0006](/adr/0006-capability-binding-ux) |
| `aileron sync --yes` | Headless mode. Auto-approves install consent for every new connector encountered. Per-command flag; no global config. | [ADR-0007](/adr/0007-install-consent) |

## Utility

| Command | Purpose | Ratified by |
|---|---|---|
| `aileron version` | Print the runtime version. | — |
| `aileron help [<command>]` | Show CLI help. | — |

## Architecture: CLI is an HTTP client

Every command above resolves to one or more HTTP operations against the Aileron server. The server may be:

- **Local** — a process the user started with `aileron launch`, listening on `localhost:8721/v1` (the v1 default).
- **Hosted** — a future Aileron Cloud deployment reachable over HTTPS (post-MVP, paired with the hosted backend introduced in [ADR-0009](/adr/0009-user-channel) Phase 2).

The CLI doesn't care which. It reads its target endpoint from configuration, opens HTTP, and invokes the spec'd operations. This separation means:

- New server functionality is added by extending [`internal/api/openapi.yaml`](https://github.com/ALRubinger/aileron/blob/main/internal/api/openapi.yaml), regenerating Go stubs (`task generate:api`), and implementing the handler.
- New CLI commands are shells over those endpoints — argument parsing, output formatting, occasionally orchestrating multiple calls.
- The same CLI binary works against any conformant Aileron server.

The OpenAPI spec is the authoritative source of truth for every API change. CLI commands are documentation of which commands wrap which endpoints; the underlying contract lives in the spec.

For the full HTTP API, see [API Reference](/api).
