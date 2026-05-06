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

### What this gives you

[OpenTelemetry](https://opentelemetry.io/) (OTel) is the vendor-neutral industry standard for distributed tracing, metrics, and logs — the substrate Grafana, Datadog, Jaeger, Honeycomb, and most modern observability tools agree on. A *trace* is a tree of *spans*; each span is a timed unit of work with a name, attributes (key/value tags), a status, and a parent. The [W3C TraceContext](https://www.w3.org/TR/trace-context/) spec defines a `traceparent` HTTP header that carries the trace ID and parent span ID across service boundaries so a multi-service request stays connected end-to-end, regardless of which language or framework each service is written in.

Aileron's role here is thin and consistent: when the agent calls Aileron, Aileron starts spans for the work it does (action execution, connector calls, capability checks, approval waits), parents them to the agent's incoming `traceparent` if one was passed, and emits them through a configurable exporter. The spans plug into your existing observability stack without bespoke integration — Aileron looks like any other instrumented service in the trace tree. If you don't have an observability practice yet, the **stdout exporter** described below works as a development aid: pipe the JSON-per-line output through `jq` to inspect what the runtime is doing.

A few terms you'll see:

- **Exporter** — the component that ships spans out of the process. Aileron supports `noop` (the default — drops spans, zero overhead), `stdout` (writes JSON-per-line to stderr for local development), and `file` (writes JSON-per-line to a daily-rotated file under `~/.aileron/traces/`, sibling to the audit log). An OTLP exporter is pending.
- **OTLP** — the OpenTelemetry Protocol, the wire format collectors expect. When Aileron's OTLP exporter ships, you'll point it at an **OTel endpoint** — typically the URL of an [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) deployed alongside your other services, which fans the spans out to whichever backend (Grafana, Datadog, etc.) you've configured.
- **Span status** — `Ok` (default), `Error`, or `Unset`. Aileron sets `Error` on any span whose underlying operation failed, with the failure message as the status description.

### Enabling tracing

When tracing is enabled, Aileron starts a server-root span on every request and child spans for the work inside. Spans propagate via [W3C TraceContext](https://www.w3.org/TR/trace-context/), so an inbound `traceparent` header from the calling agent makes Aileron's spans children of the agent's trace — your end-to-end view stays coherent.

To turn it on for local development:

```sh
AILERON_OTEL_ENABLED=true \
AILERON_OTEL_EXPORTER=stdout \
aileron launch claude
```

Spans land on stderr as JSON-per-line. Pipe to `jq` to navigate them. With tracing off (the default), there's zero SDK overhead — the call sites resolve to no-op tracers.

For durable retention across sessions, use the **file** exporter — spans land in a daily-rotated file under `~/.aileron/traces/`, mirroring the audit log's `~/.aileron/audit/` layout:

```sh
AILERON_OTEL_ENABLED=true \
AILERON_OTEL_EXPORTER=file \
aileron launch claude
```

A new file is created per local-clock day (`spans-YYYY-MM-DD.jsonl`); a session that crosses midnight rolls naturally to the next day's file. `AILERON_TRACES_DIR` overrides the state directory; the default (`~/.aileron`) keeps audit and traces side-by-side.

Tracing is independent of the audit log. The audit log answers *what was done*; the traces answer *how it ran*. Both are useful; neither replaces the other.

### What gets emitted

Today (as the Phase 7 emission integrations land slice by slice in [issue #390](https://github.com/ALRubinger/aileron/issues/390)):

| Span name | Where it's emitted | Status |
|---|---|---|
| `aileron.mcp.tool.call` | `aileron-mcp` outbound to `/v1/actions/{name}/run` — typically the trace root under `aileron launch` | ✅ shipped |
| `aileron.action.execute` | `SandboxExecutor.Execute` — root for an action invocation | ✅ shipped |
| `aileron.connector.call` | per-step `conn.Invoke` inside the executor | ✅ shipped |
| `aileron.capability.check` | per-step action-boundary capability enforcement (defense-in-depth, [ADR-0003](/adr/0003-action-model)) — first observability point on "how often does the sandbox say no?" | ✅ shipped |
| `aileron.approval.wait` | the approval-queue blocking wait — covers the entire user-decision interval; `aileron.approval.decision` is `approved` / `denied` / `timeout` / `cancelled` | ✅ shipped |
| HTTP server-root span | gateway and `/v1/actions/{name}/run` entry points | ✅ shipped |
| `aileron.gateway.openai.chat` / `aileron.gateway.anthropic.messages` | LLM round-trip on the gateway | ⏳ pending |

The file exporter is shipped — spans land in `~/.aileron/traces/spans-YYYY-MM-DD.jsonl`, sibling to the audit log's `~/.aileron/audit/audit-YYYY-MM-DD.jsonl`. Both surfaces share the path-naming convention via the `internal/dailypath` package, so retention and rotation are consistent across the two on-disk surfaces.

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
| `AILERON_OTEL_EXPORTER` | `noop` | Exporter selection. `noop` drops spans (the default — same as `AILERON_OTEL_ENABLED=false`). `stdout` writes JSON-per-line to stderr for local development. `file` writes JSON-per-line to a daily-rotated file under `AILERON_TRACES_DIR`. |
| `AILERON_TRACES_DIR` | `~/.aileron` | State directory for the `file` exporter. Spans land at `<dir>/traces/spans-YYYY-MM-DD.jsonl`. The default keeps traces and the audit log side-by-side under `~/.aileron/`. Setting this to an explicit empty string disables the file exporter (degrades to no-op). |
| `AILERON_AUDIT_DIR` | `~/.aileron` | State directory for the audit log. Audit events land at `<dir>/audit/audit-YYYY-MM-DD.jsonl`. The default keeps the audit log alongside traces. Setting this to an explicit empty string falls back to the in-memory store (events lost on daemon restart). |

A misconfigured exporter — unknown name, or a known exporter whose construction fails — degrades gracefully to no-op rather than failing daemon startup. The Aileron HTTP server keeps serving when its telemetry sidecar is misconfigured; the failure is logged at warn level so you find it without it taking the daemon down.

## Why both audit and traces

It's a fair question: if attribute keys are identical, why ship two surfaces?

The audit log is **structurally durable**: it's append-only on local disk, with no opt-in toggle, no exporter to misconfigure, and no in-memory batch processor that can drop events on shutdown. It's the *receipt* you can hand a compliance reviewer — and it's the substrate signed audit logs (a [Stage 1.5](/concepts/proof-of-control/) post-MVP item) will eventually live on.

The OpenTelemetry surface is **structurally portable**: spans plug into existing observability infrastructure (Grafana, Datadog, OTLP collectors, etc.) without bespoke integration. When Aileron sits inside an otherwise instrumented agent stack, the trace tree connects across every hop your agent already exports.

Both surfaces share keys precisely because the same operation should look the same regardless of where you read it from. When `aileron.connector.fqn` shows up in your trace tool, it shows up in `aileron audit list` filtered by the same name.
