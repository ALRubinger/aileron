---
title: "ADR-0022: v4 Tiered Network Policy"
description: "The v4 container runtime separates credentialed proxy-mediated traffic, direct uncredentialed egress, and denied private-network traffic."
order: 22
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Proposed</td></tr>
  <tr><th>Date</th><td>2026-06-01</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/896">#896</a>, <a href="https://github.com/ALRubinger/aileron/issues/747">#747</a></td></tr>
</table>
</div>

## Context

Sandboxed agents need network access for package managers, public APIs, LLM providers, and credentialed tools. Treating all egress the same either over-permits credential use or blocks normal development workflows.

## Decision

The v4 network model is tiered:

- Credentialed HTTPS traffic goes through the Aileron data plane, where policy, approval, credential injection, and audit occur.
- Uncredentialed public egress may be allowed directly by default and audited at the session/network layer.
- Private ranges and host-sensitive addresses should be denied by default unless explicitly opened by a documented runtime policy.
- Regulated deployments can move to default-deny plus allowlist without changing the composition tiers.

The first #796 runtime cut does not enforce this full policy. It establishes container execution and daemon-facing shims. The policy becomes load-bearing when #896 adds proxy/session CA bootstrap.

## Consequences

Users need clear launch-time messaging about what sandbox mode isolates today and which network controls are not yet active.

Proxy-mediated credential use must be distinguishable in audit from ordinary direct egress.

Runtime validation should catch missing proxy/CA support before the agent starts when a session requires credentialed network mediation.

## Alternatives Considered

**Deny all network by default.** Stronger but too restrictive for developer agents unless allowlist UX is already mature.

**Allow all network and rely on shims only.** Too weak for credential-sealing because third-party CLIs can bypass generated shims.

## References

- [Issue #896](https://github.com/ALRubinger/aileron/issues/896) — HTTPS proxy/data-plane mediation
- [ADR-0019](/adr/0019-v4-https-data-plane) — credentialed HTTPS mediation
