---
title: "CLI Reference"
description: "The aileron command-line interface. Every command is a thin HTTP client over the Aileron server's OpenAPI-spec'd API."
order: 1
---

The `aileron` CLI is a thin HTTP client over the Aileron server's API. The same CLI talks to a locally-running Aileron (the v1 default) or — eventually — a hosted Aileron Cloud, with no client-side change beyond the configured endpoint.

The server's API is defined in [`internal/api/openapi.yaml`](https://github.com/ALRubinger/aileron/blob/main/internal/api/openapi.yaml) and is the **authoritative source** for every endpoint. CLI commands are routed to specific HTTP operations there. New CLI surface lands by extending the OpenAPI spec, regenerating the server stubs (`task generate:api`), implementing the handler, and wiring the CLI command to call it.

This page is the human-readable index of CLI commands grouped by concern. Each command lists its purpose, its subcommands, and its syntax. Run `aileron help` or `aileron <command>` with no subcommand to see the same syntax from the binary itself.

## Runtime lifecycle

| Command | Purpose |
|---|---|
| `aileron launch [--log-level=<level>] [--local] [--sandbox=auto\|docker] [--sandbox-build=auto\|always\|never] [--sandbox-proxy=auto\|on\|off] [--host-login=auto\|on\|off] [--claude-auth=subscription\|api-key] <agent> [args...]` | Launch an AI coding agent connected to the Aileron daemon. The Docker sandbox is the default; the agent command runs inside the prepared project sandbox image. `--local` runs the agent directly on the host instead, and conflicts with an explicit `--sandbox`. |
| `aileron status [runtime\|notifications\|vault]` | Report the running runtime: version, listen address, action count, connector count, binding count, vault state. The optional section argument narrows output to one of `runtime`, `notifications`, or `vault`. Read-only; safe to run frequently. |
| `aileron daemon start\|stop\|status` | Manage the local Aileron daemon. `start` is idempotent and prints the URL if one is already running. `stop` sends SIGTERM. `status` reports whether the daemon is running and its vault state. The daemon also auto-spawns on demand for any command that needs it. |
| `aileron stop` | Alias for `aileron daemon stop`. |

The `--claude-auth` flag only affects the `claude` agent; every other agent ignores it. An empty value (the default) prompts on a TTY and resolves to `subscription` off a TTY.

## Sandbox composition

The sandbox group is `aileron sandbox [init|plan|build|check|cache]`.

| Command | Purpose |
|---|---|
| `aileron sandbox init [--force]` | Scaffold a Feature-composing `.devcontainer/devcontainer.json` for Tier 1 sandbox composition. It sets `ghcr.io/alrubinger/aileron-sandbox-base:<version>` as the base image, lists the Claude agent Feature as the worked example, and shows a commented customer-tooling Feature slot. |
| `aileron sandbox plan` | Inspect the normalized composition tier and image Aileron infers from the current project. |
| `aileron sandbox build [--runtime=auto\|docker] [--tag=<image>]` | Build the Tier 0 sandbox-base image or Tier 1 devcontainer image with Docker. Tier 2 BYO images are reported without build or injection. |
| `aileron sandbox check [--runtime=auto\|docker] [--build=auto\|always\|never] [--agent=<command>] [command]` | Validate that the selected sandbox image can launch an agent command before starting a session. |
| `aileron sandbox cache clear [--runtime=auto\|docker]` | Remove cached sandbox build artifacts so the next build starts clean. |

Docker is the only supported sandbox runtime in v4. Podman is planned but not yet supported ([ADR-0014](/adr/0014-spawn-sandbox-technology/)); passing `--sandbox=podman` or `--runtime=podman` fails with `podman runtime is not supported yet (v4 is Docker-only); see ADR-0014`.

See [Sandbox Composition](/development/sandbox-composition/) for the full workflow and [Sandbox Agent Images](/development/sandbox-agent-images/) for the support matrix and recipes.

## Actions

| Command | Purpose |
|---|---|
| `aileron action add <FQN> [--version=<v>] [--force] [--yes] [--no-bind]` | Fetch an action template from the named source and copy it into `~/.aileron/actions/`. Walks declared connector dependencies and prompts for each. On a fresh machine it also prompts to trust the publisher's signing key and to consent to each connector install. |
| `aileron action add-suite <SOURCE> [--force] [--yes] [--no-bind]` | Install a suite of actions in declaration order, sharing trust and connector state across the set so a publisher's prompt fires at most once. `<SOURCE>` is a remote `<scheme>://<owner>/<repo>/<file-path>@<ref>` or a local filesystem path. |
| `aileron action list [--json]` | List every action in `~/.aileron/actions/` with its source FQN, version, and connector dependencies. |
| `aileron action run <NAME> [--arg k=v ...] [--args <json>] [--json] [--audit-id-out <path>]` | Invoke an installed action directly from the terminal through the same daemon endpoint used by the agent-facing MCP server. Useful for smoke tests, connector diagnostics, and scripts. |
| `aileron action enable <NAME>` | Re-surface a disabled action to the LLM. The change is recorded in `~/.aileron/action-state.json`. |
| `aileron action disable <NAME>` | Hide an installed action from the LLM without uninstalling it. The manifest file is left in place; the change is recorded in `~/.aileron/action-state.json`. |

