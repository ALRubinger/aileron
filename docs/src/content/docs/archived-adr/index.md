---
title: "Archived ADRs"
description: "Pre-Pivot architecture decision records, preserved as immutable historical context"
order: 0
---

This section preserves the architecture decision records (ADRs `0001` through `0020`) that captured Aileron's design before the [strategic pivot](/pivot/overview) to a deterministic execution layer for AI agents. They remain here as immutable historical record.

## Why these are archived, not deleted

The pre-Pivot architecture was load-bearing in its time. It explored:

- The MCP gateway model (`0001`, `0002`, `0008`)
- A marketplace UI for MCP server distribution (`0003`)
- Pre-Pivot enterprise auth and OAuth callback shape (`0004`, `0005`, `0006`, `0007`)
- The original deterministic-execution-plane formalization (`0009`)
- The zero-knowledge vault trust model (`0010`, `0020`)
- TEE provider SPI and Confidential Space (`0011`)
- Auto-escrow and decoupled session lifetimes (`0012`)
- The local policy-enforced shell (`0013`)
- Aileron YAML policy schema, built-in defaults, and layer overrides (`0014`, `0015`, `0016`)
- Pluggable agent SPI (`0017`)
- Context store and read-write boundary model (`0018`, `0019`)

The Pivot reframed Aileron around a single architecturally-load-bearing claim: **the LLM endpoint is the only seam where deterministic execution, sealed credentials, and tamper-resistant approval can coexist.** That reframe substantially supersedes much of what these ADRs decided. Some load-bearing primitives — the vault trust model, the policy schema, the TEE provider story — remain relevant to Aileron Control and the enterprise tier. Others — the MCP gateway model, the marketplace UI, the original execution-plane formalization — are now superseded.

Per the project's ADR immutability rule, none of these are edited. Instead, the live [`Architecture Decisions`](/pivot/architecture/) section (currently empty) is being repopulated with fresh post-Pivot ADRs that ratify the commitments stated in the Pivot architecture pages. That work is tracked in [issue #343](https://github.com/ALRubinger/aileron/issues/343); the archival itself is tracked in issue #335 (Phase 5).

## How to read this archive

- **For historical context:** treat each ADR as a snapshot of what Aileron's architecture was at the time it was decided. The trade-off analysis, alternatives considered, and rejection rationale remain useful even where the conclusion has been superseded.
- **As input for the new ADRs:** the post-Pivot ADRs (in [`/adr/`](/pivot/architecture/) once they land) reference these archives where the new decision draws on, supersedes, or refutes a legacy one.
- **Not as current architecture:** if you're trying to understand how Aileron works today, start at [Pivot Overview](/pivot/overview), not here.

## The archived ADRs

The 20 ADRs are listed in the sidebar. They are numbered `0001` through `0020`; the live `Architecture Decisions` section will renumber from `0001` for the post-Pivot sequence — the legacy numbering belongs to this archive.
