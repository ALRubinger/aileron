---
title: "Observability"
description: "Aileron records every consequential decision in a local audit log and emits OpenTelemetry-compatible traces for action execution. This page covers what's emitted, how to hook the trace stream up to a collector, and the env vars that control both surfaces."
---

When an agent sends an email, files a ticket, or posts to a channel on your behalf, the questions that matter come *after* the fact: what did it do, did the call succeed, was it slow, and where did it go wrong when it did? Answering those reliably — across runs, machines, incidents, and weeks later when you're trying to reconstruct what happened — is what's called *observability*. For software that takes consequential actions, it isn't optional infrastructure.

Aileron records two complementary surfaces of structured data so those questions have answers. The **audit log** is the durable record of *what was done*: every install consent, every action invocation, every approval decision, every failure — append-only on local disk, queryable through the CLI, intended to outlive the daemon. **Traces** are the per-request timing tree showing *how each invocation ran*: which steps took how long, how nested calls fit together, where errors originated, and how Aileron's spans connect to the rest of an instrumented stack.

The audit log is always on; traces are off by default and opt-in via [OpenTelemetry](https://opentelemetry.io/), the open standard for distributed tracing. Both surfaces share attribute keys exactly, so a span and an audit event for the same operation read the same names — your trace tooling and your audit reader speak the same vocabulary.

If you only want a quick reference for env vars, jump to [Configuration](#configuration). If you already have an OTel collector running and just want to point Aileron at it, jump to [Hooking up to a collector](#hooking-up-to-a-collector).

## What is OpenTelemetry?

[OpenTelemetry](https://opentelemetry.io/) (OTel) is vendor-neutral: instrument your service once against the OTel SDK, and any compatible backend — Grafana, Datadog, Jaeger, Honeycomb, Tempo, New Relic — can consume the data. Aileron emits spans the same way any other OTel-instrumented Go service does; if you've used OTel before, the shape is familiar.

The terms that show up on the rest of this page:

- **Span** — a timed unit of work with a name, attributes (key/value tags), and a parent. A *trace* is the tree of spans for one logical request.
- **`traceparent`** — the [W3C TraceContext](https://www.w3.org/TR/trace-context/) HTTP header that carries trace + parent-span IDs across service boundaries so multi-service requests stay connected end-to-end, regardless of which language or framework each service is written in.
- **OTLP** — the OpenTelemetry Protocol, the wire format collectors expect.
- **OTel endpoint** — the URL of an [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) (or a managed backend's ingest URL) that receives OTLP-encoded spans. The collector fans spans out to whichever backend you've configured — multiple backends, no per-language SDK churn.
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

When tracing is enabled, Aileron starts a server-root span on every request and child spans for the work inside — action execution, connector calls, capability checks, approval waits, the augmentation/interception loop on the LLM gateway. Spans propagate via [W3C TraceContext](https://www.w3.org/TR/trace-context/), so an inbound `traceparent` header from the calling agent makes Aileron's spans children of the agent's trace — your end-to-end view stays coherent. With tracing off (the default), there's zero SDK overhead — the call sites resolve to no-op tracers. The W3C propagator is installed regardless, so an inbound `traceparent` is parsed and forwarded even when this process emits nothing.

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
| `aileron.intercept.round` | per round of the augmentation/interception loop when actions are installed |
| `aileron.action.execute` | `SandboxExecutor.Execute` — root for an action invocation |
| `aileron.capability.check` | per-step action-boundary capability enforcement (defense-in-depth, [ADR-0003](/adr/0003-action-model)) |
| `aileron.connector.call` | per-step `conn.Invoke` inside the executor |
| `aileron.approval.wait` | the approval-queue blocking wait — covers the entire user-decision interval |
| HTTP server-root span | other API entry points (`/v1/audit`, `/v1/bindings`, etc.) — generic "METHOD /path" naming |

### Span attribute schema

Every span carries the OTel-namespaced shape locked-in for the audit payload. When you query traces by attribute, you query the same names you'd query the audit log by — this table is the source of truth for what's available.

**Action execution** (`aileron.action.execute`):

| Attribute | Description |
|---|---|
| `aileron.action.name` | The action manifest name being invoked |
| `aileron.action.steps_count` | Number of `[[execute]]` steps in the action |

**Capability check** (`aileron.capability.check`):

| Attribute | Description |
|---|---|
| `aileron.action.name` | The action whose subset is being enforced |
| `aileron.connector.fqn` | The connector the step targets |
| `aileron.capability.kind` | The op the action is attempting (treated as the capability string per [ADR-0003](/adr/0003-action-model)) |

**Connector call** (`aileron.connector.call`):

| Attribute | Description |
|---|---|
| `aileron.connector.fqn` | Fully-qualified connector identifier (e.g. `github://ALRubinger/aileron-connector-google`) |
| `aileron.connector.op` | The connector operation name (e.g. `list_recent_emails`) |
| `aileron.connector.hash` | The content-addressed hash of the connector binary |

**Intercept round** (`aileron.intercept.round`):

| Attribute | Description |
|---|---|
| `aileron.intercept.round_index` | 0-based, monotonic per request |
| `aileron.intercept.protocol` | `openai` or `anthropic` |
| `aileron.intercept.tool_calls_count` | Number of Aileron tool calls in this round (set when present) |
| `aileron.intercept.terminal` | `true` when the round produced the final assistant message |
| `aileron.intercept.upstream_status` | HTTP status from the upstream LLM when non-200 |

**Approval wait** (`aileron.approval.wait`):

| Attribute | Description |
|---|---|
| `aileron.approval.id` | Correlation key — same id as the `approval.requested` / `.approved` / `.denied` audit events |
| `aileron.approval.kind` | `action` / `comms_send` / `comms_draft` / `http_request` / `shell` |
| `aileron.approval.action` | The action-or-tool name the gate covers |
| `aileron.approval.decision` | `approved` / `denied` / `timeout` / `cancelled` |
| `aileron.approval.wait_ms` | Time from `RequestedAt` to `DecidedAt`, in milliseconds (set on resolved outcomes) |
| `aileron.approval.edited` | `true` when the user edited the payload before approving |
| `aileron.approval.reason` | Free-text reason (set on denials, when supplied) |
| `aileron.connector.fqn` | Set when the gated action targets a specific connector |
| `aileron.session.id` | Set when the request came in under a launch session |

**Failure (any error span)** — the closed taxonomy from [ADR-0010](/adr/0010-failure-handling):

| Attribute | Description |
|---|---|
| `aileron.failure.class` | Failure taxonomy class (`capability_denied`, `binding_required`, etc.) |
| `aileron.failure.boundary` | Where the failure was detected (`action`, `sandbox`, `runtime`) |
| `aileron.failure.retriable` | Whether the agent should retry |
| `aileron.audit.id` | The audit event id stamped onto the failure envelope, so a span and an audit record can be cross-referenced |

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
