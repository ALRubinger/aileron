---
title: "Observability"
description: "Aileron records every consequential decision in a local audit log and emits OpenTelemetry-compatible traces for action execution. This page covers what's emitted, how to hook the trace stream up to a collector, and the env vars that control both surfaces."
---

Aileron emits two complementary surfaces of structured data: an **audit log** that's always on, and **OpenTelemetry traces** that are off by default and opt-in. They share attribute keys 1:1, so a span and an audit event for the same operation carry identical names — your trace tooling and your audit reader read the same vocabulary.

The audit log is the *receipt*: durable, local, append-only JSONL on disk that answers "what did the agent do, with what authority, against which service?" The traces are the *flight recorder*: a per-request tree of timed spans that answers "where did latency go, where did errors originate, and how do these calls correlate with the rest of the agent stack?"

If you only want a quick reference for env vars, jump to [Configuration](#configuration). If you already have an OTel collector running and just want to point Aileron at it, jump to [Hooking up to a collector](#hooking-up-to-a-collector).

## What is OpenTelemetry?

[OpenTelemetry](https://opentelemetry.io/) (OTel) is the vendor-neutral industry standard for distributed tracing, metrics, and logs — the substrate Grafana, Datadog, Jaeger, Honeycomb, New Relic, and most modern observability tools agree on. A *trace* is a tree of *spans*; each span is a timed unit of work with a name, attributes (key/value tags), a status, and a parent span. The [W3C TraceContext](https://www.w3.org/TR/trace-context/) spec defines a `traceparent` HTTP header that carries the trace ID and parent span ID across service boundaries so a multi-service request stays connected end-to-end, regardless of which language or framework each service is written in.

The wire format collectors expect is **OTLP** (OpenTelemetry Protocol). When Aileron's OTLP exporter is enabled you point it at an **OTel endpoint** — typically the URL of an [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) deployed alongside your other services — and the collector fans your spans out to whichever backend you've configured. Multiple backends, no per-language SDK churn: that's the value OTel buys you.

Aileron's role here is thin and consistent: when the agent calls Aileron, Aileron starts spans for the work it does (action execution, connector calls, capability checks, approval waits), parents them to the agent's incoming `traceparent` if one was passed, and emits them through a configurable exporter. The spans plug into your existing observability stack without bespoke integration — Aileron looks like any other instrumented service in the trace tree. If you don't have an observability practice yet, the **stdout** and **file** exporters described below work as development aids.

A few terms you'll see throughout this page:

- **Exporter** — the component that ships spans out of the process. Aileron supports `noop` (the default — drops spans, zero overhead), `stdout` (writes JSON-per-line to stderr for local development), `file` (writes JSON-per-line to a daily-rotated file under `~/.aileron/traces/`), and `otlp` (ships to a collector via OTLP/HTTP).
- **Span status** — `Ok` (default), `Error`, or `Unset`. Aileron sets `Error` on any span whose underlying operation failed, with the failure message as the status description.
- **Resource** — process-level metadata attached to every span. Aileron sets `service.name=aileron` (configurable via `AILERON_OTEL_SERVICE_NAME`).

## The audit log (always on)

Every load-bearing decision in the runtime emits a structured audit record. This is not incidental logging — it's the contract that [Proof of Control](/concepts/proof-of-control/) builds on. The records live as daily-rotated JSONL files at `~/.aileron/audit/audit-YYYY-MM-DD.jsonl` and are queryable through the CLI:

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

When tracing is enabled, Aileron starts a server-root span on every request and child spans for the work inside. Spans propagate via [W3C TraceContext](https://www.w3.org/TR/trace-context/), so an inbound `traceparent` header from the calling agent makes Aileron's spans children of the agent's trace — your end-to-end view stays coherent. With tracing off (the default), there's zero SDK overhead — the call sites resolve to no-op tracers. The W3C propagator is installed regardless, so an inbound `traceparent` is parsed and forwarded even when this process emits nothing.

### Three ways to consume traces

**`stdout`** — for local debugging. Spans land on stderr as JSON-per-line. Pipe to `jq`:

```sh
AILERON_OTEL_ENABLED=true \
AILERON_OTEL_EXPORTER=stdout \
aileron launch claude
```

**`file`** — for durable retention across sessions, mirroring the audit log's on-disk layout:

```sh
AILERON_OTEL_ENABLED=true \
AILERON_OTEL_EXPORTER=file \
aileron launch claude
# spans → ~/.aileron/traces/spans-YYYY-MM-DD.jsonl
```

A new file is created per local-clock day; a session that crosses midnight rolls naturally to the next day's file. `AILERON_TRACES_DIR` overrides the state directory; the default (`~/.aileron`) keeps audit and traces side-by-side.

**`otlp`** — production. Ships spans to an OpenTelemetry Collector via OTLP/HTTP. See the next section.

### Hooking up to a collector

The OTLP exporter honors the [standard OTel environment variables](https://opentelemetry.io/docs/specs/otel/protocol/exporter/) that every OTel-instrumented service in your stack already understands. There's no Aileron-prefixed alternative — forking the names would force you to maintain two parallel sets.

Stand up a collector locally for development:

```sh
docker run --rm -p 4318:4318 \
  otel/opentelemetry-collector-contrib:latest
```

Then point Aileron at it:

```sh
AILERON_OTEL_ENABLED=true \
AILERON_OTEL_EXPORTER=otlp \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
OTEL_EXPORTER_OTLP_INSECURE=true \
aileron launch claude
```

For a managed backend, point at its ingest endpoint and pass auth via `OTEL_EXPORTER_OTLP_HEADERS`:

```sh
# Honeycomb
AILERON_OTEL_ENABLED=true \
AILERON_OTEL_EXPORTER=otlp \
OTEL_EXPORTER_OTLP_ENDPOINT=https://api.honeycomb.io \
OTEL_EXPORTER_OTLP_HEADERS=x-honeycomb-team=YOUR_API_KEY \
aileron launch claude

# Grafana Cloud
AILERON_OTEL_ENABLED=true \
AILERON_OTEL_EXPORTER=otlp \
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-us-central-0.grafana.net/otlp \
OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic <base64(instanceID:token)>" \
aileron launch claude
```

Recognised env vars (handled by the OTel SDK directly):

- `OTEL_EXPORTER_OTLP_ENDPOINT` — collector URL. Defaults to `http://localhost:4318`.
- `OTEL_EXPORTER_OTLP_HEADERS` — comma-separated `k=v` pairs added to every export request. Use this for API keys.
- `OTEL_EXPORTER_OTLP_INSECURE` — set to `true` to allow plain HTTP (development only).
- `OTEL_EXPORTER_OTLP_TIMEOUT` — request timeout, default 10s.

The full set is in the [OTel exporter spec](https://opentelemetry.io/docs/specs/otel/protocol/exporter/).

### What gets emitted

| Span name | Where it's emitted |
|---|---|
| `aileron.mcp.tool.call` | `aileron-mcp` outbound to `/v1/actions/{name}/run` — typically the trace root under `aileron launch` |
| `aileron.gateway.openai.chat` / `aileron.gateway.anthropic.messages` | LLM round-trip on the gateway endpoints |
| `aileron.intercept.round` | per round of the augmentation/interception loop when actions are installed; carries `aileron.intercept.{round_index,protocol,tool_calls_count,terminal}` |
| `aileron.action.execute` | `SandboxExecutor.Execute` — root for an action invocation |
| `aileron.capability.check` | per-step action-boundary capability enforcement (defense-in-depth, [ADR-0003](/adr/0003-action-model)) |
| `aileron.connector.call` | per-step `conn.Invoke` inside the executor |
| `aileron.approval.wait` | the approval-queue blocking wait — covers the entire user-decision interval; `aileron.approval.decision` is `approved` / `denied` / `timeout` / `cancelled` |
| HTTP server-root span | other API entry points (`/v1/audit`, `/v1/bindings`, etc.) — generic "METHOD /path" naming |

### Span attribute schema

Every span carries the OTel-namespaced shape locked-in for the audit payload. When you query traces by attribute, you query the same names you'd query the audit log by:

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

All Aileron-side knobs are environment variables read at daemon startup. Defaults reproduce the historic behavior — tracing off, audit on. The `OTEL_EXPORTER_OTLP_*` family is consumed directly by the OTel SDK and only matters when `AILERON_OTEL_EXPORTER=otlp`.

| Env var | Default | Effect |
|---|---|---|
| `AILERON_OTEL_ENABLED` | `false` | Master switch for trace emission. When `false`, the SDK is never constructed; call sites resolve to no-op. The W3C TraceContext propagator is registered regardless, so an inbound `traceparent` is parsed and propagated even without local emission. |
| `AILERON_OTEL_SERVICE_NAME` | `aileron` | The OTel resource attribute `service.name` reported on every span. Set it to disambiguate Aileron from other services in your trace tooling. |
| `AILERON_OTEL_EXPORTER` | `noop` | Exporter selection: `noop` (drop), `stdout` (stderr JSON-per-line for dev), `file` (daily-rotated JSONL under `AILERON_TRACES_DIR`), `otlp` (ship to a collector via OTLP/HTTP). |
| `AILERON_TRACES_DIR` | `~/.aileron` | State directory for the `file` exporter. Spans land at `<dir>/traces/spans-YYYY-MM-DD.jsonl`. Setting this to an explicit empty string disables the file exporter (degrades to no-op). |
| `AILERON_AUDIT_DIR` | `~/.aileron` | State directory for the audit log. Audit events land at `<dir>/audit/audit-YYYY-MM-DD.jsonl`. Setting this to an explicit empty string falls back to the in-memory store (events lost on daemon restart). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4318` | Collector URL. Used when `AILERON_OTEL_EXPORTER=otlp`. |
| `OTEL_EXPORTER_OTLP_HEADERS` | (none) | Comma-separated `k=v` pairs added to every export request. Use for API keys (`x-honeycomb-team=...`, `Authorization=Basic ...`). |
| `OTEL_EXPORTER_OTLP_INSECURE` | `false` | Set to `true` to allow plain HTTP. Development-only. |

A misconfigured exporter — unknown name, or a known exporter whose construction fails — degrades gracefully to no-op rather than failing daemon startup. The Aileron HTTP server keeps serving when its telemetry sidecar is misconfigured; the failure is logged at warn level so you find it without it taking the daemon down.

## Why both audit and traces

It's a fair question: if attribute keys are identical, why ship two surfaces?

The audit log is **structurally durable**: it's append-only on local disk, with no opt-in toggle, no exporter to misconfigure, and no in-memory batch processor that can drop events on shutdown. It's the *receipt* you can hand a compliance reviewer — and it's the substrate signed audit logs (a [Stage 1.5](/concepts/proof-of-control/) post-MVP item) will eventually live on.

The OpenTelemetry surface is **structurally portable**: spans plug into existing observability infrastructure without bespoke integration. When Aileron sits inside an otherwise instrumented agent stack, the trace tree connects across every hop your agent already exports.

Both surfaces share keys precisely because the same operation should look the same regardless of where you read it from. When `aileron.connector.fqn` shows up in your trace tool, it shows up in `aileron audit list` filtered by the same name.
