---
title: "ADR-0018: v4 Single-Binary Runtime Model"
description: "v4 keeps one aileron runtime binary with multiple modes. The sandbox runtime hosts aileron-mcp as an in-container stdio subprocess per ADR-0024; it does not adopt a separate sidecar protocol or second product model."
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

The v4 container runtime needs a different shape. The agent runs inside a sandbox image, while Aileron owns the host daemon/data plane, generated shims, approvals, and credentials. The original framing of this ADR rejected reviving `aileron-mcp` as the in-container sandbox model, arguing it would split responsibilities across binaries and pull the design back toward an MCP-server-centric architecture that the v4 roadmap had explicitly moved away from.

> **Revision note, 2026-06-08:** The "no `aileron-mcp` in sandbox" position is reversed by [ADR-0024](/adr/0024-sandbox-mcp-parity) and [#953](https://github.com/ALRubinger/aileron/issues/953). Under sandbox launch, `aileron-mcp` runs as a stdio subprocess of the in-container agent, reached via a read-only host-mounted binary, with the daemon reached over HTTPS through the existing `AILERON_URL` rewrite. The single-binary-runtime principle this ADR ratifies is preserved at the host-launch level and extended to sandbox launch through reuse of the existing `Agent.ConfigureMCP` contract and the established `sandboxDiscoveryMounts` host-mount pattern. No second sidecar protocol is introduced. The Decision section below is rewritten to reflect this; the Consequences section's "one authority boundary" claim is unchanged.

## Decision

v4 uses one `aileron` runtime binary with multiple modes and helper entrypoints as needed. The sandbox runtime contract is:

- `aileron launch --sandbox=...` prepares, validates, and runs the selected image.
- Container-side generated shims are HTTPS clients that call `AILERON_API_URL` or the future HTTPS data plane.
- Future proxy, wait/drain, and service helpers are modes or helper artifacts of the Aileron runtime, not a separate MCP server model.
- `aileron-mcp` is the canonical MCP-server entrypoint under BOTH host launch AND sandbox launch. Under sandbox launch it runs in-container as a stdio subprocess of the agent, reached via a read-only host-mounted binary at `/usr/local/bin/aileron-mcp`. See [ADR-0024](/adr/0024-sandbox-mcp-parity) for the sandbox-revival decision and rationale.

## Consequences

The sandbox runtime has one authority boundary: the Aileron daemon/data plane. Container-side helpers authenticate to it with launch-scoped env and mounted runtime artifacts.

Packaging can still include more than one executable artifact when the implementation needs helper binaries, but the architecture treats them as Aileron runtime modes/helpers. They do not define a second product model or a separate sidecar protocol.

Docs and issues distinguish host launch from sandbox launch. Both paths register `aileron-mcp` with the agent. Sandbox launch additionally describes `AILERON_API_URL`, generated shims, the future HTTPS proxy, and the read-only host-mount of the MCP binary; see [ADR-0024](/adr/0024-sandbox-mcp-parity).

## Alternatives Considered

**Revive `aileron-mcp` inside the sandbox.** Originally rejected on the grounds that it would couple the v4 sandbox path to MCP catalog mechanics and create a second control surface beside the HTTPS data plane. **Revised 2026-06-08:** accepted, recorded as [ADR-0024](/adr/0024-sandbox-mcp-parity). The MCP catalog cost is real but is paid in exchange for tool-selection parity with host launch; the "second control surface" concern is mitigated by the daemon remaining the single execution chokepoint and by the fact that user-installed MCP servers continue to register through the user's own config rather than through any Aileron-aggregation layer.

**Separate sidecar process per session.** Rejected for the first v4 runtime path. It complicates lifecycle and packaging before the need is concrete. The daemon/data-plane process can own the session contract.

## References

- [Issue #747](https://github.com/ALRubinger/aileron/issues/747) — v4 umbrella
- [Issue #796](https://github.com/ALRubinger/aileron/issues/796) — completed sandbox substrate
- [Issue #953](https://github.com/ALRubinger/aileron/issues/953) — sandbox MCP parity (the revision driver)
- [ADR-0017](/adr/0017-sandbox-composition) — sandbox composition
- [ADR-0024](/adr/0024-sandbox-mcp-parity) — sandbox MCP parity (Path B1)
