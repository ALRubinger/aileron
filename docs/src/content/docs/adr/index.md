---
title: "Architecture Decisions"
description: "Architecture decision records for Aileron"
order: 0
---

This section holds the architecture decision records (ADRs) that define Aileron's architecture. Each ADR captures one decision: the context, the choice, the trade-offs, and the consequences.

## What's landed

- [ADR-0001: Manifest Format Conventions](/adr/0001-manifest-format) — Markdown body + TOML frontmatter for actions; pure TOML for connectors and project config; JSON for runtime IPC.
- [ADR-0002: Connector Model](/adr/0002-connector-model) — Connectors are sandboxed, content-addressed binaries; Aileron core ships only primitive capability types.
- [ADR-0003: Action Model](/adr/0003-action-model) — Atomic actions copied into the user's `~/.aileron/actions/` on install; depend on connectors with explicit version + hash + capability subsets; capability enforcement at the action boundary in addition to the connector boundary. v1 is user-level only.
- [ADR-0004: Dependency Resolution](/adr/0004-dependency-resolution) — Per-scheme FQN-to-fetch-URL mapping (Go modules tag conventions); content-addressed shared store; install/sync/check pipelines; offline-by-default after first install.
- [ADR-0005: Sandbox Choice](/adr/0005-sandbox-choice) — WASM Component Model is the connector sandbox; runtime mediates credential use so connectors never hold raw credentials. Non-WASM-capable connectors are out of scope for v1.
- [ADR-0006: Capability Binding UX](/adr/0006-capability-binding-ux) — Bindings are explicit user acts; install-time transparency, first-use binding, opt-in pre-binding; bindings are user-local, never project-committable; visible lifecycle commands (list/inspect/setup/rebind/revoke).
- [ADR-0007: Install Consent Flow](/adr/0007-install-consent) — Install is a single binary decision (install or cancel); no per-capability denial; signature failure is a hard fail; updates show capability diffs; headless mode requires explicit `--yes` per command.
- [ADR-0008: Intent Matching Mechanisms](/adr/0008-intent-matching) — Tool augmentation via function calling is the primary mechanism; pre-LLM bypass for high-confidence patterns is opt-in for read-only actions only; agent-defined tools take precedence in name collisions.
- [ADR-0009: User Channel and OOB Approval Surfaces](/adr/0009-user-channel) — In-band streaming output describes; out-of-band surfaces decide. The agent is structurally never in the trust path for approvals. v1 MVP ships with CLI prompt only; web UI, system notifications, biometric, and a hosted Aileron backend are Phase 2.
- [ADR-0010: Failure-Handling Policy](/adr/0010-failure-handling) — Failures are visible and structured; closed-set failure-class taxonomy; idempotency is the default; bounded retry on retriable errors; v1 actions are linear with first-failure-terminates semantics. LLM fallback, conditional compensation, and per-call retry tuning are post-MVP enhancements.

## The sequence

The ten ADRs form a complete architectural ratification of Aileron's v1, tracked in [issue #343](https://github.com/ALRubinger/aileron/issues/343):

1. **Manifest Format Conventions** — *landed ([ADR-0001](/adr/0001-manifest-format))*
2. **Connector Model** — *landed ([ADR-0002](/adr/0002-connector-model))*
3. **Action Model** — *landed ([ADR-0003](/adr/0003-action-model))*
4. **Dependency Resolution** — *landed ([ADR-0004](/adr/0004-dependency-resolution))*
5. **Sandbox Choice** — *landed ([ADR-0005](/adr/0005-sandbox-choice))*
6. **Capability Binding UX** — *landed ([ADR-0006](/adr/0006-capability-binding-ux))*
7. **Install Consent Flow** — *landed ([ADR-0007](/adr/0007-install-consent))*
8. **Intent Matching Mechanisms** — *landed ([ADR-0008](/adr/0008-intent-matching))*
9. **User Channel and OOB Approval Surfaces** — *landed ([ADR-0009](/adr/0009-user-channel))*
10. **Failure-Handling Policy** — *landed ([ADR-0010](/adr/0010-failure-handling))*

Aileron is user-level by construction in v1 — actions live in `~/.aileron/actions/`, not in the project's repo. Project-level shared actions and team-coordination features are post-MVP, deferred until concrete teams ask for them.
9. User channel and OOB approval surfaces
10. Failure-handling policy
11. Project portability

ADRs land in the order above to maximize unblocking — each ADR can reference the formats and primitives ratified by earlier ones.

## How to read these

- Each ADR is a *decision*, not a *design document*. Operational detail is included where it ratifies the decision; broader implementation detail belongs in code and PRs.
- **ADRs are editable until the first working MVP ships.** Real usage will surface assumptions worth revising; until MVP, an ADR is amended in place when its decision changes. Once MVP ships, this section will switch to the standard immutability convention: superseding decisions land as new ADRs that explicitly cite what they replace.
