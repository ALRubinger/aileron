---
title: "Sandbox MCP — Manual Verification Walkthrough"
description: "Run an Aileron action end-to-end inside a v4 sandbox container, via the agent's MCP transport, with a real connector and HITL approval."
order: 9
---

This walkthrough exercises the v4 sandbox MCP wiring (ADR-0024) with a real agent and a real action. Use it to confirm a development build of Aileron is wired correctly, or to repro a sandbox-MCP issue with a known-good harness.

The integration test at `test/integration/sandbox_mcp_test.go` (build tag `integration_sandbox`) covers the same flow under automation. This walkthrough is the human-facing companion when you want to watch each step.

## What you need

- Docker or Podman installed and running.
- A development build of the Aileron CLI and `aileron-mcp` siblings, both on `PATH`. `task build:cli && task build:mcp` produces them under `./build/`.
- A Google OAuth client configured via `aileron binding setup gmail` (or any installed action you want to test).
- The `aileron-connector-google` connector installed (`aileron connector install github://ALRubinger/aileron-connector-google`).
- The `draft-email` action installed (`aileron action install github://ALRubinger/aileron-connector-google/actions/draft-email`).

## Run

```bash
aileron launch --sandbox=docker claude
```

The launcher will:

1. Resolve the host-built `aileron-mcp` binary and bind-mount it read-only into the container at `/usr/local/bin/aileron-mcp`.
2. Build the MCP environment (`AILERON_URL` rewritten to `host.docker.internal:<port>`, `AILERON_SESSION_ID`, `AILERON_TOKEN`).
3. Register `aileron-mcp` with Claude Code via `--mcp-config`.
4. Validate the container can `command -v aileron-mcp` AND `aileron-mcp --version` exits 0 (catches arch mismatch).
5. Start the container with `--add-host=host.docker.internal:host-gateway` on Linux Docker.

Once Claude is running:

```text
> /mcp
```

You should see one MCP server named `aileron` with the Aileron action catalog. Look for `draft_email`.

Then ask:

```text
> Draft an email to alice@example.com saying I'm running late
```

Claude calls `mcp__aileron__draft_email`. The flow:

1. `aileron-mcp` (inside the container) POSTs `/v1/actions/draft-email/run` to the daemon (on the host, via `host.docker.internal`) with `X-Aileron-Session-Id` set to the launch session.
2. The daemon sees the action manifest declares `[approval]` and returns `202 Accepted` with a `review_url`.
3. `aileron-mcp` surfaces the review URL back to Claude, which surfaces it to you.
4. Open the review URL in your browser, or run `aileron approval approve <id>` in another terminal.
5. The daemon executes the action: fetches the OAuth credential from the vault, calls Gmail's draft endpoint, returns the draft id.
6. `aileron-mcp`'s `check_action_status` polling surfaces the result back to Claude.
7. The audit log records the chain `approval.requested → approval.approved → execution.started → execution.succeeded`, all stamped with the launch session id.

Verify the audit chain:

```bash
aileron audit list --session $(aileron sessions list --json | jq -r '.[0].id')
```

Verify the draft landed in Gmail's draft folder — that's the upstream contract under test.

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

macOS and Windows Docker Desktop handle this automatically. Podman uses `host.containers.internal` natively (no flag needed).

## Sources

- [ADR-0008](/adr/0008-intent-matching/) — MCP is the canonical action-exposure surface (extended to sandbox launch).
- [ADR-0009](/adr/0009-user-channel/) — agent is never in the approval trust path.
- [ADR-0018](/adr/0018-v4-single-binary-runtime/) — v4 single-binary model; sandbox MCP revival amended in.
- [ADR-0024](/adr/0024-sandbox-mcp-parity/) — the Path B1 architecture decision this walkthrough exercises.
- [Issue #953](https://github.com/ALRubinger/aileron/issues/953) — the tracking issue for sandbox MCP parity.