MCP servers that cached the tool list at boot need to be restarted before an enable or disable toggle takes effect with the LLM.

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

| Command | Purpose |
|---|---|
| `aileron connector install <FQN> [--version=<v>] [--hash=<sha256:...>] [--yes]` | Install a connector by FQN: fetch, verify signature, verify hash, store in the content-addressed store at `~/.aileron/store/`. Pass `--yes` to auto-accept install consent in non-interactive use. |
| `aileron connector check [--include-prerelease] [--json]` | Walk every connector referenced by installed actions; query upstream sources for newer versions; print a per-connector update report. Network-dependent. Pre-release versions are excluded unless `--include-prerelease` is passed. |

## Keyring

The keyring is the v1 source of trust for connector signature verification. Every `aileron connector install` checks the binary's signature against keys registered for the FQN's authority; without an entry, install fails closed. The keyring file lives at `~/.aileron/keyring.json`.

| Command | Purpose |
|---|---|
| `aileron keyring trust <authority>` | Authorize a publisher's signing key at owner granularity, so the single grant covers every connector that publisher ships. The key is resolved automatically: `github://owner/connector` reads that repo's `keys/publisher.pub` on its default branch, and a bare `github://owner` resolves the key from the Hub catalog entry's `key_url`. Re-running is a safe no-op. |
| `aileron keyring list` | List trusted publishers and their key fingerprints. |
| `aileron keyring revoke <authority>` | Remove a publisher's keys from the trust list. Pass `--key <fingerprint>` instead of an authority to revoke a single key by fingerprint. |

## Hub

| Command | Purpose |
|---|---|
| `aileron hub list [connectors\|actions\|suites] [--json]` | List entries published to the Hub. The optional positional argument narrows to one catalog type; with none, all types are listed. |
| `aileron hub search <query> [--type connectors\|actions\|suites] [--json]` | Search Hub entries by keyword, optionally filtered to one catalog type. |
| `aileron hub show <FQN> [--type connectors\|actions\|suites] [--json]` | Show a single Hub entry by FQN. |

## Secrets

| Command | Purpose |
|---|---|
| `aileron secret set [--passphrase-file <path>] <name>` | Store a secret in the encrypted vault. The first secret on a new vault prompts to create and confirm the vault passphrase. Reference the stored value as `vault:secret/<name>` in `aileron.yaml`. |
| `aileron secret list [--json]` | List stored secret names. The secret value is never printed. |

## Bindings

| Command | Purpose |
|---|---|
| `aileron binding setup <connector-FQN>` | Pre-bind every capability the named connector declares. Used for headless setup and proactive setup. |
| `aileron binding list [--connector FQN] [--kind KIND] [--json]` | List every binding on this machine: kind, scope, identity, last-used timestamp, refresh status. |
| `aileron binding inspect <name>` | Show one binding's metadata: capability type, scope, account, created/last-used/last-refresh timestamps, connectors that use it. |
| `aileron binding rebind <name>` | Replace an existing binding with a fresh credential (after rotation, after revocation). Same flow as first-use binding. |
| `aileron binding reauthorize <name>` | Re-run the authorization flow for an existing binding, for example to widen scope or recover from an upstream revocation, keeping the binding's name. |
| `aileron binding revoke <name>` | Remove a binding entirely. Subsequent invocations of any connector that would have used it trigger first-use binding. |

## Sync and verify

| Command | Purpose |
|---|---|
| `aileron sync` | Walk `~/.aileron/actions/`; install any missing connectors into the local store; surface unbound capability requirements. Idempotent. |
| `aileron sync --bind-all` | Like `sync`, but additionally pre-binds every required capability before exiting. Useful for fresh-machine setup. |
| `aileron sync --yes` | Headless mode. Auto-approves install consent for every new connector encountered. Per-command flag; no global config. |

## Vault

| Command | Purpose |
|---|---|
| `aileron vault init [--passphrase-file <path>]` | Create the local file vault. Deliberate first-run flow; refuses to overwrite an existing vault. |
| `aileron vault put agents/<name>/<purpose> --from-file <path>` | Write a per-agent credential envelope from a file, bytes verbatim. The `<purpose>` segment (e.g. `oauth`, `apikey`) selects which credential to write. Daemon-backed; agents namespace only. |
| `aileron vault delete <path-as-listed> [--yes]` | Delete a credential. The argument is any path `vault list` prints: an agent entry (`agents/<name>/<purpose>`), a user entry (`user/<service>`), or a binding (`<kind>/<service>/<identity>`). It dispatches to the matching namespace's delete endpoint. Confirms first unless `--yes`. Daemon-backed. |
| `aileron vault list [--scope agent\|user\|all] [--prefix agents/] [--include-control-plane] [--json]` | List vault entries, metadata only. The credential value is never listed. With no `--scope` it prints the union of every locally-owned namespace (agent, user, and capability-binding credentials, which include connector entries like `connectors/<provider>/default` and `oauth2/<provider>/...`), each line prefixed with its scope. `--scope` narrows to a single typed namespace; `--include-control-plane` adds the `connected-accounts/` and `llm-config/` namespaces to the union. |

