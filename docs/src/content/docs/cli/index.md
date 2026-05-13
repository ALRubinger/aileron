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
| `aileron launch` | Start the local Aileron server. Prompts for the vault passphrase if one is set; creates a vault on first run. Listens on `http://localhost:8721/v1` by default. | [ADR-0011](/adr/0011-local-credential-vault) |
| `aileron status` | Report the running runtime: version, listen address, action count, connector count, binding count, vault state. Read-only; safe to run frequently. | — |

## Actions

| Command | Purpose | Ratified by |
|---|---|---|
| `aileron action add <FQN>@<version>` | Fetch an action template from the named source and copy it into `~/.aileron/actions/`. Walks declared connector dependencies and prompts for each. | [ADR-0003](/adr/0003-action-model), [ADR-0007](/adr/0007-install-consent) |
| `aileron action update <name>` | Fetch the latest version of an installed action's template; show a diff against the local file; apply on confirmation. | [ADR-0003](/adr/0003-action-model) |
| `aileron action list` | List every action in `~/.aileron/actions/` with its source FQN, version, and connector dependencies. | [ADR-0003](/adr/0003-action-model) |
| `aileron action remove <name>` | Delete an installed action file. Connectors no longer referenced are *not* automatically removed; use `aileron connector gc`. | [ADR-0003](/adr/0003-action-model) |

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
