---
title: "Supported Agents"
description: "Agents that work with Aileron"
---

Aileron is agent-portable. Your policy, credentials, and audit trail come with you when you switch agents.

| Agent | Shell policy | MCP tools | Full experience |
|-------|-------------|-----------|-----------------|
| Claude Code | Yes | Yes | Yes |
| [pi.dev](https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent) | Yes | Not yet | Shell policy now |
| OpenCode | Yes | Yes | Yes |
| Goose | Yes | Yes | Yes |
| Amp | Yes | Yes | Yes |
| Aider | Yes | In progress | Shell policy now |
| Codex CLI | Yes | Not yet | Shell policy only |
| Cline (VS Code) | Yes | Yes | Yes |

## How agent support works

Aileron uses a pluggable agent SPI. Each agent exposes a standard interface for command normalization (unwrapping agent-specific command wrapping) and shell configuration (pre-launch setup). This means agent-specific quirks are handled transparently, and the policy rules stay clean.

For agents that don't respect `$SHELL` directly, Aileron installs a wrapper script that satisfies the agent's shell validation and delegates to `aileron-sh`.

See [ADR-0017: Pluggable Agent SPI](/adr/0017-pluggable-agent-spi) for the design rationale.
