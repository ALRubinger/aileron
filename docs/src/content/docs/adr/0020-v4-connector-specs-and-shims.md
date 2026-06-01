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

Connector packages publish a machine-readable spec. For the first implementation cut, installed packages MAY include `aileron.connector.v1.json` at the connector package root inside the content-addressed store:

```text
~/.aileron/store/connectors/sha256/<hash>/aileron.connector.v1.json
```

The spec captures the Aileron operation contract directly. Later code generation can derive the same shape from OpenAPI or GraphQL plus Aileron extensions for:

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

Spec-backed operation shims post to the stable daemon API contract:

```text
POST ${AILERON_API_URL%/}/connector-operations/run
```

The request body contains `connector_fqn`, `tool`, `operation`, and raw JSON `args`. The proxy/data-plane implementation behind this contract is deferred to [ADR-0019](/adr/0019-v4-https-data-plane/) and [#896](https://github.com/ALRubinger/aileron/issues/896).

As the first #896 cut, the daemon endpoint resolves installed specs, writes an audit event for recognized connector-operation attempts, and returns `501 not_implemented` until mediated HTTPS execution and credential injection are available. That keeps generated shims fail-closed while giving the proxy/data-plane work a tested API boundary.

The v1 spec requires:

| Field | Requirement |
|---|---|
| `schema_version` | `aileron.connector.v1` |
| `connector.fqn` | valid connector FQN |
| `tools[].name` | unique command name using letters, digits, dots, dashes, underscores, or colons |
| `tools[].operations[].name` | unique operation name per tool using the same restricted character set |
| `tools[].operations[].inputs[].name` | optional, unique input name per operation using the same restricted character set |
| `tools[].operations[].audit[].name` | optional, unique audit field name per operation using the same restricted character set |

Optional fields include operation `summary`, `description`, `method`, `path`, `idempotency`, `approval`, `credential`, `inputs`, and `audit`. They feed shim help text now and policy/audit matching as the data plane lands.

## Consequences

Tool discovery stays cheap: the agent sees a small file and familiar command names rather than a growing MCP catalog.

Connector authors own API semantics in a spec, not in hand-written prompt text. Aileron owns validation, generated shim behavior, and conflict handling.

Name conflicts must be deterministic. The first implementation rejects conflicts with actionable errors when two installed specs resolve to the same tool name, when a spec-backed shim conflicts with an action-derived shim, or when a spec-backed shim conflicts with the selected agent command.

## Alternatives Considered

**Hand-written shims per connector.** Rejected as the default because it duplicates validation and help rendering logic and makes behavior inconsistent across connectors.

**Expose every operation through MCP.** Rejected for the v4 sandbox path because it grows the prompt/tool catalog with installed connector count and keeps MCP as the central runtime model.

## References

- [Issue #895](https://github.com/ALRubinger/aileron/issues/895) — connector specs and generated shims
- [Issue #893](https://github.com/ALRubinger/aileron/issues/893) — code generation
- [ADR-0019](/adr/0019-v4-https-data-plane) — HTTPS data-plane mediation
