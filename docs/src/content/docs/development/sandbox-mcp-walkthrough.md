---
title: "Sandbox MCP — Manual Verification Walkthrough"
description: "Run an Aileron action end-to-end inside a v4 sandbox container, via the agent's MCP transport, with a real connector and HITL approval."
order: 9
---

This walkthrough exercises the v4 sandbox MCP wiring (ADR-0024) with a real agent and a real action. Use it to confirm a development build of Aileron is wired correctly, or to repro a sandbox-MCP issue with a known-good harness.

The integration test at `internal/app/sandbox_mcp_test.go` (build tag `integration_sandbox`) covers the same flow under automation with a stub upstream. This walkthrough is the human-facing companion when you want to watch each step against a real connector.

## What you need

- Docker installed and running.
- A development build of the Aileron CLI and `aileron-mcp` siblings, both on `PATH`. `task build:cli && task build:mcp` produces them under `./build/`.
- `jq` available on `PATH` (used by the audit-verification command below).
- A Google OAuth client configured via `aileron binding setup gmail` (or any installed action you want to test).
- The `aileron-connector-google` connector installed (`aileron connector install github://ALRubinger/aileron-connector-google`).
- The `draft-email` action installed (`aileron action install github://ALRubinger/aileron-connector-google/actions/draft-email`).

## What to validate (agents and runtimes)

The per-agent bar is **"the agent sees `aileron` and its `draft_email` tool"**. The only thing that varies per agent is how `aileron-mcp` is registered (see ADR-0024). Everything after registration is identical across agents.

| Agent | Identifier | MCP registration mechanism |
|---|---|---|
| Claude | `claude` | `--mcp-config` JSON |
| Pi | `pi` | `--mcp-config` JSON |
| Goose | `goose` | `--with-extension` |
| OpenCode | `opencode` | `opencode.json` in the workspace |
| Codex | `codex` | bind-mounted `config.toml` |

Supported runtimes are Docker on macOS, Linux, and Windows. Docker Desktop on Windows runs Linux containers through the WSL2 backend, which is the same `sandbox-base` image used on macOS and Linux. The launcher adds `--add-host=host.docker.internal:host-gateway` on Linux only. macOS and Windows Docker Desktop resolve `host.docker.internal` automatically.

### Linux is automated

The Linux column is covered in CI, so you do not run it by hand:

- `TestSandboxMCPRegistration_Matrix` (`internal/launch/agents`) asserts, per agent, that the `ModeSandbox` registration references the `aileron-mcp` binary and carries every daemon env var. It runs in the standard unit suite.
- `TestSandboxMCP` (`internal/app`, build tag `integration_sandbox`) runs the `aileron-mcp` to daemon round-trip over both a host subprocess and a real Docker container, asserts `tools/list` exposes `draft_email`, and checks the `approval.requested → approval.approved → execution.started → execution.succeeded` audit chain. It runs in the CI integration job on Linux.

### macOS and Windows are a light manual smoke

