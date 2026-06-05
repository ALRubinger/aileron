---
title: "ADR-0021: v4 Shell-Layer Mediation"
description: "Inside the sandbox container, Aileron can own bash and mediate shell commands through a wrapper, DEBUG trap, and always-async approval semantics."
order: 21
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Proposed</td></tr>
  <tr><th>Date</th><td>2026-06-01</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/801">#801</a>, <a href="https://github.com/ALRubinger/aileron/issues/747">#747</a></td></tr>
</table>
</div>

## Context

[ADR-0015](/adr/0015-launch-audit-scope) removed host shell interception because host-launched agents do not reliably honor shell overrides and Aileron should not mutate the user's shell.

The container runtime changes that boundary. In an Aileron-owned sandbox image, `/bin/bash` and `/bin/sh` can be runtime substrate. Shell mediation can therefore exist in the container without reviving the fragile host shell-shim model.

## Decision

Shell-layer mediation is a container-only runtime layer tracked in #801:

- The sandbox image routes `/bin/bash` and `/bin/sh` through an Aileron wrapper.
- The wrapper invokes real bash under a locked-down Aileron rcfile.
- A DEBUG trap asks the Aileron daemon/data plane for allow, deny, mediate, or pending decisions before commands run.
- Approval-required commands return quickly with a pending id; they do not block the agent process while waiting for the user.
- Follow-up result draining and `aileron wait <id>` are explicit synchronization mechanisms.

This layer builds on #796's container execution substrate and should share policy/audit concepts with the HTTPS data plane.

The first #801 implementation slice establishes the image-side helper contract without enabling mediation. `aileron/sandbox-base` now includes `/usr/local/bin/aileron-shell-mediator` and `/etc/aileron/shell/aileron-bashrc`, and sandbox image validation has an opt-in probe for those files. Launch does not route `/bin/bash` or `/bin/sh` through the helper yet; later #801 slices wire shell routing, daemon decisions, approval result draining, bypass tests, and audit.

## Consequences

Host launch remains governed by ADR-0015: Aileron audits Aileron actions, not arbitrary host commands.

Sandbox launch can add command-level mediation because the shell is part of the controlled image. This is the path toward the broader Aileron Way claim for containerized sessions.

Compatibility risk is real. The implementation needs clear debug mode, scoped rollout, latency targets, and tests for bypass attempts and subshells.

## Alternatives Considered

**Bring back host shell interception.** Rejected. The constraints that led to ADR-0015 still apply outside the container.

**Kernel/syscall mediation first.** Deferred. Kernel mediation remains a deeper enforcement direction, but shell-layer mediation is the pragmatic v4 layer.

## References

- [Issue #801](https://github.com/ALRubinger/aileron/issues/801) — shell-layer interception
- [ADR-0015](/adr/0015-launch-audit-scope) — host launch audit boundary
- [ADR-0017](/adr/0017-sandbox-composition) — sandbox substrate
