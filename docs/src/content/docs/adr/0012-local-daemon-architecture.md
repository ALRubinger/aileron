---
title: "ADR-0012: Local Daemon Architecture"
description: "Aileron runs as a single long-lived user-scoped local daemon. CLI commands and aileron launch are thin clients that auto-spawn the daemon on first use and connect over TCP-on-loopback at an ephemeral port advertised via a discovery file. Vault unlock, sessions, and audit logs all live in the daemon. Cloud deployments are a separate shape and out of scope for this decision."
order: 12
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-05-06</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/454">#454</a></td></tr>
</table>
</div>

## Context

> **Revision note, 2026-06-01:** This ADR describes the local daemon architecture that current CLI and host launch use. The v4 sandbox runtime keeps the daemon as the host-side control/data-plane authority, but container-facing calls use runtime-provided env such as `AILERON_API_URL` and later `HTTPS_PROXY` / session CA bootstrap. See [ADR-0018](/adr/0018-v4-single-binary-runtime) and [ADR-0019](/adr/0019-v4-https-data-plane).

The local Aileron runtime today is split awkwardly across three command shapes:

1. **`aileron serve`** — a long-running standalone HTTP server on `localhost:8721`. CLI commands like `aileron binding list` connect to it via `AILERON_API_URL`.
2. **CLI subcommands** (`aileron action add`, `aileron binding setup`, etc.) — each connects to the standalone server when one is running, and fails or behaves degraded when one is not.
3. **`aileron launch <agent>`** — spawns a *per-session embedded gateway* on a random localhost port, used only for the lifetime of the agent process. The embedded gateway is wholly independent of the standalone server: separate process, separate vault state, separate URL.

This shape produces three problems in practice:

- **Users must run `aileron serve` manually before CLI commands work.** Forgetting this is the most common first-run failure. The standalone server is conceptually invisible — users want to "use Aileron," not run a server.
- **Port conflicts surface to users.** Two `aileron serve` instances collide on `8721`. Users have to know about ports, bind addresses, and `--bind` flags.
- **Vault unlock state isn't shared across processes.** A user who unlocks the vault on the standalone server still gets prompted again when running `aileron launch claude`, because the per-session embedded gateway starts fresh-locked. The standalone server's already-unlocked vault is invisible to launch. The same is true the other way around: a CLI command that needs the vault must independently hit the unlock surface.

[ADR-0011](/adr/0011-local-credential-vault) ratifies the vault as a security primitive whose KEK lives in memory for the lifetime of "the runtime process," but deliberately leaves the runtime topology abstract — what the runtime process *is* and how its lifecycle maps to user actions. The current implementation has fanned that single runtime process out into many (one standalone server, one per launch session), each with its own independent vault state. This ADR specifies the runtime topology that [ADR-0011](/adr/0011-local-credential-vault) left abstract: the runtime process is the user-scoped daemon defined here, and the daemon's lifecycle bounds the KEK's lifetime described in [ADR-0011](/adr/0011-local-credential-vault).

The constraint we are designing against:

> **The user should never have to think about server lifecycle, ports, or which process holds the unlocked vault.** They run `aileron <command>` or `aileron launch <agent>` and the system Just Works.

## Decision

### A single user-scoped long-running daemon owns the runtime

Aileron runs as **one** process per user, on the user's machine: the **local daemon**. CLI commands and `aileron launch` are thin clients that connect to it.

- The daemon is started **automatically** the first time any client (CLI or launch) needs it; no `aileron serve` step.
- The daemon **stays running** until the user explicitly stops it (`aileron stop`) or until the system reboots. There is no idle timeout.
- All in-memory state — unlocked vault KEK, active sessions, comms listeners, action approval queue — lives in the daemon. Clients hold none of it.

This collapses the previous three-process model (standalone server + per-session embedded gateways) into one. The vault is unlocked once. Sessions persist across launches. There is one URL to know about, and the user never needs to.

### Transport: TCP on 127.0.0.1, ephemeral port, advertised via discovery file

The daemon binds **`127.0.0.1` at an OS-assigned ephemeral port**. After binding, it writes the live address to **`~/.aileron/daemon.url`** (file mode `0600`):

