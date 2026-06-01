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

| Agent | Command | Sandbox image support | Notes |
|---|---|---|---|
| Claude Code | `claude` | Documented recipe | First-class recipe below. Use `sandbox check --agent=claude` before launch. |
| Codex | `codex` | Command contract only | Install the CLI in Tier 1 or BYO images; no maintained recipe yet. |
| Goose | `goose` | Command contract only | Install the CLI in Tier 1 or BYO images; no maintained recipe yet. |
| OpenCode | `opencode` | Command contract only | Install the CLI in Tier 1 or BYO images; no maintained recipe yet. |
| Pi | `pi` | Command contract only | Install the CLI in Tier 1 or BYO images; no maintained recipe yet. |
| Other agents | varies | Unsupported | Add an Aileron launch agent and an image recipe before relying on sandbox launch. |

Tier 0 `aileron/sandbox-base` intentionally does not include agent CLIs. Use Tier 1 when you want Aileron's base runtime plus an installed agent, or Tier 2 when your team owns the full image.

## Claude Code Recipe

Start with the standard scaffold:

```bash
aileron sandbox init
```

Edit `.devcontainer/Dockerfile`:

```dockerfile
FROM aileron/sandbox-base:latest

USER root

RUN apk add --no-cache \
    git \
    nodejs \
    npm \
    ripgrep \
    && npm install -g @anthropic-ai/claude-code

USER agent
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

The support matrix covers image contents only. It does not add shell mediation, HTTPS proxy/session CA bootstrap, credential injection, or live discovery refresh. Those runtime layers are tracked separately.