GitHub-hosted macOS and Windows runners cannot run Linux Docker containers, so those two columns are not automatable in CI. The container itself is the same Linux image on all three operating systems, and the host-side launcher code is unit-tested on macOS and Windows already, so the residual risk is only host Docker networking (`host.docker.internal` resolution). Cover it with a **light manual smoke** per agent rather than a full Gmail round-trip: launch the agent, confirm it lists `aileron` with `draft_email`, then exit. Record each in issue [#962](https://github.com/ALRubinger/aileron/issues/962). A full round-trip (through to a real Gmail draft and approval) on at least one agent per OS is the optional gold-standard check; the steps below walk through it.

## Run

Pick the agent to validate. Every other step is identical.

```bash
AGENT=claude   # one of: claude | pi | goose | opencode | codex
aileron launch --sandbox=docker "$AGENT"
```

The launcher will:

1. Resolve the host-built `aileron-mcp` binary and bind-mount it read-only into the container at `/usr/local/bin/aileron-mcp`. The launcher skips this step when the resolved image already bakes `aileron-mcp` in. It detects a baked image by reading the `ai.aileron.mcp.version` label ([issue #957](https://github.com/ALRubinger/aileron/issues/957)). The published `ghcr.io/alrubinger/aileron-sandbox-base` image bakes the binary so sealed customer-operated runtimes launch without a host `aileron-mcp`. The local Tier 0 base build stays unbaked and keeps using this host-mount.
2. Build the MCP environment (`AILERON_URL` rewritten to `host.docker.internal:<port>`, `AILERON_SESSION_ID`, `AILERON_TOKEN`).
3. Register `aileron-mcp` with Claude Code via `--mcp-config`.
4. Validate the container can `command -v aileron-mcp` AND `aileron-mcp --version` exits 0 (catches arch mismatch).
5. Start the container with `--add-host=host.docker.internal:host-gateway` on Linux Docker.

Once the agent is running, confirm the `aileron` MCP server registered. Claude and Codex list servers with `/mcp`:

```text
> /mcp
```

You should see one MCP server named `aileron` with the Aileron action catalog. Look for `draft_email`. Goose, OpenCode, and Pi surface their tool list differently. For those, ask the agent to list its available tools, or skip straight to the draft request below and confirm it invokes the Aileron tool. The session log (`.aileron/session.log` under the launch directory) records `aileron-mcp`'s discovery call against the daemon, which is the runtime-agnostic proof that registration succeeded.

Then ask:

```text
> Draft an email to alice@example.com saying I'm running late
```

The agent calls `mcp__aileron__draft_email`. The flow:

1. `aileron-mcp` (inside the container) POSTs `/v1/actions/draft-email/run` to the daemon (on the host, via `host.docker.internal`) with `X-Aileron-Session-Id` set to the launch session.
2. The daemon sees the action manifest declares `[approval]` and returns `202 Accepted` with a `review_url`.
3. `aileron-mcp` surfaces the review URL back to the agent, which surfaces it to you.
4. Open the review URL in your browser, or run `aileron approval approve <id>` in another terminal.
5. The daemon executes the action: fetches the OAuth credential from the vault, calls Gmail's draft endpoint, returns the draft id.
6. `aileron-mcp`'s `check_action_status` polling surfaces the result back to the agent.
7. The audit log records the chain `approval.requested → approval.approved → execution.started → execution.succeeded`, all stamped with the launch session id.

Verify the audit chain:

```bash
aileron audit list --session "$(aileron sessions list --json | jq -r '.[0].id')"
```

Assert the four contractual events are present for the session in one shot:

```bash
SESSION=$(aileron sessions list --json | jq -r '.[0].id')
aileron audit list --session "$SESSION" --json \
  | jq -r '[.[].type] | sort | unique' \
  | jq -e 'index("approval.requested") and index("approval.approved")
           and index("execution.started") and index("execution.succeeded")' \
  && echo "PASS: audit chain complete for $SESSION" \
  || echo "FAIL: audit chain incomplete for $SESSION"
```

Verify the draft landed in Gmail's draft folder. That is the upstream contract under test.

## Record the result

The **Linux column is automated** (the tests above), so those cells are green when CI is green; you do not hand-run them. For **macOS and Windows**, the per-agent bar is the light smoke: the agent listed `aileron` with `draft_email`. Record those cells in issue [#962](https://github.com/ALRubinger/aileron/issues/962) (its body, not a comment) with the environment (OS, arch, Docker version, Aileron CLI commit, agent version) and any deviations. If you also run the optional full round-trip, note that the audit assertion above printed `PASS` and the draft landed in Gmail.

| Agent ↓ / Runtime → | macOS Docker (manual smoke) | Linux Docker (CI) | Windows Docker (manual smoke) |
|---|---|---|---|
| Claude | ☐ | automated | ☐ |
| Pi | ☐ | automated | ☐ |
| Goose | ☐ | automated | ☐ |
| OpenCode | ☐ | automated | ☐ |
| Codex | ☐ | automated | ☐ |

Codex is the highest-risk cell on any runtime because it is the only agent whose MCP registration uses a bind-mounted `config.toml` rather than a CLI flag or workspace file. Validate it deliberately and read the Codex troubleshooting note below.

## Verify coexistence with a user MCP server (R3)

Aileron does NOT aggregate or proxy user-installed MCP servers under sandbox launch. Aileron is one MCP server; your own MCP servers connect through your own config and coexist independently.

To verify, add a user MCP server to your global Claude Code config (`~/.config/claude-code/mcp.json` or `~/Library/Application Support/Claude/claude_code_settings.json` on macOS):

```json
{
  "mcpServers": {
    "userthing": {
      "command": "/usr/local/bin/your-mcp-server",
      "env": {}
    }
  }
}
```

Re-launch under sandbox:

```bash
aileron launch --sandbox=docker claude
```

In Claude, `/mcp` should show BOTH `aileron` and `userthing`. Both work independently. The Aileron daemon never sees the `userthing` traffic.

## Troubleshooting

### `mcp__aileron__draft_email` missing from the tool list

The most common cause is a cross-arch host: an `arm64` host bind-mounting into an `amd64` container (or vice versa). The launcher's validate step runs `aileron-mcp --version` for exactly this case and should fail before launch with a clear message. If the validate step passed but tools are missing, look at the session log (`.aileron/session.log` under the launch directory) for the daemon discovery call from `aileron-mcp` — a vault-locked or daemon-restart-during-launch race leaves `aileron-mcp` without action tools.

### Tool-name collision with a user MCP server

Claude Code's `mcp__<server>__<tool>` convention disambiguates by server. A user's `draft_email` from `userthing` appears as `mcp__userthing__draft_email` alongside `mcp__aileron__draft_email`. Both work independently; the agent picks based on intent. There is no conflict.

### Codex: user devcontainer MCP entries masked

The Codex sandbox path bind-mounts a generated `config.toml` into the container at `/home/agent/.codex/config.toml`. Any user-shipped `[mcp_servers.foo]` entry in a devcontainer-baked config is silently masked by the launcher-provided file. Aileron does NOT promise a Codex multi-config-file workaround today — whether Codex reads additional `~/.codex/*.toml` files is unverified. If you want Codex + sandbox with extra MCP servers, pre-merge your entries into a wrapper script that writes a combined config before `aileron launch` invokes Codex.

### Agent crashed mid-approval

The daemon executes regardless of agent presence. If the agent crashes after the `202` response but before the user approves (or after they approve but before `aileron-mcp` polls the result), the action still runs to completion on approve, and the result sits in memory keyed by `approval_id`. The next daemon restart drops in-memory approval state. Either re-launch and re-invoke (idempotency depends on the action), or read the audit log directly. This decoupling is intentional per ADR-0009 (agent is never in the trust path).

### `host.docker.internal` not resolving (Linux)

Linux Docker does not configure `host.docker.internal` automatically. The launcher adds `--add-host=host.docker.internal:host-gateway` on Linux Docker; this requires Docker 20.10+. If your daemon hangs trying to reach the daemon, confirm with:

```bash
docker run --rm --add-host=host.docker.internal:host-gateway alpine getent hosts host.docker.internal
```

macOS and Windows Docker Desktop handle this automatically. Podman's native `host.containers.internal` alias is the deferred re-add path; Podman is planned but not yet supported in v4 ([ADR-0014](/adr/0014-spawn-sandbox-technology/)).

### Baked image version skew

The published `sandbox-base` image ships its own `aileron-mcp` baked in. The launcher detects this through the `ai.aileron.mcp.version` label and skips the host-mount, so a baked image runs the version of `aileron-mcp` that was compiled into it. That version can differ from the host CLI's version. ADR-0024's host-mount lockstep guarantees the two match for the v4 default topology, but a baked image breaks that lockstep by design.

`aileron sandbox check` surfaces the difference. It compares the baked `ai.aileron.mcp.version` label against the host CLI version. A match prints an `mcp: baked aileron-mcp <version> (matches host CLI)` line. A mismatch prints a warning that names both versions and never fails the check. Skew is expected and managed for sealed customer-operated runtimes. Skew in the v4 default topology is worth investigating, since that flow expects the unbaked local image and a host-built `aileron-mcp`.

For the default unbaked flow, the host `aileron-mcp` sibling is still required. Build it with `task build:cli && task build:mcp` and keep both on `PATH`.

## Sources

- [ADR-0008](/adr/0008-intent-matching/) — MCP is the canonical action-exposure surface (extended to sandbox launch).
- [ADR-0009](/adr/0009-user-channel/) — agent is never in the approval trust path.
- [ADR-0018](/adr/0018-v4-single-binary-runtime/) — v4 single-binary model; sandbox MCP revival amended in.
- [ADR-0024](/adr/0024-sandbox-mcp-parity/) — the Path B1 architecture decision this walkthrough exercises.
- [Issue #953](https://github.com/ALRubinger/aileron/issues/953) — the tracking issue for sandbox MCP parity.
- [Issue #957](https://github.com/ALRubinger/aileron/issues/957) — baking `aileron-mcp` into the published `sandbox-base` image.