```
http://127.0.0.1:54321
```

Clients (CLI and launch) read `daemon.url` to find the daemon. `AILERON_API_URL` overrides remain available as a developer/test escape hatch but are not the primary mechanism.

On **Linux with Docker**, the daemon additionally binds the Docker bridge-gateway IP (the host end of `docker0`, what `host-gateway` resolves to) on the *same* ephemeral port, serving the same token-protected handler. Without it a sandbox container cannot reach the daemon: the launcher rewrites the loopback URL to `host.docker.internal`, which `--add-host host.docker.internal:host-gateway` resolves to the bridge-gateway IP rather than loopback, so a loopback-only daemon refuses the container's connection. The advertised `daemon.url` stays the `127.0.0.1` URL (the launcher does the loopback→`host.docker.internal` rewrite per runtime); the bridge listener is gated by the same daemon token, never bound on macOS/Windows (Docker Desktop forwards `host.docker.internal` to loopback) or on non-Docker Linux, and a failed gateway derivation is a hard startup error rather than a silent wider bind.

Why TCP and not a unix socket:

- **The browser cannot speak unix sockets.** The webapp — including the vault unlock modal ratified in #429 — is HTTP. The daemon must expose HTTP on a port a browser can navigate to.
- **The Anthropic SDK (and most third-party HTTP SDKs Aileron proxies) cannot speak unix sockets.** `aileron launch claude` sets `ANTHROPIC_BASE_URL` for the child process; that URL must be HTTP-over-TCP because the SDK that consumes it has no socket support.

Given those two constraints, the daemon must already bind a TCP listener. Adding a unix socket as a *second* transport would only buy multi-user isolation on a shared box (which is not in our threat model — Aileron is single-user-per-machine per [ADR-0011](/adr/0011-local-credential-vault)) at the cost of two transports, two auth surfaces, and two bug surfaces. We pay only for the transport we use.

Why ephemeral port and not the historical `8721`:

- **No port conflicts.** Multiple users on a shared machine each get their own daemon, each binds an ephemeral port, no contention.
- **No fixed-port mental model.** Users never see, type, or troubleshoot a port number. The discovery file abstracts it.
- **Mirrors precedent.** Jupyter Server's `jpserver-{pid}.json`, Bazel's `$OUTPUT_BASE/server/` lock file, and Tor's `ControlPortWriteToFile` all use the "let the OS pick the port, write it to a file the client reads" pattern — for the same reason: HTTP-style daemons that can't preempt a fixed port.

The historical port `8721` is preserved only for the **cloud-shaped deployment** (`aileron serve --bind 0.0.0.0:8721` inside a Docker Compose), where stable ingress matters. The local daemon does not take a `--bind` flag.

### Lifecycle: auto-spawn on demand, end on `aileron stop` or reboot

The daemon's lifecycle is bracketed by two events: first need and explicit stop.

**Auto-spawn.** When a client (CLI or launch) needs to connect:

1. Read `~/.aileron/daemon.url`. If present, attempt to connect.
2. If the file is missing or the connection is refused (stale URL), the client `fork-exec`s a new daemon, waits for it to bind and write `daemon.url`, then connects.
3. A `flock(2)` on `~/.aileron/daemon.lock` ensures only one daemon spawns even when multiple clients race on first run.

The daemon writes its PID to **`~/.aileron/daemon.pid`** at startup so `aileron stop` can find it.

**Explicit stop.** `aileron stop` reads the PID file and sends `SIGTERM`. The daemon flushes session state, unlinks `daemon.url` and `daemon.pid`, and exits. The unlocked KEK is dropped from memory.

**Reboot.** Same effect as stop: state on disk persists, in-memory state (KEK, active sessions in their `running` state) is gone. Next client invocation auto-spawns a fresh daemon. Any session that was `running` at reboot time is reaped on the new daemon's startup (see "Session persistence" below).

A future `aileron daemon install` may wire the daemon into `launchd` / `systemd` for true "always running across reboot." That is deferred; v1 ships with the on-demand model only.

### Vault unlock lives in the daemon

The daemon starts **vault-locked**. It exposes the existing endpoints:

