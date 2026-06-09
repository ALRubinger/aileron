---
title: "ADR-0018: v4 Single-Binary Runtime Model"
description: "v4 keeps one aileron runtime binary with multiple modes; the sandbox runtime does not revive aileron-mcp as a container sidecar or MCP-server architecture."
order: 18
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Proposed</td></tr>
  <tr><th>Date</th><td>2026-06-01</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/747">#747</a>, <a href="https://github.com/ALRubinger/aileron/issues/899">#899</a></td></tr>
</table>
</div>

## Context

Earlier launch work used `aileron-mcp` as the host-side bridge between agents and the local daemon. That remains part of the current host launch path for agents that consume MCP config.

The v4 container runtime needs a different shape. The agent runs inside a sandbox image, while Aileron owns the host daemon/data plane, generated shims, approvals, and credentials. Reviving `aileron-mcp` as the in-container sandbox model would split responsibilities across binaries and pull the design back toward an MCP-server-centric architecture that the v4 roadmap explicitly moved away from.

## Decision

v4 uses one `aileron` runtime binary with multiple modes and helper entrypoints as needed. The sandbox runtime contract is:

- `aileron launch --sandbox=...` prepares, validates, and runs the selected image.
- Container-side generated shims are HTTPS clients that call `AILERON_API_URL` or the future HTTPS data plane.
- Future proxy, shell-intercept, wait/drain, and service helpers are modes or helper artifacts of the Aileron runtime, not a separate MCP server model.
- `aileron-mcp` may continue to support the legacy/current host-launch MCP integration until that path is retired or superseded, but it is not the v4 sandbox control plane.

## Consequences

The sandbox runtime has one authority boundary: the Aileron daemon/data plane. Container-side helpers authenticate to it with launch-scoped env and mounted runtime artifacts.

Packaging can still include more than one executable artifact when the implementation needs helper binaries, but the architecture treats them as Aileron runtime modes/helpers. They do not define a second product model or a separate sidecar protocol.

Docs and issues must distinguish host launch from sandbox launch. Host launch can still mention `aileron-mcp`; sandbox launch should describe `AILERON_API_URL`, generated shims, and the future HTTPS proxy.

## Alternatives Considered

**Revive `aileron-mcp` inside the sandbox.** Rejected. It couples the v4 sandbox path to MCP catalog mechanics, keeps action discovery prompt-costly, and creates a second control surface beside the HTTPS data plane.

**Separate sidecar process per session.** Rejected for the first v4 runtime path. It complicates lifecycle and packaging before the need is concrete. The daemon/data-plane process can own the session contract.

## References

- [Issue #747](https://github.com/ALRubinger/aileron/issues/747) — v4 umbrella
- [Issue #796](https://github.com/ALRubinger/aileron/issues/796) — completed sandbox substrate
- [ADR-0017](/adr/0017-sandbox-composition) — sandbox composition