The `put` and `delete` verbs talk to the running daemon and never open the vault file directly. `put` is agents-only: you create a binding with `binding setup`, not `vault put`, so it accepts only the `agents/<name>/<purpose>` namespace and rejects any other path. `delete` is symmetric with `list`: it classifies the path and dispatches to the matching namespace-scoped daemon endpoint, so anything `list` prints — an agent entry, a `user/<service>` entry, or a binding — is deletable. The tenant-keyed control-plane namespaces (`connected-accounts/`, `llm-config/`) are managed by the control plane, not this CLI, so `delete` rejects them with a message naming the supported paths. For agent entries the `<purpose>` segment must match `^[a-z0-9][a-z0-9_-]*$` and defaults to `oauth` for entries an older daemon returns without one. A listed line can be pasted straight into `delete`. An agent holding both an OAuth envelope and an API key surfaces as two distinct lines (`agents/<name>/oauth` and `agents/<name>/apikey`).

`vault list` itself is read-only and broader: the default (no `--scope`) walks the vault once and surfaces every locally-owned namespace, so connector and capability-binding credentials appear alongside the agent and user entries instead of being silently hidden. Listing works on a locked vault because it never decrypts a stored value (metadata is plaintext). The two tenant-keyed control-plane namespaces (`connected-accounts/`, `llm-config/`) are excluded unless `--include-control-plane` is passed. See [Sandbox Agent Auth](/development/sandbox-agent-auth/) for the seeding and recovery flows.

## Auth

| Command | Purpose |
|---|---|
| `aileron auth <agent> --import-from-host` | Seed the vault from an already-authenticated host install of claude or codex. |
| `aileron auth github [--runtime <auto\|docker>] [--image <ref>]` | Run gh's OAuth device-authorization flow inside a gh-bearing container and store the captured bearer token at `user/github`. HTTPS only; no refresh machinery. |

`aileron auth github` is a one-time acquisition flow. The `github` verb resolves the shipped `gh` capture descriptor by name and drives the generic capture flow. That flow runs `gh auth login` and `gh auth token` in the same container, then PUTs the token to the user-level `user/github` vault namespace through the daemon. The provider knowledge ships as a trusted YAML descriptor in the auth domain, so adding another tool is a new descriptor rather than new code.

## Approvals

| Command | Purpose |
|---|---|
| `aileron approval list` | List pending action-approval requests. |
| `aileron approval approve <id> [--reason "..."]` | Approve a pending action so the agent's blocked tool call unblocks. The optional `--reason` is recorded with the decision. |
| `aileron approval deny <id> [--reason "..."]` | Deny a pending action so the agent receives `approval_denied`. The optional `--reason` is recorded with the decision. |

## Audit

| Command | Purpose |
|---|---|
| `aileron audit list [--since RFC3339] [--audit-id ID] [--connector FQN] [--class CLASS] [--limit N] [--json]` | List action-execution audit-log entries, optionally filtered by time, audit id, connector, or class. |
| `aileron audit show <audit-id>` | Show the full record for one audit-log entry. |

## Sessions

| Command | Purpose |
|---|---|
| `aileron sessions list [--active] [--agent NAME] [--since RFC3339] [--limit N] [--json]` | List `aileron launch` session records. A bare `aileron sessions` lists too, answering "what did I run today?" without ceremony. |
| `aileron sessions get <session-id> [--json]` | Show one session record by id. |
| `aileron sessions watch <session-id> [--no-follow]` | Stream a session's events as they arrive. Pass `--no-follow` to print the current snapshot and exit. |

## Browser

| Command | Purpose |
|---|---|
| `aileron open [approvals \| approval <id>]` | Open the Aileron webapp in your browser. With no argument it opens the root; `approvals` opens the approvals view; `approval <id>` focuses one approval, mirroring the URL the terminal notifier prints. |

## Utility

| Command | Purpose |
|---|---|
| `aileron version` | Print the runtime version. |
| `aileron help [<command>]` | Show CLI help. |

## Architecture: CLI is an HTTP client

Every command above resolves to one or more HTTP operations against the Aileron server. The server may be:

- **Local** — a process the user started with `aileron launch`, listening on a daemon-advertised address (the v1 default).
- **Hosted** — a future Aileron Cloud deployment reachable over HTTPS (post-MVP).

The CLI doesn't care which. It reads its target endpoint from configuration, opens HTTP, and invokes the spec'd operations. This separation means:

- New server functionality is added by extending [`internal/api/openapi.yaml`](https://github.com/ALRubinger/aileron/blob/main/internal/api/openapi.yaml), regenerating Go stubs (`task generate:api`), and implementing the handler.
- New CLI commands are shells over those endpoints — argument parsing, output formatting, occasionally orchestrating multiple calls.
- The same CLI binary works against any conformant Aileron server.

The OpenAPI spec is the authoritative source of truth for every API change. CLI commands are documentation of which commands wrap which endpoints; the underlying contract lives in the spec.

For the full HTTP API, see [API Reference](/api).
