---
title: "Binary Architecture"
description: "The four binaries that make up an Aileron install, the trust boundary each owns, and how they connect to the agent host."
order: 2
---

An Aileron install ships as four cooperating binaries. Each owns a narrow trust boundary; the install pipeline puts them in your PATH and the agent host calls into whichever one it needs. End users rarely think about more than `aileron` itself, but every page in this docs section assumes you understand the split.

## The four binaries

| Binary | Source | Trust boundary | Called by |
|---|---|---|---|
| `aileron` | [`cmd/aileron`](https://github.com/ALRubinger/aileron/tree/main/cmd/aileron) | CLI plus user-scoped local daemon (per [ADR-0012](/adr/0012-local-daemon-architecture)). The vault, sessions, audit log, connector store, and binding store all live behind the daemon. | The user (interactive CLI) and the other three binaries (as clients). |
| `aileron-mcp` | [`cmd/aileron-mcp`](https://github.com/ALRubinger/aileron/tree/main/cmd/aileron-mcp) | MCP server adapter. Translates an MCP `tools/call` request from the agent host into an HTTP POST to the local daemon's `/v1/actions/{name}/run`. | The agent host (Claude Code, Cursor, Continue, anything that speaks MCP). |
| `aileron-sh` | [`cmd/aileron-sh`](https://github.com/ALRubinger/aileron/tree/main/cmd/aileron-sh) | Shell shim and Claude Code `PreToolUse` hook. Evaluates a command against the loaded shell policy, asks for approval where required, and passes through to the real shell on allow. | The agent host's shell-invocation path. |
| `aileron-enclave` | [`cmd/aileron-enclave`](https://github.com/ALRubinger/aileron/tree/main/cmd/aileron-enclave) | TEE-side handler. Runs inside a confidential VM in production (Google Confidential Space, AMD SEV-SNP) and as a local dev process when `AILERON_TEE_PROVIDER=local`. Owns the long-lived credential escrow. | The daemon (`aileron`), over HTTPS, when a credential needs TEE-mediated use. Post-MVP for the default install. |

## How they cooperate

```
┌─────────────────────────────┐
│  Agent host (Claude Code,   │
│  Cursor, Continue, …)       │
└──────────┬──────────────────┘
           │                  │
           │ MCP              │ shell exec
           │ tools/call       │ (PreToolUse hook)
           ▼                  ▼
   ┌────────────────┐  ┌──────────────┐
   │  aileron-mcp   │  │  aileron-sh  │
   └───────┬────────┘  └──────┬───────┘
           │                  │
           │ HTTP /v1/actions │ HTTP /v1/shell/check
           ▼                  ▼
        ┌─────────────────────────┐
        │   aileron daemon        │
        │  (cmd/aileron)          │
        │  vault, sessions,       │
        │  audit, connector       │
        │  store, executor        │
        └───────────┬─────────────┘
                    │
                    │ HTTPS (TEE-mediated credential use)
                    ▼
              ┌────────────────┐
              │ aileron-enclave│
              │ (TEE/local)    │
              └────────────────┘
```

The daemon is the trust pivot. Vault unlock happens in its process; nothing outside it ever sees the master key. The other three binaries are thin clients that hand requests to the daemon and surface the daemon's structured responses back to whoever called them.

## What runs where

- **`aileron launch <agent>`** starts the agent host as a subprocess and wires `aileron-mcp` into its MCP transport (per [ADR-0012](/adr/0012-local-daemon-architecture)). The daemon auto-spawns on first call and stays running as long as a session is active.
- **`aileron daemon start|stop|status`** controls the daemon directly. Useful for diagnostics, rare in normal use.
- **`aileron-mcp`** can be installed standalone with `task mcp:setup` when an agent host needs the MCP server outside an `aileron launch` session.
- **`aileron-sh`** has two entry points: `--hook` for Claude Code's `PreToolUse` JSON contract, and `-c "command"` for agents that invoke `$SHELL` directly.
- **`aileron-enclave`** is post-MVP for the default install. The credential vault works without it via the local-mode path in [ADR-0011](/adr/0011-local-credential-vault). When the TEE-backed escrow lands, this binary is what runs inside Confidential Space.

## Versions are coupled

All four binaries are built from the same repo at the same commit. The release pipeline produces a matched set; a `task build` builds all four. Mismatched versions (e.g., a newer `aileron-mcp` talking to an older daemon) are not supported, because the daemon's HTTP API is the contract and we have not committed to compatibility across versions yet.

This is a pre-MVP simplification. Per [ADR-0012](/adr/0012-local-daemon-architecture)'s versioning section, a stable wire protocol is a post-MVP concern; until then, ship the four binaries together.

## See also

- [ADR-0012: Local Daemon Architecture](/adr/0012-local-daemon-architecture)
- [Building from Source](/development/building-from-source/)
- [Repo Layout](/development/repo-layout/)
