---
title: "v4 Acceptance Scorecard"
description: "The living 'is v4 done?' runbook and pass/fail scorecard, mapped 1:1 to the v4 acceptance bar in issue #747."
order: 11
---

This page is the living checkpoint for v4 delivery. It answers one question: is the containerized AI-native runtime done?

The v4 bar comes from [issue #747](https://github.com/ALRubinger/aileron/issues/747): an agent launches in the sandbox, authenticates, exercises actions and connectors through the Aileron HTTPS data plane, and every flow is audited, verified across macOS, Linux, and Windows (Docker), for every supported agent, with a recorded walkthrough. Per the 2026-06-15 scope note, Docker is the only runtime on all three operating systems (Podman stays descoped), and verification is automated on Linux with a light per-agent manual smoke on macOS and Windows.

This page orients and links. It does not duplicate the proof-out steps; those live in the walkthrough and auth pages below. Update the scorecard here as gating items move.

## Current verdict

**v4 is NOT yet delivered.** Two gating items are outstanding: the macOS and Windows manual smoke for the Manual E2E item ([#962](https://github.com/ALRubinger/aileron/issues/962)), and the demo script plus recorded walkthrough ([#852](https://github.com/ALRubinger/aileron/issues/852)). Everything else in the v4 bar is shipped.

## How to prove v4 end-to-end

The full launch, authenticate, exercise-actions-and-connectors-through-the-HTTPS-data-plane, and confirm-audit sequence is already written. Compose these pages rather than re-running ad hoc steps:

1. **Launch and exercise the data plane.** The [Sandbox MCP Manual Verification Walkthrough](/development/sandbox-mcp-walkthrough/) walks an action end-to-end inside a v4 sandbox container through the agent's MCP transport, with a real connector and a HITL approval, and verifies the `approval.requested → approval.approved → execution.started → execution.succeeded` audit chain.
2. **Authenticate the agent.** The [Sandbox Agent Auth](/development/sandbox-agent-auth/) page covers vault-backed credential injection for `aileron launch <agent> --sandbox=docker`: the vault path scheme, the per-agent envelope schemas, the in-container login-then-snapshot flow, and the recovery path.
3. **Verify CLI traffic through the proxy.** The [Sandbox Proxy CLI Verification Matrix](/development/sandbox-proxy-cli-matrix/) verifies the v4 HTTPS proxy with `curl`, `gh`, and `aws`, with the expected audit events for each.

### The supported matrix

The v4 bar requires every supported agent on Docker across all three operating systems:

| Agent | macOS Docker | Linux Docker | Windows Docker |
|---|---|---|---|
| Claude | manual smoke | automated (CI) | manual smoke |
| Pi | manual smoke | automated (CI) | manual smoke |
| Goose | manual smoke | automated (CI) | manual smoke |
| OpenCode | manual smoke | automated (CI) | manual smoke |
| Codex | manual smoke | automated (CI) | manual smoke |

The Linux column is automated in CI (the per-agent MCP-registration matrix plus the `aileron-mcp`↔daemon round-trip, merged in PR [#1064](https://github.com/ALRubinger/aileron/pull/1064)), so those cells are green when CI is green and you do not hand-run them. The macOS and Windows columns are a light per-agent manual smoke (the agent lists `aileron` with `draft_email`), recorded in [#962](https://github.com/ALRubinger/aileron/issues/962), because GitHub-hosted mac and Windows runners cannot run Linux Docker containers. The walkthrough documents this split and the per-agent×runtime table in full, including the per-agent MCP registration mechanism and the Codex troubleshooting notes.

## Acceptance scorecard

This maps 1:1 to the "Verify and prove" bar in [#747](https://github.com/ALRubinger/aileron/issues/747). Status legend: ✅ done, ☐ pending.

### Gating items (these gate v4 closure)

| Item | Status | Notes |
|---|---|---|
| Integration test variants — in-container, never-approved, concurrency ([#960](https://github.com/ALRubinger/aileron/issues/960)) | ✅ | Merged via PR [#1037](https://github.com/ALRubinger/aileron/pull/1037) on 2026-06-13. |
| Manual E2E ([#962](https://github.com/ALRubinger/aileron/issues/962)) | ☐ | Linux automated and green via PR [#1064](https://github.com/ALRubinger/aileron/pull/1064) for all five agents. macOS and Windows light per-agent smoke pending. Claude × macOS full round-trip done 2026-06-15. |
| Demo script + recorded walkthrough ([#852](https://github.com/ALRubinger/aileron/issues/852)) | ☐ | Pending. |

Two gating items remain open, so the verdict above is NOT yet delivered.

### Non-gating items (visibly distinguished; these do NOT gate v4 closure)

| Item | Status | Why it does not gate v4 |
|---|---|---|
| Agent auth v4.x deferral ([#1027](https://github.com/ALRubinger/aileron/issues/1027)) | ☐ | Now just [#1025](https://github.com/ALRubinger/aileron/issues/1025) (container integration test), explicitly v4.x. [#987](https://github.com/ALRubinger/aileron/issues/987) (read-only-FS EnvBinding) was re-parented to v5 [#1065](https://github.com/ALRubinger/aileron/issues/1065) on 2026-06-17. |
| Platform packaging — Scoop, Homebrew ([#1094](https://github.com/ALRubinger/aileron/issues/1094)) | ✅ | Windows install via Scoop supports the v4 Windows requirement but does not gate closure. Closure gates on the runtime smoke ([#962](https://github.com/ALRubinger/aileron/issues/962)), not packaging. |

## Sources

- [Issue #747](https://github.com/ALRubinger/aileron/issues/747) — the v4 milestone, its "Verify and prove" bar, and the 2026-06-15 scope note this scorecard tracks.
- [Sandbox MCP Manual Verification Walkthrough](/development/sandbox-mcp-walkthrough/) — the per-agent×runtime proof-out steps and the automated-Linux / manual-mac-Windows split.
- [Sandbox Agent Auth](/development/sandbox-agent-auth/) — the authenticate step of the v4 bar.
- [Sandbox Proxy CLI Verification Matrix](/development/sandbox-proxy-cli-matrix/) — the data-plane proxy verification with `curl`, `gh`, and `aws`.
