---
title: "Observability"
description: "Aileron records every consequential decision in a local audit log and emits OpenTelemetry-compatible traces for action execution. This page covers what's emitted, how to enable trace export, and the env vars that control both surfaces."
---

Aileron emits two complementary surfaces of structured data: an **audit log** that's always on, and **OpenTelemetry traces** that are off by default and opt-in. They share attribute keys 1:1, so a span and an audit event for the same operation carry identical names — your trace tooling and your audit reader read the same vocabulary.

The audit log is the *receipt*: durable, local, append-only JSONL on disk that answers "what did the agent do, with what authority, against which service?" The traces are the *flight recorder*: a per-request tree of timed spans that answers "where did latency go, where did errors originate, and how do these calls correlate with the rest of the agent stack?"

If you only want a quick reference for env vars, jump to [Configuration](#configuration).

## The audit log (always on)

Every load-bearing decision in the runtime emits a structured audit record. This is not incidental logging — it's the contract that [Proof of Control](/concepts/proof-of-control/) builds on. The record lives at `~/.aileron/audit.jsonl` (one event per line) and is queryable through the CLI:

```sh
aileron audit list             # newest events first
aileron audit get <audit-id>   # full event by id
```

Today, four families of events land in the log:

- **Install consent** — every connector and action install records artifact FQN, version, hash, signature status, and the user's decision ([ADR-0007](/adr/0007-install-consent)).
- **Action execution** — every invocation records which connector it called, which capability it exercised, and which binding identity satisfied it ([ADR-0003](/adr/0003-action-model), [ADR-0011](/adr/0011-local-credential-vault)). Credential bytes are never recorded.
- **Failure** — every failure surfaces with a stable `class`, `boundary`, retry, and `audit_id` ([ADR-0010](/adr/0010-failure-handling)). The same `audit_id` is stamped onto the agent-visible tool-result envelope, so the LLM's "what went wrong?" reaction can be traced back to a specific event.
- **Approval lifecycle** — `approval.requested`, `approval.approved`, `approval.denied`. Each carries the same `aileron.approval.id` so a request and its decision are trivially correlated.

The schema is durable: every payload field uses the OpenTelemetry-namespaced key shape (`aileron.connector.fqn`, `aileron.binding.name`, `aileron.failure.class`, etc.) so consumers — log shippers, trace tools, custom queries — read the same vocabulary regardless of which surface they came in through.

## OpenTelemetry traces (opt-in)

When tracing is enabled, Aileron starts a server-root span on every request and child spans for the work inside. Spans propagate via [W3C TraceContext](https://www.w3.org/TR/trace-context/), so an inbound `traceparent` header from the calling agent makes Aileron's spans children of the agent's trace — your end-to-end view stays coherent.

To turn it on for local development:

```sh
AILERON_OTEL_ENABLED=true \
AILERON_OTEL_EXPORTER=stdout \
aileron launch claude
```

Spans land on stderr as JSON-per-line. Pipe to `jq` to navigate them. With tracing off (the default), there's zero SDK overhead — the call sites resolve to no-op tracers.

Tracing is independent of the audit log. The audit log answers *what was done*; the traces answer *how it ran*. Both are useful; neither replaces the other.

### What gets emitted

Today (as the Phase 7 emission integrations land slice by slice in [issue #390](https://github.com/ALRubinger/aileron/issues/390)):

| Span name | Where it's emitted | Status |
|---|---|---|
| `aileron.action.execute` | `SandboxExecutor.Execute` — root for an action invocation | ✅ shipped |
| `aileron.connector.call` | per-step `conn.Invoke` inside the executor | ✅ shipped |
| HTTP server-root span | gateway and `/v1/actions/{name}/run` entry points | ✅ shipped (#457) |
| `aileron.capability.check` | the action-boundary capability enforcement | ⏳ pending |
| `aileron.approval.wait` | the approval-queue blocking wait | ⏳ pending |
| `aileron.mcp.tool.call` | `aileron-mcp` outbound to `/v1/actions/{name}/run` | ⏳ pending |
| `aileron.gateway.openai.chat` / `aileron.gateway.anthropic.messages` | LLM round-trip on the gateway | ⏳ pending |

The file exporter (writing spans to `~/.aileron/traces/spans.jsonl` with date-based rotation) is also pending — it's gated on the parallel audit-log file rotation work so spans and audit events share a rotating writer with the same retention story.

### Span attribute schema

Every span carries the OTel-namespaced shape locked-in for the audit payload (PR #452). When you query traces by attribute, you query the same names you'd query the audit log by:

| Attribute | On | Description |
|---|---|---|
| `aileron.action.name` | `aileron.action.execute` | The action manifest name being invoked |
| `aileron.action.steps_count` | `aileron.action.execute` | Number of `[[execute]]` steps in the action |
| `aileron.connector.fqn` | `aileron.connector.call` | Fully-qualified connector identifier (e.g. `github://ALRubinger/aileron-connector-google`) |
| `aileron.connector.op` | `aileron.connector.call` | The connector operation name (e.g. `list_recent_emails`) |
| `aileron.connector.hash` | `aileron.connector.call` | The content-addressed hash of the connector binary |
| `aileron.failure.class` | error spans | Failure taxonomy class (`capability_denied`, `binding_required`, etc.) per [ADR-0010](/adr/0010-failure-handling) |
| `aileron.failure.boundary` | error spans | Where the failure was detected (`action`, `sandbox`, `runtime`) |
| `aileron.failure.retriable` | error spans | Whether the agent should retry |

When a span fails, the OTel span status is also set to `Error` with the failure message — your tracing UI's red flags work without parsing attributes.

## Configuration

All knobs are environment variables read at daemon startup. Defaults reproduce the historic behavior — tracing off, audit on.

| Env var | Default | Effect |
|---|---|---|
| `AILERON_OTEL_ENABLED` | `false` | Master switch for trace emission. When `false`, the SDK is never constructed; the call sites resolve to no-op. The W3C TraceContext propagator is registered regardless, so an inbound `traceparent` is parsed and propagated even without local emission. |
| `AILERON_OTEL_SERVICE_NAME` | `aileron` | The OTel resource attribute `service.name` reported on every span. Set it to disambiguate Aileron from other services in your trace tooling. |
| `AILERON_OTEL_EXPORTER` | `noop` | Exporter selection. `noop` drops spans (the default — same as `AILERON_OTEL_ENABLED=false`). `stdout` writes JSON-per-line to stderr for local development. The file exporter is pending. |
| `AILERON_AUDIT_PATH` | `~/.aileron/audit.jsonl` | Override the audit log location. The default suits the personal-use case; environments with stricter filesystem layouts can point this elsewhere. |

A misconfigured exporter — unknown name, or a known exporter whose construction fails — degrades gracefully to no-op rather than failing daemon startup. The Aileron HTTP server keeps serving when its telemetry sidecar is misconfigured; the failure is logged at warn level so you find it without it taking the daemon down.

## Why both audit and traces

It's a fair question: if attribute keys are identical, why ship two surfaces?

The audit log is **structurally durable**: it's append-only on local disk, with no opt-in toggle, no exporter to misconfigure, and no in-memory batch processor that can drop events on shutdown. It's the *receipt* you can hand a compliance reviewer — and it's the substrate signed audit logs (a [Stage 1.5](/concepts/proof-of-control/) post-MVP item) will eventually live on.

The OpenTelemetry surface is **structurally portable**: spans plug into existing observability infrastructure (Grafana, Datadog, OTLP collectors, etc.) without bespoke integration. When Aileron sits inside an otherwise instrumented agent stack, the trace tree connects across every hop your agent already exports.

Both surfaces share keys precisely because the same operation should look the same regardless of where you read it from. When `aileron.connector.fqn` shows up in your trace tool, it shows up in `aileron audit list` filtered by the same name.
