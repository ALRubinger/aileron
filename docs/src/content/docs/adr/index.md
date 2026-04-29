---
title: "Architecture Decisions"
description: "Architecture decision records for Aileron"
order: 0
---

This section holds the architecture decision records (ADRs) that define Aileron's architecture. Each ADR captures one decision: the context, the choice, the trade-offs, and the consequences.

## What's landed

- [ADR-0001: Manifest Format Conventions](/adr/0001-manifest-format) — Markdown body + TOML frontmatter for actions; pure TOML for connectors and project config; JSON for runtime IPC.

## What's coming

The full sequence is tracked in [issue #343](https://github.com/ALRubinger/aileron/issues/343):

1. **Manifest Format Conventions** — *landed (this ADR-0001)*
2. Connector model
3. Action model
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
- ADRs are immutable once merged. Superseding decisions land as new ADRs that explicitly cite what they replace.
