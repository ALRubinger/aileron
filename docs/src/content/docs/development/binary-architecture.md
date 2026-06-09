---
title: "Binary Architecture"
description: "The binaries that make up an Aileron install, the trust boundary each owns, and how host and sandbox launch differ."
order: 2
---

An Aileron install currently ships multiple cooperating binaries, but the v4 runtime model treats `aileron` as the product boundary. Host launch can still use `aileron-mcp` as an adapter for MCP-aware agents; sandbox launch does not revive `aileron-mcp` as the in-container runtime model. See [ADR-0018](/adr/0018-v4-single-binary-runtime/).

## The three binaries

| Binary | Source | Trust boundary | Called by |
|---|---|---|---|
| `aileron` | [`cmd/aileron`](https://github.com/ALRubinger/aileron/tree/main/cmd/aileron) | CLI plus user-scoped local daemon (per [ADR-0012](/adr/0012-local-daemon-architecture)). The vault, sessions, audit log, connector store, binding store, sandbox launch, and future v4 data-plane modes live behind this runtime boundary. | The user, launch flows, generated sandbox shims, and future runtime helpers. |
| `aileron-mcp` | [`cmd/aileron-mcp`](https://github.com/ALRubinger/aileron/tree/main/cmd/aileron-mcp) | Host-launch MCP adapter. Translates an MCP `tools/call` request from the agent host into an HTTP POST to the local daemon's `/v1/actions/{name}/run`. | Host-launched MCP-aware agent hosts. Not the v4 sandbox control plane. |
| `aileron-enclave` | [`cmd/aileron-enclave`](https://github.com/ALRubinger/aileron/tree/main/cmd/aileron-enclave) | TEE-side handler. Runs inside a confidential VM in production (Google Confidential Space, AMD SEV-SNP) and as a local dev process when `AILERON_TEE_PROVIDER=local`. Owns the long-lived credential escrow. | The daemon (`aileron`), over HTTPS, when a credential needs TEE-mediated use. Post-MVP for the default install. |

## How they cooperate

```
┌─────────────────────────────┐
│  Agent host (Claude Code,   │
│  Codex CLI, Goose,          │
│  OpenCode, …)               │
└──────────┬──────────────────┘
           │
           │ MCP tools/call
           │ + LLM gateway
           ▼
   ┌────────────────┐
   │  aileron-mcp   │  (host launch only)
   └───────┬────────┘
           │
           │ HTTP /v1/actions
           ▼
        ┌─────────────────────────┐
        │   aileron daemon        │
        │  (cmd/aileron)          │
        │  vault, sessions,       │
        │  audit, connector       │
        │  store, executor,       │
        │  LLM gateway            │
        └───────────┬─────────────┘
                    │
                    │ HTTPS (TEE-mediated credential use)
                    ▼
              ┌────────────────┐
              │ aileron-enclave│
              │ (TEE/local)    │
              └────────────────┘
```

The daemon is the trust pivot. Vault unlock happens in its process; nothing outside it ever sees the master key. The other binaries are thin clients that hand requests to the daemon and surface the daemon's structured responses back to whoever called them.

Per [ADR-0015](/adr/0015-launch-audit-scope), Aileron's host-launch audit boundary is the actions Aileron executes (connector installs, action invocations, binding lifecycle, gateway-routed LLM requests). Commands the host-launched agent runs locally through its own exec tool — `git`, `go test`, `rm` — are outside this boundary. Container-only shell mediation was prototyped under [#801](https://github.com/ALRubinger/aileron/issues/801) and withdrawn in [#952](https://github.com/ALRubinger/aileron/issues/952); see [ADR-0021](/adr/0021-v4-shell-layer-mediation/) (Withdrawn).

## What runs where

- **`aileron launch <agent>`** with `--sandbox=off` starts the agent host as a subprocess and wires `aileron-mcp` into its MCP transport (per [ADR-0012](/adr/0012-local-daemon-architecture)). The daemon auto-spawns on first call and stays running as long as a session is active.
- **`aileron launch --sandbox=auto|docker|podman <agent>`** prepares the selected sandbox image, validates it, and runs the agent command in a container. It injects `AILERON_API_URL`, session metadata, `tools.txt`, and generated connector shims. It does not run `aileron-mcp` inside the container.
- **`aileron daemon start|stop|status`** controls the daemon directly. Useful for diagnostics, rare in normal use.
- **`aileron-mcp`** can be installed standalone with `task mcp:setup` when an agent host needs the MCP server outside an `aileron launch` session.
- **`aileron-enclave`** is post-MVP for the default install. The credential vault works without it via the local-mode path in [ADR-0011](/adr/0011-local-credential-vault). When the TEE-backed escrow lands, this binary is what runs inside Confidential Space.

## Versions are coupled

All shipped binaries are built from the same repo at the same commit. The release pipeline produces a matched set; a `task build` builds them together. Mismatched versions (e.g., a newer `aileron-mcp` talking to an older daemon) are not supported, because the daemon's HTTP API is the contract and we have not committed to compatibility across versions yet.

This is a pre-MVP simplification. Per [ADR-0012](/adr/0012-local-daemon-architecture)'s versioning section, a stable wire protocol is a post-MVP concern; until then, ship the binaries together.

## See also

- [ADR-0012: Local Daemon Architecture](/adr/0012-local-daemon-architecture)
- [ADR-0015: Launch Audit Scope](/adr/0015-launch-audit-scope)
- [ADR-0018: v4 Single-Binary Runtime Model](/adr/0018-v4-single-binary-runtime)
- [ADR-0019: v4 HTTPS Data-Plane Mediation](/adr/0019-v4-https-data-plane)
- [Building from Source](/development/building-from-source/)
- [Repo Layout](/development/repo-layout/)
