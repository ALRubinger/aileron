---
title: "ADR-0021: v4 Shell-Layer Mediation"
description: "Inside the sandbox container, Aileron can own bash and mediate shell commands through a wrapper, DEBUG trap, and always-async approval semantics."
order: 21
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Withdrawn</td></tr>
  <tr><th>Date</th><td>2026-06-01</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/801">#801</a>, <a href="https://github.com/ALRubinger/aileron/issues/747">#747</a></td></tr>
</table>
</div>

## Context

> **Withdrawn 2026-06-08 ([#952](https://github.com/ALRubinger/aileron/issues/952)).** Slices 1–6 of [#801](https://github.com/ALRubinger/aileron/issues/801) shipped the image helper, daemon endpoint, launcher routing, and deny semantics described below; the strategic call in [#747](https://github.com/ALRubinger/aileron/issues/747) is that the surface has no named user. The four bypass paths this ADR acknowledges (env strip, recursion-guard tampering, interactive-shell spawn, `eval`/`command`/`exec`/function indirection) are better addressed by container isolation, git, the HTTPS proxy ([#896](https://github.com/ALRubinger/aileron/issues/896)), and tool-level HITL at the MCP/action boundary. Anything in this space that returns later returns under a fresh ADR. The body below is retained verbatim as historical record.

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

The second #801 implementation slice adds the internal launch opt-in. When `AILERON_SANDBOX_SHELL_MEDIATION=1` is set on the host, sandbox launch validates the selected image for the shell helper contract and passes shell-mediation metadata into the container. The agent command remains unwrapped.

The third #801 implementation slice establishes the daemon-side shell decision contract at `POST /v1/sandbox-shell/decide`. This first endpoint cut is allow-only and records sanitized `sandbox.shell.decided` audit events. It does not route shell execution, deny commands, create approvals, or drain approval results yet.

The fourth #801 implementation slice connects the image-side helper to that endpoint. When `AILERON_SANDBOX_SHELL_MEDIATION=1` is set, `aileron-shell-mediator intercept` POSTs the command to `POST /v1/sandbox-shell/decide` and exits zero only on a clean allow. Every other outcome fails closed, which covers a missing daemon URL or token, a network failure, a non-200 status, an unparseable body, and any decision other than allow. `aileron-bashrc` installs a bash `DEBUG` trap behind the opt-in that routes each about-to-run command through the mediator and vetoes the command when the mediator exits nonzero. The endpoint stays allow-only and launch still does not route `/bin/bash` or `/bin/sh` through the helper, so the live agent session is not yet mediated. Shell routing, deny and pending-approval policy, approval result draining with `aileron wait <id>`, and bypass and subshell-inheritance tests remain deferred to later #801 slices.

The fifth #801 implementation slice routes the live agent session's shell through the mediator. Launch now injects `BASH_ENV=/etc/aileron/shell/aileron-bashrc` and `SHELL=/usr/local/bin/bash` under the opt-in. `BASH_ENV` makes the agent's non-interactive `bash -c` children source the rcfile and install the `DEBUG` trap, which is the invocation model the agent actually uses. A single shell wrapper is baked as `/usr/local/bin/bash` and `/usr/local/bin/sh`, ahead of the real shells on `PATH`, so `which bash` resolves through Aileron and `sh`-only callers are routed into the trap-bearing bash. The wrapper execs the real shell by absolute path, so it cannot re-resolve to itself and loop. Image validation now fails before the agent starts when the opt-in is set but the wrapper is absent, which gives BYO images an actionable error. The `sandbox.shell.decided` audit event gains a server-side `aileron.shell.latency_ms` field measured across the decision handler. The agent command is still run directly, so routing lives at the shell layer and not the agent-process layer. The endpoint stays allow-only, so nothing is denied yet. Deny and pending-approval policy, approval result draining with `aileron wait <id>`, container-observed round-trip latency, and bypass and subshell-inheritance tests remain deferred to later #801 slices.

The sixth #801 implementation slice activates deny. The daemon decision endpoint reads `AILERON_SANDBOX_SHELL_DENY_PATTERN` at startup as a single deny-trigger regex. A daemon started with the variable set but the value uncompilable refuses to start, so a typo cannot leave an operator with a sandbox that silently allows. When the pattern matches the about-to-run command, the daemon returns `decision: "deny"` with the stable reason format `matched deny pattern: <pattern source>`, and the audit payload carries the same reason plus a new `aileron.shell.matched_pattern` field. The byte-for-byte allow path is preserved when the variable is unset. On the container side, the `DEBUG` trap now distinguishes interactive and non-interactive shells. Under non-interactive `bash -c`, which is the agent's invocation model, the trap calls `exit "$rc"` when the mediator denies, so the rest of `&& cmd` and `; cmd` chains do not run and the bash process exits with a nonzero status that reflects the mediator's decision. Under an interactive REPL, the trap returns 1 instead, so extdebug suppresses the about-to-run command, the `[Aileron] denied:` line reaches stderr, and the prompt stays alive. A human typing in a sandbox shell is not killed by a single denied command. The OpenAPI contract now enumerates `decision` as `allow`, `deny`, or the future `pending_approval`, and a `Shellcheck` step on the CI lint job catches POSIX-versus-bash drift in `aileron-bashrc`, `aileron-shell-mediator`, and the wrapper before they reach the image. Slice 6 is detection plus reliable veto when the agent cooperates; it is not untrusted-code containment. Four bypass paths are acknowledged but not closed in this slice. An adversarial agent can `unset AILERON_SANDBOX_SHELL_MEDIATION` or `unset BASH_ENV` before spawning child shells, and the rcfile returns inert in that case. The recursion guard `_AILERON_IN_TRAP` lives in the environment, so an agent that exports it once defeats the guard for every subsequent command. An agent that spawns `bash -ic` reaches the soft-veto branch designed for human REPLs and observes exit 0 after a deny. Regex matching against `$BASH_COMMAND` does not see through `eval`, `command`, `exec`, function indirection, or base64-then-eval payloads. Closing these holes is a separate architecture track that owns per-exec env injection by a supervisor process, an off-env recursion guard, wrapper-layer non-interactive enforcement, and either shallow command unwrapping or a different match surface entirely. Real policy DSL replacing the env-driven trigger, approval-pending with result draining and `aileron wait <id>`, container-observed round-trip latency telemetry, and bypass and subshell-inheritance test coverage remain deferred to later #801 slices.

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
