---
title: "Adding an Agent"
description: "How to wire a new AI coding agent into `aileron launch`."
order: 6
---

`aileron launch <agent>` runs a supported AI coding agent under the Aileron daemon. With `--sandbox=off`, host launch wires the agent to the daemon and current MCP/gateway path. With `--sandbox=auto|docker|podman`, sandbox launch prepares a container image, validates that the agent command exists inside it, and runs the agent in the container with Aileron's session env and generated discovery/action shims. Adding a new agent means writing one Go file under `internal/launch/agents/`, registering it in `cmd/aileron/main.go`, documenting whether its command is available in sandbox images, and shipping tests.

## When `aileron launch` is the right answer

An agent is integrable when it:

- Supports the [Model Context Protocol](https://modelcontextprotocol.io/) so Aileron's tool catalogue is reachable as `mcp__aileron__*`.
- Exposes an environment-controllable LLM base URL (`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, etc.) so gateway routing is available. This is desirable but not required. Agents that resolve the LLM provider from a settings file (Goose, OpenCode, Pi) integrate fine without it; they just don't get gateway routing under launch today.
- Has a headless mode the user can drive from the terminal.

An agent is **not** a candidate for `aileron launch` when it has no MCP support, no CLI entry point, or runs only inside an IDE the daemon can't talk to. Those agents may still belong elsewhere in the Aileron stack. They just don't fit this SPI.

[ADR-0015](https://docs.withaileron.ai/adr/0015-launch-audit-scope/) bounds what `aileron launch` is responsible for. The launcher resolves the daemon, routes LLM traffic, and registers `aileron-mcp`. It does **not** replace `$SHELL`, install wrapper scripts, write policy files, or audit shell commands the agent runs locally. The agent's own approval and sandbox layer keeps that boundary.

Sandbox launch adds a separate runtime path defined by [ADR-0017](/adr/0017-sandbox-composition/) and [ADR-0018](/adr/0018-v4-single-binary-runtime/). It does not use `aileron-mcp` as the in-container runtime model. The first sandbox cut injects `AILERON_API_URL`, session metadata, `/etc/aileron/tools.txt`, and generated connector shims. Container-only shell mediation was prototyped under [#801](https://github.com/ALRubinger/aileron/issues/801) and withdrawn in [#952](https://github.com/ALRubinger/aileron/issues/952); see [ADR-0021](/adr/0021-v4-shell-layer-mediation/) (Withdrawn).

## The `Agent` interface

The SPI is defined in [`internal/launch/agent.go`](https://github.com/ALRubinger/aileron/blob/main/internal/launch/agent.go):

```go
type Agent interface {
    Name() string
    BinaryNames() []string
    Args() []string
    Env() map[string]string
    LLMEndpointEnv() string
    ConfigureMCP(mcpBin string, mcpEnv map[string]string, dir string) ([]string, error)
}
```

Six methods. Three are usually one-liners.

### `Name() string`

The agent identifier used on the CLI (`aileron launch claude`). Lowercase, no whitespace. Matches the convention the agent itself uses on PATH.

### `BinaryNames() []string`

Candidate binary names to search on `$PATH`, in preference order. Most agents have one (`["claude"]`). Use multiple entries when the agent ships under different names across platforms or distributions (`["codex", "openai-codex"]`).

### `Args() []string`

Extra CLI arguments to prepend before user-supplied arguments. Return `nil` when none are needed. Use this when the agent's CLI needs a flag to suppress its own approval prompts now that Aileron mediates the action surface. Examples in the tree:

- `Claude.Args()` passes `--allowedTools "Bash(*) mcp__aileron"` so Claude Code does not double-prompt for Bash or for tools the daemon already governs.
- `Pi.Args()` passes `--tools bash,read,edit,...` for the same reason.
- `Codex.Args()`, `Goose.Args()`, `OpenCode.Args()` all return `nil` because those agents drive approval from their own config file.

### `Env() map[string]string`

Extra environment variables for the agent process. Return `nil` when none are needed. Use this when the agent reads a mode flag from the environment. `Goose.Env()` sets `GOOSE_MODE=auto` for autonomous runs.

### `LLMEndpointEnv() string`

The environment variable the agent reads to override its LLM base URL. Return `""` when the agent has no such env var; gateway routing is then unavailable under launch.

| Agent | Returns | Why |
|---|---|---|
| Claude | `"ANTHROPIC_BASE_URL"` | Claude Code reads it. |
| Codex | `"OPENAI_BASE_URL"` | Codex CLI reads it on the API-key auth path. ChatGPT-login sessions ignore it. |
| Pi | `""` | Pi resolves the endpoint from `.pi/settings.json`. |
| Goose | `""` | Goose configures provider base URLs per provider inside `config.yaml`. |
| OpenCode | `""` | OpenCode configures provider base URLs per provider inside `opencode.json`. |

### `ConfigureMCP(mcpBin, mcpEnv, dir) ([]string, error)`

The one method every agent does interesting work in. It arranges for the agent to discover `aileron-mcp`. There are two shapes.

## `ConfigureMCP`: the two shapes

### Shape 1: CLI-flag agents (Claude, Pi)

The agent CLI accepts `--mcp-config <json>`. `ConfigureMCP` returns the flag pair and writes nothing to disk.

```go
func (c Claude) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string) ([]string, error) {
    envJSON, err := json.Marshal(mcpEnv)
    if err != nil {
        return nil, fmt.Errorf("marshaling MCP env: %w", err)
    }
    mcpConfig := fmt.Sprintf(
        `{"mcpServers":{%q:{"command":%q,"env":%s}}}`,
        launch.MCPServerName, mcpBin, string(envJSON),
    )
    return []string{"--mcp-config", mcpConfig}, nil
}
```

The JSON shape Claude Code accepts:

```json
{
  "mcpServers": {
    "aileron": {
      "command": "/usr/local/bin/aileron-mcp",
      "env": {
        "AILERON_URL": "http://127.0.0.1:7000",
        "AILERON_SESSION_ID": "sess-abc"
      }
    }
  }
}
```

Pi happens to honour the same shape, so [`agents/pi.go`](https://github.com/ALRubinger/aileron/blob/main/internal/launch/agents/pi.go) is nearly identical to Claude's implementation.

### Shape 2: Config-file agents (Codex, Goose, OpenCode)

The agent reads MCP servers from a config file rather than from the CLI. `ConfigureMCP` writes (or merges) the file and returns `nil` args. Existing user keys in the file must round-trip unchanged.

**Codex** writes `~/.codex/config.toml` under `[mcp_servers.aileron]`. The merge is line-oriented so the rest of the file (other sections, comments, user keys) survives intact. See [`mergeCodexMCPBlock`](https://github.com/ALRubinger/aileron/blob/main/internal/launch/agents/codex.go) for the merge algorithm.

```toml
[mcp_servers.aileron]
command = "/usr/local/bin/aileron-mcp"

[mcp_servers.aileron.env]
AILERON_URL = "http://127.0.0.1:7000"
AILERON_SESSION_ID = "sess-codex"
```

**Goose** writes `~/.config/goose/config.yaml` under `extensions.aileron`. The YAML round-trip is full-document: read, set the one key, write. Other extensions and top-level keys are preserved by the round-trip itself. Note that Goose uses XDG on macOS too, not `~/Library/Application Support`.

```yaml
extensions:
  aileron:
    type: stdio
    enabled: true
    name: aileron
    cmd: /usr/local/bin/aileron-mcp
    envs:
      AILERON_URL: http://127.0.0.1:7000
      AILERON_SESSION_ID: sess-goose
```

**OpenCode** writes project-local `opencode.json` under `mcp.aileron`. The `dir` argument to `ConfigureMCP` is the launch working directory; OpenCode prefers project-local config so launching from a different project gets a different file. The merge is a JSON read-modify-write that preserves every other key.

```json
{
  "mcp": {
    "aileron": {
      "type": "local",
      "command": ["/usr/local/bin/aileron-mcp"],
      "enabled": true,
      "environment": {
        "AILERON_URL": "http://127.0.0.1:7000",
        "AILERON_SESSION_ID": "sess-oc"
      }
    }
  }
}
```

The three implementations differ in syntax but share the rule: **read whatever the user has, set only `mcp.aileron` (or the equivalent), write it back**. Never blow away keys you didn't touch.

## Gateway routing

When `LLMEndpointEnv()` returns a non-empty string, [`launcher.go`](https://github.com/ALRubinger/aileron/blob/main/internal/launch/launcher.go) sets that variable to the daemon's gateway URL in the agent's environment. The agent's LLM calls then flow through Aileron's gateway, where credential redaction, audit, and policy live.

Returning `""` is fine when the agent doesn't honour a single base-URL env var. Goose, OpenCode, and Pi all fall into this bucket. Gateway routing for those agents would require `ConfigureMCP` to also rewrite the per-provider endpoint inside the agent's config file. That work is deferred per agent.

Don't fake an env var that the agent doesn't actually read. Returning `""` is the honest signal that launch can't route this agent's LLM traffic today.

## Tests

Mirror the pattern in [`internal/launch/agents/{claude,pi,codex,goose,opencode}_test.go`](https://github.com/ALRubinger/aileron/tree/main/internal/launch/agents). Every new agent needs:

### Identity test

Asserts `Name()`, `BinaryNames()`, `LLMEndpointEnv()`, and the `Args()`/`Env()` defaults. Catches accidental renames and protects the small surface every caller depends on.

```go
func TestMyAgent_Identity(t *testing.T) {
    a := agents.MyAgent{}
    if a.Name() != "myagent" {
        t.Errorf("Name() = %q, want %q", a.Name(), "myagent")
    }
    if got := a.BinaryNames(); len(got) != 1 || got[0] != "myagent" {
        t.Errorf("BinaryNames() = %v, want [\"myagent\"]", got)
    }
    if a.LLMEndpointEnv() != "MYAGENT_BASE_URL" {
        t.Errorf("LLMEndpointEnv() = %q, want MYAGENT_BASE_URL", a.LLMEndpointEnv())
    }
}
```

### `ConfigureMCP` happy path

For CLI-flag agents: assert the flag pair, then `json.Unmarshal` the value and assert the parsed shape. Don't string-match the JSON; the encoder's key order isn't stable.

For config-file agents: drive `HOME` to a `t.TempDir()`, call `ConfigureMCP`, read the file back, and assert the file contents. Use the agent's native parser for the round-trip (TOML, YAML, JSON) rather than substring matching where you can.

### `ConfigureMCP` preserves other config

For config-file agents only. Seed a config file with unrelated keys (and a stale `aileron` entry), call `ConfigureMCP`, then assert the unrelated keys survived and the stale `aileron` entry was overwritten. This is the test that catches a careless rewrite that nukes user state.

See [`TestCodex_ConfigureMCP_PreservesOtherSections`](https://github.com/ALRubinger/aileron/blob/main/internal/launch/agents/codex_test.go) and [`TestGoose_ConfigureMCP_PreservesOtherExtensions`](https://github.com/ALRubinger/aileron/blob/main/internal/launch/agents/goose_test.go) for worked examples.

## Wiring

One line in `cmd/aileron/main.go` alongside the other agents:

```go
registry.Register(agents.MyAgent{})
```

That's it. The CLI's `aileron launch <name>` lookup is a `Registry.Get(name)`, so the agent is reachable as soon as it's registered.

## What is explicitly *not* the agent's job

Per [ADR-0015](https://docs.withaileron.ai/adr/0015-launch-audit-scope/), the agent does not:

- Replace `$SHELL`.
- Install or invoke a wrapper script around `bash` / `zsh` / `sh`.
- Write a per-command approval policy file for the agent.
- Audit shell commands the agent runs locally.

The host agent's own approval and sandbox layer (Claude Code's allowedTools, Codex's `approval_policy` and sandbox mode, Goose's permission profile, OpenCode's `permission.bash`, Pi's `--tools` allowlist) keeps the local-exec boundary. Aileron's audit boundary is the actions the agent calls through `aileron-mcp` and the LLM traffic it routes through the gateway. Nothing more.

For sandbox launch, the agent command must exist in the selected image. If `BinaryNames()` returns `["claude"]`, then `claude` must be on `PATH` inside the Tier 0/Tier 1/Tier 2 image before launch validation passes. The support matrix and image recipes are tracked in [#894](https://github.com/ALRubinger/aileron/issues/894).

If a future contribution proposes reintroducing host shell interception under a new name, point them here first. Container-only shell mediation was prototyped under #801 and withdrawn in #952; any return is gated on a fresh ADR.