- `GET /v1/vault/status` — `{"locked": true}` or `{"locked": false}`.
- `POST /v1/vault/unlock` — accepts the passphrase, derives the KEK ([ADR-0011](/adr/0011-local-credential-vault)), unlocks the in-memory vault.

The daemon never owns a UI. Whichever **client** triggers the first vault-needing request is responsible for prompting:

| Client | Vault-prompt surface |
|---|---|
| `aileron launch <agent>` | Webapp modal at the daemon URL, as ratified in #429. |
| `aileron binding list` (and other CLI commands) | Stderr passphrase prompt in the CLI process (it owns the TTY). The CLI POSTs the passphrase to `/v1/vault/unlock` and continues. |
| Non-TTY / CI | `--passphrase-file <path>` flag or `AILERON_VAULT_PASSPHRASE` env, forwarded by whichever client hits the daemon first. |

The rule is **whoever owns the TTY (or the browser tab) at the moment of `423 Locked` does the prompt**. The daemon is just an unlocked-or-not boolean.

Once any client unlocks, every subsequent client — every CLI invocation, every concurrent launch session — sees an unlocked vault until the daemon stops. **One unlock per daemon lifetime.** This is the user-visible win of this ADR.

### Sessions are persistent and live in the daemon

Today, a "session" exists only inside a running `aileron launch` process; on exit, all session state is gone. Under the daemon model, sessions are first-class persistent records owned by the daemon.

