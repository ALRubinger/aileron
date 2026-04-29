---
title: "Architecture Decisions"
description: "Post-Pivot architecture decision records for Aileron"
order: 0
---

This section holds the architecture decision records (ADRs) that ratify Aileron's post-Pivot architecture. Each ADR fills in the trade-offs, alternatives considered, and operational details for a single architectural commitment.

The pre-Pivot ADRs (0001 through 0020) are preserved at [`/archived-adr/`](/archived-adr/) as immutable historical record. The numbering here starts fresh from `0001`.

## What's landed

- [ADR-0001: Manifest Format Conventions](/adr/0001-manifest-format) — Markdown body + TOML frontmatter for actions; pure TOML for connectors and project config; JSON for runtime IPC.

## What's coming

The full sequence of post-Pivot ADRs is tracked in [issue #343](https://github.com/ALRubinger/aileron/issues/343):

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
- The architectural *posture* is captured in the post-Pivot strategy materials archived under [`docs/archive/architecture/`](https://github.com/ALRubinger/aileron/tree/main/docs/archive/architecture). Each ADR cites the corresponding archive page.
- ADRs are immutable once merged. Superseding decisions land as new ADRs that explicitly cite what they replace.
