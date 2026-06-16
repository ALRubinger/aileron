---
title: "Sandbox Agent Images"
description: "Supported agent commands, image requirements, and recipes for Aileron sandboxes."
order: 7
---

Sandbox launch runs the agent command inside the selected container image. Aileron prepares and validates the image, but the image must already contain the agent CLI.

Use `sandbox check` to validate an image before starting a daemon-backed session:

```bash
aileron sandbox check --runtime=docker --agent=claude
aileron sandbox check --runtime=docker --build=never --agent=codex
```

The check uses the same composition plan and minimal launch validation as `aileron launch --sandbox=...`: `/bin/sh`, the `/home/agent/workspace` mount, workspace write access, and the requested agent command on `PATH`.

## Support Matrix

| Agent | Command | Sandbox image support | MCP under `--sandbox=docker` | Notes |
|---|---|---|---|---|
| Claude Code | `claude` | Documented recipe | ✓ via `--mcp-config` | First-class recipe below. Use `sandbox check --agent=claude` before launch. |
| Codex | `codex` | Documented recipe | ✓ via bind-mounted `config.toml` | Recipe below; scaffold with `sandbox init --agent=codex`. Sandbox launch writes a generated `config.toml` to a host tempdir and bind-mounts it into `/home/agent/.codex/config.toml` (ADR-0024). Host `~/.codex/config.toml` is never touched. |
| Goose | `goose` | Command contract only | ✓ via `--with-extension` | Install the CLI in Tier 1 or BYO images; no maintained recipe yet. |
| OpenCode | `opencode` | Command contract only | ✓ via workspace `opencode.json` | Launcher writes `opencode.json` into the launch directory; the workspace bind-mount makes it readable in-container. |
| Pi | `pi` | Command contract only | ✓ via `--mcp-config` | Shares Claude's MCP wiring. |
| Other agents | varies | Unsupported | n/a | Add an Aileron launch agent and an image recipe before relying on sandbox launch. |

Under `--sandbox=docker` the launcher resolves the host-built `aileron-mcp` binary, bind-mounts it read-only at `/usr/local/bin/aileron-mcp`, builds an `mcpEnv` rewritten for the runtime (`host.docker.internal` on Docker), and calls each agent's `ConfigureMCP` hook. Four of the five agents (Claude, Pi, Goose, OpenCode) work without any agent-side code change because their config is either inline-with-exec (`--mcp-config`, `--with-extension`) or workspace-local (`opencode.json` in the bind-mounted workspace). Codex is the one exception — its host `~/.codex/config.toml` is irrelevant inside the container, so the launcher writes a generated `config.toml` to a host tempdir and bind-mounts it. See [ADR-0024](/adr/0024-sandbox-mcp-parity/) and the [manual walkthrough](/development/sandbox-mcp-walkthrough/) for the load-bearing flow.

Docker is the only supported sandbox runtime in v4. Podman is planned but not yet supported; its `host.containers.internal` host alias is the deferred re-add path, and passing `--runtime=podman` fails with `podman runtime is not supported yet (v4 is Docker-only); see ADR-0014` (see [ADR-0014](/adr/0014-spawn-sandbox-technology/)).

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

## Codex Recipe

`aileron sandbox init --agent=codex` scaffolds a ready-to-build `.devcontainer/Dockerfile` for Codex. The `@openai/codex` npm package ships prebuilt musl binaries, so it installs cleanly on the Alpine base:

```bash
aileron sandbox init --agent=codex
```

Build and validate:

```bash
aileron sandbox build --runtime=docker
aileron sandbox check --runtime=docker --agent=codex
```

Then launch:

```bash
aileron launch --sandbox=docker codex
```

Codex owns its own authentication flow. Do not bake OpenAI, cloud, or Aileron credentials into the image.

## BYO Image Contract

A BYO image must provide:

- `/bin/sh`
- a writable `/home/agent/workspace` bind mount when launched by Docker
- the requested agent command on `PATH`

Validate a BYO image by setting `customizations.aileron.image` in `.devcontainer/devcontainer.json` and running:

```bash
aileron sandbox check --runtime=docker --build=never --agent=claude
```

## BYO Image Proxy Contract

`aileron launch --sandbox=docker` runs the HTTPS proxy by default ([ADR-0019](/adr/0019-v4-https-data-plane/)). The launcher mounts a session-scoped CA at `/etc/aileron/proxy/ca.pem`, sets standard proxy env (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`), and runs the agent through the `aileron-run-with-proxy-ca` wrapper. For the proxy to terminate TLS without breaking the agent's HTTPS clients, the in-container trust store must include that CA before the agent starts.

`aileron sandbox check --agent=<command>` validates the proxy contract for every `--runtime=docker` invocation. The check exits non-zero with an actionable message when the image is missing any of the required pieces below. The launch-time validation runs the same script.

A BYO image meets the proxy contract by providing two helpers on `PATH`:

| Helper | Purpose |
|---|---|
| `aileron-install-proxy-ca` | Installs the mounted CA into the in-container trust store. Must accept `--check` to dry-run the trust-store probe without writing anything, and must accept an optional positional CA file argument (default `${AILERON_SANDBOX_PROXY_CA_FILE:-/etc/aileron/proxy/ca.pem}`). Exits 0 on success, 2 when the CA file is missing or empty, 126 when invoked unprivileged for an install, 127 when the underlying trust-store tooling is missing. |
| `aileron-run-with-proxy-ca` | Entrypoint wrapper that installs the CA as root, then drops privileges to the `agent` user and executes the requested agent command. The launcher always starts the container through this wrapper when the proxy is in force. |

The canonical implementations ship with the `aileron/sandbox-base` image. BYO authors who derive from another base distro can write drop-in equivalents — the launcher only cares about the CLI shape, not the trust-store mechanism. Pick the mechanism that matches the base:

| Base distro | Install file at | Apply with | Notes |
|---|---|---|---|
| Debian / Ubuntu | `/usr/local/share/ca-certificates/aileron-sandbox-proxy-ca.crt` | `update-ca-certificates` | Requires the `ca-certificates` package. |
| Alpine | `/usr/local/share/ca-certificates/aileron-sandbox-proxy-ca.crt` | `update-ca-certificates` | Provided by the `ca-certificates` package. The sandbox-base image's helper already works on Alpine because Alpine's `update-ca-certificates` accepts the same input directory. |
| RHEL / Fedora / Amazon Linux | `/etc/pki/ca-trust/source/anchors/aileron-sandbox-proxy-ca.crt` | `update-ca-trust extract` | Requires the `ca-certificates` package. Write a small wrapper that mirrors the Debian helper's CLI but switches the install path and update command. |

Two operational requirements apply to every equivalent helper:

1. The CA must be installed as `root` once at container start, before the agent process runs. This is what `aileron-run-with-proxy-ca` guarantees by switching back to the `agent` user with `exec` after the install.
2. The install step must be idempotent — the same helper is invoked on every container start, and the same CA is installed every time. Existing `update-ca-certificates` / `update-ca-trust` implementations are naturally idempotent.

Validate a BYO image meets both the agent and proxy contracts with:

```bash
aileron sandbox check --runtime=docker --build=never --agent=claude
```

The check reports `support: ok` only when the agent command and both proxy helpers are present and the `--check` probe succeeds. To launch without the proxy — useful for images that cannot meet the contract during initial bring-up — pass `--sandbox-proxy=off` on `aileron launch`. `sandbox check` does not honor that opt-out; it always exercises the full contract so BYO authors see the same failures the launcher would see.

## Current Limits

The support matrix covers image contents only. It does not add live discovery refresh.