A `Session` is identified by a **ULID** ([Universally Unique Lexicographically Sortable Identifier](https://github.com/ulid/spec)) replacing the prior 8-byte hex format. ULID gives time-sortable IDs without depending on a separate timestamp index — useful for "list newest" queries and natural for the JSONL replay model.

The shape of a session record:

```go
type Session struct {
    ID         string     // ULID
    StartedAt  time.Time
    EndedAt    *time.Time // nil = running
    Agent      string     // "claude", "pi", etc.
    WorkingDir string
    ExitCode   *int       // nil + EndedAt non-nil = orphaned (see below)
}
```

Three states fall out of `EndedAt` × `ExitCode`:

- **Running** — `EndedAt == nil`. Daemon believes the session is live.
- **Ended cleanly** — `EndedAt != nil && ExitCode != nil`. Agent process exited; daemon recorded the exit code.
- **Orphaned** — `EndedAt != nil && ExitCode == nil`. Daemon was restarted while the session was running; on `Open()`, the persistence layer reaps any `EndedAt == nil` records by stamping `EndedAt = time.Now()` and leaving `ExitCode` unset. The webapp renders this as "ended (status unknown — daemon restart)." The honest signal is the *absence* of an exit code; we don't fabricate one.

Persistence is mediated by an **interface** (`internal/sessions.Store`) so the storage backend is replaceable:

```go
type Store interface {
    Put(ctx context.Context, s Session) error            // upsert
    Get(ctx context.Context, id string) (Session, error) // ErrNotFound if absent
    List(ctx context.Context, f Filter) ([]Session, error)
    Close() error
}
```

The v1 implementation is **JSONL** at `~/.aileron/sessions.jsonl` — one full session record per line, append-on-`Put`, last-write-wins per ID, in-memory map maintained at runtime, file rewritten from the map on `Close()` for compaction. SQLite (or any other backend) is a future implementation that ships behind the same interface without callers changing.

A shared test suite (`internal/sessions/sessionstest.RunSuite`) exercises the contract against any implementation, per the "test the contract, not the implementation" rule in `CLAUDE.md`.

### Audit log moves to user-scope, daily-rotated

The current audit log is per-project: `<cwd>/.aileron/audit.jsonl`, with an `aileron.yaml` `Settings.AuditLog` override. That model made sense when launch was the only audit producer and you could think of `.aileron/audit.jsonl` as "this project's tape," like `.git/`.

Under the daemon model, it doesn't fit. The daemon is user-scoped; sessions are user-scoped; the vault is user-scoped. Anchoring audit to the working directory means "what did agents do for me today?" requires scanning every `.aileron/` everywhere, projects that move orphan their audits, and the webapp can't render a coherent global timeline.

The audit log moves to **`~/.aileron/audit/audit-YYYY-MM-DD.jsonl`**, daily-rotated, owned by the daemon. Records carry `session_id` as the join key; queries filter by session ID across files (or just today's, for a live session).

Per-project audit visibility — `cd ~/proj && cat .aileron/audit.jsonl` — is removed. It can be reintroduced later as a *projection* over the user-level log filtered by `working_dir`. Storage is the user's; the view is whatever we want it to be.

The `aileron.yaml` `Settings.AuditLog` override is removed. Per the no-backwards-compat-before-release policy, this is fine.

### `aileron launch` becomes a thin client; the embedded gateway goes away

Today, `aileron launch claude` starts a per-session embedded gateway, points `ANTHROPIC_BASE_URL` at that ephemeral gateway URL, registers `aileron-mcp` against it, and tears the whole thing down when the agent exits.

Under the daemon model:

- Launch reads `~/.aileron/daemon.url`, auto-spawning the daemon if needed.
- It POSTs `/v1/sessions` to register a new session (gets a ULID, stored persistently).
- It sets `ANTHROPIC_BASE_URL` and `AILERON_URL` to the **daemon's** URL — same URL across every launch session, stable across daemon lifetime.
- The daemon multiplexes per-session state (audit entries, comms listeners, approval queue) by `session_id`.
- On agent exit, launch POSTs `/v1/sessions/{id}/end` with the exit code; the session record updates from `running` to `ended cleanly`.

The per-session embedded gateway code — `internal/launch/gateway.go`, `StartGateway`, `gatewayConfig` — is removed. The session-scoped sockets (`/tmp/ai-{sessionID}.sock`, `/tmp/ai-comms-{sessionID}.sock`) are reconsidered as part of the move; the comms server likely becomes a daemon-owned dispatcher keyed by session ID.

### Cloud Aileron is a separate shape, not addressed here

The `aileron serve --bind` family of flags, including `--bind-all` (#450), is reframed as the **cloud deployment** mode: TCP exposure, multi-user-aware, deployed via Docker Compose, behind real auth/TLS. That is a separate product surface with its own ADRs to come.

The local daemon described here does **not** take `--bind` and is **not** intended for remote access. Users who want remote access run cloud-shaped Aileron, even if "the cloud" is a container on their own LAN.

## Implementation status

This ADR is proposed; no implementation exists yet. Affected packages and files:

**New:**
- `internal/daemon/` — the daemon binary's main loop, lifecycle, discovery-file management, PID file, signal handling.
- `internal/daemon/discovery/` — read/write `daemon.url`, `daemon.pid`, `daemon.lock`.
- `internal/daemon/spawn/` — client-side auto-spawn helper used by CLI and launch.
- `internal/sessions/` — `Session`, `Filter`, `Store` interface, `ErrNotFound`, ULID generator.
- `internal/sessions/jsonl/` — JSONL implementation.
- `internal/sessions/sessionstest/` — shared test contract suite.
- `cmd/aileron/daemon.go` (or similar) — `aileron daemon start` (rare; auto-spawn covers normal use), `aileron stop`.

**Removed / repurposed:**
- `internal/launch/gateway.go`, `StartGateway`, `gatewayConfig` — gone; launch is a thin client.
- `internal/launch.resolveAuditLog` — replaced by daemon-level audit path resolution.
- `aileron.yaml` `Settings.AuditLog` field — removed.
- `internal/launch.generateSessionID` — replaced by ULID generation in `internal/sessions`.

**Changed:**
- `internal/launch.Launch` — no longer constructs a gateway; instead resolves the daemon URL, registers a session, configures the agent's environment with the daemon's URL, runs the agent, ends the session.
- `internal/cli/*` — every subcommand that talks to the runtime now resolves the daemon URL through the discovery file (with `AILERON_API_URL` as override) and auto-spawns if absent.

The Stage 1 cryptographic primitives, vault format, and unlock endpoint from [ADR-0011](/adr/0011-local-credential-vault) are unchanged. The vault is *where it always was* (`~/.aileron/secrets.json`); only the *which process holds it unlocked* answer changes.

## Alternatives Considered

### Unix socket transport (rejected)

The daemon listens on `~/.aileron/aileron.sock`; CLI and launch connect to that. Multi-user safe via filesystem permissions; no port-conflict surface at all.

Rejected because the daemon must already expose HTTP-over-TCP for two non-negotiable consumers: the browser (for the webapp's vault unlock modal and for any future webapp surfaces) and the Anthropic SDK (which the launched agent's `ANTHROPIC_BASE_URL` points at). Adding a unix socket as a *second* transport would buy only multi-user isolation on a shared machine — outside the threat model of [ADR-0011](/adr/0011-local-credential-vault) — at the cost of two transports to maintain, two auth surfaces to harden, and two failure modes to debug.

### Auto-start inside the CLI process, no daemon (rejected)

Each CLI invocation spawns a runtime in-process, reads/writes the vault, exits. No background process, no discovery file, no IPC.

Rejected because it does not solve the vault-unlock-state-sharing problem that motivated this ADR. Each invocation would re-prompt for the passphrase, defeating the user-visible win. It also forecloses the "long-running launch session" use case where the agent stays connected to a stable URL for the duration of a development session.

### Fixed port `8721` for the local daemon (rejected)

Keep the historical port for the local daemon too; document the conflict resolution as "kill the other one."

Rejected because port conflicts surface to users at exactly the wrong moment — first-run, when they have the least context to debug. The discovery file approach hides ports entirely from the user-facing model. The historical port is retained only for the cloud-shaped deployment, where ingress stability is a feature rather than friction.

### Per-session embedded gateways stay; daemon coordinates only sessions (rejected)

Keep the per-session gateway for launch, add a daemon for the standalone CLI use case, have the daemon coordinate session metadata across them.

Rejected because it preserves the multi-process vault-state problem — the per-session gateway still has its own locked vault, still requires its own unlock. The split between "the daemon" and "the per-session gateway" is exactly what we are trying to remove.

### Recovery codes for orphaned-session gap (rejected for v1)

When the daemon restarts mid-session, fabricate an `ExitCode = -1` "killed by daemon restart" rather than leaving `ExitCode == nil`.

Rejected because it lies. We do not know whether the session ended in success, failure, or never-actually-ended-at-all (the agent process may still be running, parented to init, with us unaware). The honest signal is the *absence* of an exit code, rendered to the user as "status unknown — daemon restart." Adding a fake exit code introduces a value the user cannot distinguish from a real exit code.

## Consequences

### For users

- **No `aileron serve` step.** First run is `aileron <command>` or `aileron launch <agent>`; the daemon auto-spawns.
- **No port conflicts.** Ports are not in the user-facing model. The daemon URL is in `~/.aileron/daemon.url` for anyone who needs to inspect, but day-to-day use never references it.
- **One vault unlock per daemon lifetime.** Whether the user starts with `aileron launch claude` or `aileron binding list`, they unlock once and every subsequent command (and every concurrent launch session) sees an unlocked vault.
- **Sessions persist.** `aileron sessions list` shows past launches, their working directories, exit codes, and audit-log links — across daemon restarts.
- **Per-project audit log goes away.** Users who relied on `cd ~/proj && cat .aileron/audit.jsonl` get no replacement in v1; the projection-by-`working_dir` is post-MVP. Pre-release, that's an acceptable tradeoff per the no-backwards-compat policy.

### For the launch path

- The per-session embedded gateway is removed. Launch becomes a small client: register a session, run the agent with environment pointing at the daemon, end the session.
- The session-scoped sockets (`/tmp/ai-{sessionID}.sock`, `/tmp/ai-comms-{sessionID}.sock`) are revisited; the comms server likely becomes a daemon-owned dispatcher with per-session routing.
- Startup banner simplifies: one URL, stable across the daemon's life.

### For CLI commands

- Every command that touches runtime state now goes through the daemon. Commands that are pure local-file operations (e.g., reading `aileron.yaml`) may bypass it.
- `AILERON_API_URL` is reframed as a developer/test override, not a primary connection mechanism. Most users never set it.

### For the vault

- [ADR-0011](/adr/0011-local-credential-vault)'s "KEK lives in memory for the lifetime of the runtime process" model maps cleanly to the daemon: one process, one KEK lifetime, ends at `aileron stop` or reboot.
- The webapp unlock modal (#429) becomes the canonical unlock surface for the launch path, with stderr prompts retained for CLI-initiated unlocks.

### For audit and forensics

- Audit log centralizes at `~/.aileron/audit/audit-YYYY-MM-DD.jsonl`. Daily rotation matches the daemon log convention.
- `session_id` is the join key between session records and audit entries; cross-session queries are straightforward.
- Per-project visibility is lost; can return as a projection.

### For tests

- The session SPI is exercised via `sessionstest.RunSuite(t, factory)`. JSONL impl and any future impl run the same suite.
- Daemon lifecycle (spawn, discover, connect, stop) needs end-to-end tests; the `flock`-based singleton invariant is a key correctness property.

### For cloud Aileron

- The cloud-shaped serve (`aileron serve --bind`) is unaffected by this ADR. It continues to exist as the deployment mode for hosted / Docker Compose Aileron, with its own auth, TLS, and multi-user model deferred to a separate ADR.
- The local daemon and the cloud serve share the underlying HTTP API surface; they differ in transport binding, lifecycle management, and authentication.

### Open implementation questions (deferred)

- *Should the daemon ship with `launchd` / `systemd` integration (`aileron daemon install`) for true cross-reboot persistence?* — Post-MVP. The on-demand auto-spawn model covers the common case; cross-reboot persistence is a polish that real usage will tell us we need.
- *Should the comms server (Slack/Discord listeners) move into the daemon entirely, or stay per-session as a daemon-multiplexed dispatcher?* — Implementation detail; resolved during the implementation PRs.
- *What is the migration path for users with existing `<cwd>/.aileron/audit.jsonl` files?* — Pre-release, no migration: the file is ignored. Documentation calls out the move.
- *Does `aileron daemon status` warrant a dedicated subcommand, or is it implicit in the daemon URL file's existence?* — Deferred until users ask for it.

## Examples

### First run (no daemon yet)

```
$ aileron binding list
✈️  Aileron daemon starting at http://127.0.0.1:54321 ...
Vault is encrypted. Enter passphrase to unlock:
> ********

(empty — no bindings configured)
```

The CLI auto-spawned the daemon, prompted on stderr (it owns the TTY), POSTed the passphrase, then ran the original command.

### Subsequent run (daemon already running, vault already unlocked)

```
$ aileron binding list
oauth2/aileron-connector-google/personal  oauth2  google  ...

$ aileron launch claude
✈️  Aileron — webapp http://127.0.0.1:54321 — session 01HK6QRT... — log ~/.aileron/logs/...
```

No prompts. Vault is unlocked from the prior CLI invocation; same daemon URL; webapp is reachable for any approval surfaces that fire.

### First run via launch

```
$ aileron launch claude
✈️  Aileron daemon starting at http://127.0.0.1:54321 ...
✈️  Aileron — webapp http://127.0.0.1:54321 — session 01HK6QRT... — log ~/.aileron/logs/...
✈️  Vault locked — open http://127.0.0.1:54321 and enter your passphrase to unlock.
```

The daemon auto-spawned. Launch printed the banner pointing at the webapp; the user opens the URL, types the passphrase, the modal POSTs `/v1/vault/unlock`, the daemon's vault is now unlocked for every subsequent client.

### Stopping the daemon

```
$ aileron stop
✈️  Aileron daemon stopped. Vault locked.
```

KEK gone from memory; `daemon.url` and `daemon.pid` removed; sessions table flushed and compacted on disk. Next `aileron <anything>` auto-spawns a fresh daemon.

### Listing sessions across restarts

```
$ aileron sessions list
ID                          STARTED               AGENT   STATUS                EXIT
01HK6QRT...                 2026-05-05 14:22:03   claude  running               -
01HK5XPM...                 2026-05-05 11:08:41   claude  ended cleanly         0
01HK5JN3...                 2026-05-05 09:14:17   claude  ended (status unknown — daemon restart)  -
```

The `status unknown` row is honest: the daemon was restarted while that session was running; we do not pretend to know its exit code.
