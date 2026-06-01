---
title: "ADR-0020: v4 Connector Specs and Generated HTTPS Shims"
description: "Connector packages provide machine-readable operation specs; Aileron uses them to render discovery and generated HTTPS shims for the sandbox runtime."
order: 20
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Proposed</td></tr>
  <tr><th>Date</th><td>2026-06-01</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/895">#895</a>, <a href="https://github.com/ALRubinger/aileron/issues/893">#893</a></td></tr>
</table>
</div>

## Context

The first sandbox runtime cut generates static `tools.txt` and connector shim scripts from installed action manifests. That proves the discovery and execution shape, but the v4 runtime needs a durable connector package contract for richer operations, help text, approval metadata, idempotency, and audit shape.

## Decision

Connector packages publish a machine-readable spec. The expected shape is OpenAPI or GraphQL plus Aileron extensions for:

- operation identity and stable action names
- credential kind and scope
- approval preview / approval policy hints
- idempotency and retry metadata
- audit fields and redaction rules
- CLI/help rendering metadata

Aileron uses installed specs and action manifests to render:

- `/etc/aileron/tools.txt`
- per-tool `--help`
- generated HTTPS shims
- proxy/data-plane matching metadata

Generated shims are thin HTTPS clients. They call `AILERON_API_URL` for explicit installed-action dispatch today and can call through the v4 HTTPS data plane as that layer lands.

## Consequences

Tool discovery stays cheap: the agent sees a small file and familiar command names rather than a growing MCP catalog.

Connector authors own API semantics in a spec, not in hand-written prompt text. Aileron owns validation, generated shim behavior, and conflict handling.

Name conflicts must be deterministic. The implementation should either reject conflicts with actionable errors or apply documented namespacing.

## Alternatives Considered

**Hand-written shims per connector.** Rejected as the default because it duplicates validation and help rendering logic and makes behavior inconsistent across connectors.

**Expose every operation through MCP.** Rejected for the v4 sandbox path because it grows the prompt/tool catalog with installed connector count and keeps MCP as the central runtime model.

## References

- [Issue #895](https://github.com/ALRubinger/aileron/issues/895) — connector specs and generated shims
- [Issue #893](https://github.com/ALRubinger/aileron/issues/893) — code generation
- [ADR-0019](/adr/0019-v4-https-data-plane) — HTTPS data-plane mediation
