---
title: "Sandbox Agent Images"
description: "Supported agent commands, image requirements, and recipes for Aileron sandboxes."
order: 7
---

Sandbox launch runs the agent command inside the selected container image. Aileron prepares and validates the image, but the image must already contain the agent CLI.

Use `sandbox check` to validate an image before starting a daemon-backed session:

```bash
aileron sandbox check --runtime=docker --agent=claude
aileron sandbox check --runtime=podman --build=never --agent=codex
```

The check uses the same composition plan and minimal launch validation as `aileron launch --sandbox=...`: `/bin/sh`, the `/home/agent/workspace` mount, workspace write access, and the requested agent command on `PATH`.

## Support Matrix

| Agent | Command | Sandbox image support | MCP under `--sandbox=docker` | Notes |
|---|---|---|---|---|
| Claude Code | `claude` | Documented recipe | ✓ via `--mcp-config` | First-class recipe below. Use `sandbox check --agent=claude` before launch. |
| Codex | `codex` | Command contract only | ✓ via bind-mounted `config.toml` | Sandbox launch writes a generated `config.toml` to a host tempdir and bind-mounts it into `/home/agent/.codex/config.toml` (ADR-0024). Host `~/.codex/config.toml` is never touched. |
| Goose | `goose` | Command contract only | ✓ via `--with-extension` | Install the CLI in Tier 1 or BYO images; no maintained recipe yet. |
| OpenCode | `opencode` | Command contract only | ✓ via workspace `opencode.json` | Launcher writes `opencode.json` into the launch directory; the workspace bind-mount makes it readable in-container. |
| Pi | `pi` | Command contract only | ✓ via `--mcp-config` | Shares Claude's MCP wiring. |
| Other agents | varies | Unsupported | n/a | Add an Aileron launch agent and an image recipe before relying on sandbox launch. |

Under `--sandbox=docker` the launcher resolves the host-built `aileron-mcp` binary, bind-mounts it read-only at `/usr/local/bin/aileron-mcp`, builds an `mcpEnv` rewritten for the runtime (`host.docker.internal` on Docker, `host.containers.internal` on Podman), and calls each agent's `ConfigureMCP` hook. Four of the five agents (Claude, Pi, Goose, OpenCode) work without any agent-side code change because their config is either inline-with-exec (`--mcp-config`, `--with-extension`) or workspace-local (`opencode.json` in the bind-mounted workspace). Codex is the one exception — its host `~/.codex/config.toml` is irrelevant inside the container, so the launcher writes a generated `config.toml` to a host tempdir and bind-mounts it. See [ADR-0024](/adr/0024-sandbox-mcp-parity/) and the [manual walkthrough](/development/sandbox-mcp-walkthrough/) for the load-bearing flow.

Tier 0 `aileron/sandbox-base` intentionally does not include agent CLIs. Use Tier 1 when you want Aileron's base runtime plus an installed agent, or Tier 2 when your team owns the full image.

## Claude Code Recipe

`aileron sandbox init` scaffolds for Claude Code by default — the generated `.devcontainer/Dockerfile` already extends `aileron/sandbox-base`, switches to `USER root`, runs the Claude install, and switches back to `USER agent`. No edits required:

```bash
aileron sandbox init
```

Build and validate:

```bash
aileron sandbox build --runtime=docker
aileron sandbox check --runtime=docker --agent=claude
```

Then launch:

```bash
aileron launch --sandbox=docker claude
```

Claude Code still owns its own authentication flow. Do not bake Claude, Anthropic, cloud, or Aileron credentials into the image.

## BYO Image Contract

A BYO image must provide:

- `/bin/sh`
- a writable `/home/agent/workspace` bind mount when launched by Docker or Podman
- the requested agent command on `PATH`
- `wget` when Aileron mounts generated connector shims

Validate a BYO image by setting `customizations.aileron.image` in `.devcontainer/devcontainer.json` and running:

```bash
aileron sandbox check --runtime=docker --build=never --agent=claude
```

## Current Limits

The support matrix covers image contents only. It does not add live discovery refresh. Internal HTTPS proxy/session CA bootstrap work now expects images used for that development mode to provide `aileron-install-proxy-ca` and `aileron-run-with-proxy-ca`; the Aileron sandbox-base image includes both. Launch now authenticates standard proxy-shaped requests with proxy userinfo / `Proxy-Authorization`, but full forward-proxy transport remains tracked separately from the image support contract.
