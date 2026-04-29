---
title: "Architecture Decisions"
description: "Architecture decision records for Aileron"
order: 0
---

This section holds the architecture decision records (ADRs) that define Aileron's architecture. Each ADR captures one decision: the context, the choice, the trade-offs, and the consequences.

## What's landed

- [ADR-0001: Manifest Format Conventions](/adr/0001-manifest-format) — Markdown body + TOML frontmatter for actions; pure TOML for connectors and project config; JSON for runtime IPC.
- [ADR-0002: Connector Model](/adr/0002-connector-model) — Connectors are sandboxed, content-addressed binaries; Aileron core ships only primitive capability types.
- [ADR-0003: Action Model](/adr/0003-action-model) — Atomic actions copied into the project on install; depend on connectors with explicit version + hash + capability subsets; capability enforcement at the action boundary in addition to the connector boundary.

## What's coming

The full sequence is tracked in [issue #343](https://github.com/ALRubinger/aileron/issues/343):

1. **Manifest Format Conventions** — *landed (ADR-0001)*
2. **Connector Model** — *landed (ADR-0002)*
3. **Action Model** — *landed (ADR-0003)*
4. Dependency resolution
5. Sandbox choice
6. Capability binding UX
7. Install consent flow
8. Intent matching mechanisms
9. User channel and OOB approval surfaces
10. Failure-handling policy
11. Project portability

ADRs land in the order above to maximize unblocking — each ADR can reference the formats and primitives ratified by earlier ones.

## How to read these

- Each ADR is a *decision*, not a *design document*. Operational detail is included where it ratifies the decision; broader implementation detail belongs in code and PRs.
- **ADRs are editable until the first working MVP ships.** Real usage will surface assumptions worth revising; until MVP, an ADR is amended in place when its decision changes. Once MVP ships, this section will switch to the standard immutability convention: superseding decisions land as new ADRs that explicitly cite what they replace.
