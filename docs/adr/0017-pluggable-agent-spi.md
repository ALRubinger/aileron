# ADR-0017: Pluggable Agent SPI

**Status:** Accepted
**Date:** 2026-04-11

## Context

ADR-0013 positioned Aileron as a local policy-enforced shell that intercepts agent commands via `SHELL=aileron-sh`. The initial implementation hardcoded agent-specific behavior:

1. **Command normalization** lived in `aileron-sh` as a `switch` statement. Claude Code wraps commands in `eval '...'`; other agents don't. Adding a new agent required editing the shim binary.

2. **Shell configuration** was Claude-specific. The launcher unconditionally installed a wrapper script at `~/.aileron/bash` and set `CLAUDE_CODE_SHELL` — an env var only Claude Code reads. Other agents that ignore `$SHELL` (e.g. Pi, which hardcodes `/bin/bash` and reads its shell from a settings file) had no hook to configure their shell resolution.

3. **The `Agent` interface** had four methods (`Name`, `BinaryNames`, `Args`, `Env`) — enough to launch an agent but not enough to handle the behavioral differences between agents.

The community requested Pi.dev support ([#98](https://github.com/ALRubinger/aileron/issues/98)), which exposed all three gaps: Pi wraps commands differently (not at all), resolves its shell differently (settings file, not `$SHELL`), and needs agent-specific pre-launch setup.

## Decision

Extend the `Agent` interface into a Service Provider Interface (SPI) with three additional methods:

### 1. `NormalizeCommand(raw string) (command string, evaluate bool)`

Each agent owns its command-unwrapping logic. The shim (`aileron-sh`) calls `agents.NormalizeCommand(agentName, raw)` which delegates to the registered agent's implementation. No switch statements.

- **Claude:** Unwraps `eval '...'` from the `shopt ... && eval 'cmd' < /dev/null && pwd ...` template. Returns `(cmd, true)` for user commands, `(raw, false)` for infrastructure commands (snapshots, etc.).
- **Pi:** Passthrough — `(raw, true)`. Pi doesn't wrap commands.
- **Unknown agents:** Passthrough — `(raw, true)`. Safe default.

### 2. `ConfigureShell(shimPath, dir string) error`

Agents that need pre-launch setup to use `aileron-sh` implement this. Called by the launcher before spawning the agent process.

- **Claude:** Installs a wrapper script at `~/.aileron/bash` (path must contain "bash" for Claude Code's shell validation). Sets `CLAUDE_CODE_SHELL` via `Env()`.
- **Pi:** Writes `.pi/settings.json` with `"shellPath": "/path/to/aileron-sh"`. Pi ignores `$SHELL` and reads its shell from this file.
- **Agents that respect `$SHELL`:** Return `nil` (no-op).

### 3. Normalizer registry (bridge pattern)

`aileron-sh` is a separate binary that cannot hold Go references to `Agent` structs. It only knows the agent name from the `AILERON_AGENT` env var. The `agents.NormalizeCommand(name, raw)` function bridges this gap with a package-level registry mapping agent names to their normalizer implementations.

## Consequences

### Adding a new agent

One file (`core/launch/agents/<name>.go`) implementing the seven-method `Agent` interface, plus one `Register()` call in `cmd/aileron/main.go` and one entry in the normalizer registry. No edits to `aileron-sh`, the launcher, or any existing agent.

### Agent-specific behavior is self-contained

Claude's eval unwrapping, wrapper installation, and `CLAUDE_CODE_SHELL` env var are all defined in `claude.go`. Pi's settings.json writing is in `pi.go`. The launcher and shim are agent-agnostic.

### The normalizer registry is a pragmatic compromise

A pure SPI would have zero static registrations. The normalizer registry (`agents/normalize.go`) exists because `aileron-sh` is a compiled binary that resolves agents by string name at runtime. The registry is the thinnest possible bridge — a map of name to interface. If Aileron later supports plugin-based agents (shared libraries, external binaries), this registry becomes the integration point.

### `$SHELL` is not universal

The assumption in ADR-0013 that "any agent that respects `$SHELL`" would work was optimistic. Pi hardcodes `/bin/bash`. Other agents may resolve their shell from config files, env vars, or CLI flags. `ConfigureShell` absorbs this variance without complicating the launcher.
