---
title: "Observability"
description: "Aileron records every consequential decision in a local audit log and emits OpenTelemetry-compatible traces for action execution. This page covers what's emitted, how to hook the trace stream up to a collector, and the env vars that control both surfaces."
---

When an agent sends an email, files a ticket, or posts to a channel on your behalf, the questions that matter come *after* the fact: what did it do, did the call succeed, was it slow, and where did it go wrong when it did? Answering those reliably is what's called [*observability*](https://opentelemetry.io/docs/concepts/observability-primer/). The same answers have to hold across runs, across machines, and weeks later when you're trying to reconstruct what happened. For software that takes consequential actions, observability isn't optional infrastructure.

Aileron records two complementary surfaces of structured data so those questions have answers. The **audit log** is the durable record of *what was done*. It captures every install consent, every action invocation, every approval decision, and every failure. The log is append-only on local disk, queryable through the CLI, and intended to outlive the daemon. **Traces** are the per-request timing tree showing *how each invocation ran*. Each trace says which steps took how long, how nested calls fit together, where errors originated, and how Aileron's spans connect to the rest of an instrumented stack.

For actions that touch money, send messages that can't be unsent, or grant access, the audit log is the receipt that demonstrates what the agent actually did. That's [Proof of Control](/concepts/proof-of-control/). The log is self-verifiable to you today, with a forward path to independently and cryptographically verifiable as Aileron's deployment story evolves. The durability properties below (append-only, daily-rotated, surviving daemon restart) exist because the receipt has to outlast the runtime that wrote it.

The audit log is always on. Traces are off by default and opt-in via [OpenTelemetry](https://opentelemetry.io/), the open standard for distributed tracing. Both surfaces share attribute keys exactly, so a span and an audit event for the same operation read the same names. Your trace tooling and your audit reader speak the same vocabulary.

If you only want a quick reference for env vars, jump to [Configuration](#configuration). If you already have an OTel collector running and just want to point Aileron at it, jump to [Hooking up to a collector](#hooking-up-to-a-collector).

## What is OpenTelemetry?

[OpenTelemetry](https://opentelemetry.io/) (OTel) is vendor-neutral. Instrument your service once against the OTel SDK, and any compatible backend can consume the data. That includes Grafana, Datadog, Jaeger, Honeycomb, Tempo, and New Relic. Aileron emits spans the same way any other OTel-instrumented Go service does. If you've used OTel before, the shape is familiar.

The terms that show up on the rest of this page:

- **Span:** A timed unit of work with a name, attributes (key/value tags), and a parent. A *trace* is the tree of spans for one logical request.
- **`traceparent`:** The [W3C TraceContext](https://www.w3.org/TR/trace-context/) HTTP header that carries trace and parent-span IDs across service boundaries. It keeps multi-service requests connected end-to-end, regardless of which language or framework each service is written in.
- **OTLP:** The OpenTelemetry Protocol, the wire format collectors expect.
- **OTel endpoint:** The URL of an [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) (or a managed backend's ingest URL) that receives OTLP-encoded spans. The collector fans spans out to whichever backend you've configured. Multiple backends, no per-language SDK churn.
- **Exporter:** The component that ships spans out of the process. Aileron supports `noop` (the default; drops spans, zero overhead), `stdout` (writes JSON-per-line to stderr for local development), `file` (writes JSON-per-line to a daily-rotated file under `~/.aileron/traces/`), and `otlp` (ships to a collector via OTLP/HTTP).
- **Span status:** `Ok` (default), `Error`, or `Unset`. Aileron sets `Error` on any span whose underlying operation failed, with the failure message as the status description.
- **Resource:** Process-level metadata attached to every span. Aileron sets `service.name=aileron` (configurable via `AILERON_OTEL_SERVICE_NAME`).

## The audit log (always on)

Every load-bearing decision in the runtime emits a structured audit record. The audit log is the contract that [Proof of Control](/concepts/proof-of-control/) builds on. The records live as daily-rotated JSONL files at `~/.aileron/audit/audit-YYYY-MM-DD.jsonl` and are queryable through the CLI:

```sh
aileron audit list             # newest events first
aileron audit get <audit-id>   # full event by id
```

Today, five families of events land in the log:

- **Install consent:** Every connector and action install records artifact FQN, version, hash, signature status, and the user's decision ([ADR-0007](/adr/0007-install-consent)).
- **Action execution:** Every invocation records which connector it called, which capability it exercised, and which binding identity satisfied it ([ADR-0003](/adr/0003-action-model), [ADR-0011](/adr/0011-local-credential-vault)). Credential bytes are never recorded.
- **Failure:** Every failure surfaces with a stable `class`, `boundary`, retry, and `audit_id` ([ADR-0010](/adr/0010-failure-handling)). The same `audit_id` is stamped onto the agent-visible tool-result envelope, so the LLM's "what went wrong?" reaction can be traced back to a specific event.
- **Approval lifecycle:** Three event types: `approval.requested`, `approval.approved`, `approval.denied`. Each carries the same `aileron.approval.id` so a request and its decision are trivially correlated.
- **Sandbox HTTPS data plane:** Generated connector shims and transparent sandbox proxy requests emit proxy audit events. `connector.proxy.proxied` and `connector.proxy.rejected` identify the resolved connector operation, upstream scheme/host/path, decision, proxy source, and response status or rejection reason. `sandbox.proxy.rejected` records transparent proxy attempts that fail before a connector operation is uniquely resolved. `sandbox.proxy.disabled` records launch sessions that start with the v4 HTTPS proxy not in force (operator opt-out, preflight failure, or unsupported sandbox mode). These events never record credential bytes, request bodies, raw headers, query strings, or full upstream URLs.

The schema is durable. Every payload field uses the OpenTelemetry-namespaced key shape (`aileron.connector.fqn`, `aileron.binding.name`, `aileron.failure.class`, etc.). Consumers (log shippers, trace tools, custom queries) read the same vocabulary regardless of which surface they came in through.

## OpenTelemetry traces (opt-in)

When tracing is enabled, Aileron starts a server-root span on every request and child spans for the work inside. The child spans cover action execution, connector calls, capability checks, and approval waits. Spans propagate via [W3C TraceContext](https://www.w3.org/TR/trace-context/). An inbound `traceparent` header from the calling agent makes Aileron's spans children of the agent's trace, so your end-to-end view stays coherent. With tracing off (the default), there's zero SDK overhead. The call sites resolve to no-op tracers. The W3C propagator is installed regardless, so an inbound `traceparent` is parsed and forwarded even when this process emits nothing.

### Three ways to consume traces

**`stdout`:** Local debugging. Spans land on stderr as JSON-per-line. Pipe to `jq`:

```sh
AILERON_OTEL_ENABLED=true \
AILERON_OTEL_EXPORTER=stdout \
aileron launch claude
```

**`file`:** Durable retention across sessions, mirroring the audit log's on-disk layout:

```sh
AILERON_OTEL_ENABLED=true \
AILERON_OTEL_EXPORTER=file \
aileron launch claude
# spans → ~/.aileron/traces/spans-YYYY-MM-DD.jsonl
```

A new file is created per local-clock day. A session that crosses midnight rolls naturally to the next day's file. `AILERON_TRACES_DIR` overrides the state directory. The default (`~/.aileron`) keeps audit and traces side by side.

**`otlp`:** Production. Ships spans to an OpenTelemetry Collector via OTLP/HTTP. See the next section.

### Hooking up to a collector

The OTLP exporter honors the [standard OTel environment variables](https://opentelemetry.io/docs/specs/otel/protocol/exporter/) that every OTel-instrumented service in your stack already understands. There's no Aileron-prefixed alternative. Forking the names would force you to maintain two parallel sets.

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

- `OTEL_EXPORTER_OTLP_ENDPOINT`: Collector URL. Defaults to `http://localhost:4318`.
- `OTEL_EXPORTER_OTLP_HEADERS`: Comma-separated `k=v` pairs added to every export request. Use this for API keys.
- `OTEL_EXPORTER_OTLP_INSECURE`: Set to `true` to allow plain HTTP (development only).
- `OTEL_EXPORTER_OTLP_TIMEOUT`: Request timeout. Default 10s.

The full set is in the [OTel exporter spec](https://opentelemetry.io/docs/specs/otel/protocol/exporter/).

### What gets emitted

| Span name | Where it's emitted |
|---|---|
| `aileron.mcp.tool.call` | `aileron-mcp` outbound to `/v1/actions/{name}/run`. Typically the trace root under `aileron launch`. |
| `aileron.action.execute` | `SandboxExecutor.Execute`. Root for an action invocation. |
| `aileron.capability.check` | Per-step action-boundary capability enforcement. Defense-in-depth, [ADR-0003](/adr/0003-action-model). |
| `aileron.connector.call` | Per-step `conn.Invoke` inside the executor. |
| `aileron.approval.wait` | The approval-queue blocking wait. Covers the entire user-decision interval. |
| HTTP server-root span | Other API entry points like `/v1/audit` and `/v1/bindings`. Generic "METHOD /path" naming. The LLM gateway endpoints (`POST /v1/chat/completions`, `POST /v1/messages`) emit no Aileron-side spans — they are transparent reverse proxies and emit no work spans of their own. |

### Span attribute schema

Every span carries the OTel-namespaced shape locked in for the audit payload. When you query traces by attribute, you query the same names you'd query the audit log by. This table is the source of truth for what's available.

**Action execution** (`aileron.action.execute`):

| Attribute | Description |
|---|---|
| `aileron.action.name` | The action manifest name being invoked. |
| `aileron.action.steps_count` | Number of `[[execute]]` steps in the action. |

**Capability check** (`aileron.capability.check`):

| Attribute | Description |
|---|---|
| `aileron.action.name` | The action whose subset is being enforced. |
| `aileron.connector.fqn` | The connector the step targets. |
| `aileron.capability.kind` | The op the action is attempting. Treated as the capability string per [ADR-0003](/adr/0003-action-model). |

**Connector call** (`aileron.connector.call`):

| Attribute | Description |
|---|---|
| `aileron.connector.fqn` | Fully-qualified connector identifier (e.g. `github://ALRubinger/aileron-connector-google`). |
| `aileron.connector.op` | The connector operation name (e.g. `list_recent_emails`). |
| `aileron.connector.hash` | The content-addressed hash of the connector binary. |

**Sandbox HTTPS data plane** (`connector.proxy.proxied`, `connector.proxy.rejected`, `sandbox.proxy.rejected` audit events):

| Attribute | Description |
|---|---|
| `aileron.proxy.source` | Where the proxy attempt entered Aileron: `generated_connector_shim`, `daemon_request_boundary`, `transparent_connect_tls`, or `launcher` (for `sandbox.proxy.disabled`). |
| `aileron.proxy.method` | HTTP method after daemon-side normalization. |
| `aileron.proxy.upstream.scheme` | Upstream scheme. Currently `https` for mediated requests. |
| `aileron.proxy.upstream.host` | Upstream host, including port when present. |
| `aileron.proxy.upstream.path` | Upstream path only. Query strings are intentionally omitted. |
| `aileron.proxy.upstream.status` | Upstream HTTP status for proxied requests. |
| `aileron.proxy.reject_reason` | Rejection class for unresolved transparent proxy attempts. |
| `aileron.connector.reject_reason` | Rejection class after a connector operation has been resolved. |
| `aileron.connector.fqn` | Set on connector-resolved proxy events. |
| `aileron.connector.tool` | Set on connector-resolved proxy events. |
| `aileron.connector.operation` | Set on connector-resolved proxy events. |
| `aileron.connector.credential` | Credential kind required by the spec, not the credential value. |
| `aileron.session.id` | Launch session associated with the sandbox request when present. |

**`sandbox.proxy.disabled`** (launch-session start without the v4 HTTPS proxy):

| Attribute | Description |
|---|---|
| `aileron.proxy.source` | Always `launcher`. The launcher is the only emitter for this event family. |
| `aileron.proxy.boundary` | Always `https_proxy`. |
| `aileron.proxy.decision` | Always `disabled`. |
| `aileron.proxy.disabled_reason` | Why the proxy is not in force for this session: `user_opt_out` (operator passed `--sandbox-proxy=off` or `AILERON_SANDBOX_PROXY=off`), `preflight_failed` (image lacks `aileron-install-proxy-ca` / `aileron-run-with-proxy-ca`; launch was refused), or `unsupported_sandbox_mode` (sandbox runtime is `off` or not `docker`/`podman`). |
| `aileron.sandbox.mode` | The value of `--sandbox` for the session (`docker`, `podman`, `off`, etc.). |
| `aileron.sandbox.image` | Resolved sandbox image reference when known (omitted when the proxy was disabled before image resolution). |
| `aileron.session.id` | Launch session id. |

The event is emitted once per launch session by the `aileron launch` CLI through the daemon's `POST /v1/sandbox-proxy/disabled` endpoint. It never records credential bytes, environment variables, the BYO image's contents, or shell history. See [ADR-0019](/adr/0019-v4-https-data-plane/) for the v4 HTTPS data plane decision and the `--sandbox-proxy` flag semantics.

**Approval wait** (`aileron.approval.wait`):

| Attribute | Description |
|---|---|
| `aileron.approval.id` | Correlation key. Same id as the `approval.requested` / `.approved` / `.denied` audit events. |
| `aileron.approval.kind` | `action` / `comms_send` / `comms_draft` / `http_request` / `shell`. |
| `aileron.approval.action` | The action-or-tool name the gate covers. |
| `aileron.approval.decision` | `approved` / `denied` / `timeout` / `cancelled`. |
| `aileron.approval.wait_ms` | Time from `RequestedAt` to `DecidedAt`, in milliseconds. Set on resolved outcomes. |
| `aileron.approval.edited` | `true` when the user edited the payload before approving. |
| `aileron.approval.reason` | Free-text reason. Set on denials when supplied. |
| `aileron.connector.fqn` | Set when the gated action targets a specific connector. |
| `aileron.session.id` | Set when the request came in under a launch session. |

**Failure (any error span).** From the closed taxonomy in [ADR-0010](/adr/0010-failure-handling):

| Attribute | Description |
|---|---|
| `aileron.failure.class` | Failure taxonomy class (`capability_denied`, `binding_required`, etc.). |
| `aileron.failure.boundary` | Where the failure was detected (`action`, `sandbox`, `runtime`). |
| `aileron.failure.retriable` | Whether the agent should retry. |
| `aileron.audit.id` | The audit event id stamped onto the failure envelope. Cross-references a span and an audit record. |

When a span fails, the OTel span status is also set to `Error` with the failure message. Your tracing UI's red flags work without parsing attributes.

## Configuration

All Aileron-side knobs are environment variables read at daemon startup. Defaults reproduce the historic behavior: tracing off, audit on. The `OTEL_EXPORTER_OTLP_*` family is consumed directly by the OTel SDK and only matters when `AILERON_OTEL_EXPORTER=otlp`.

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

A misconfigured exporter degrades gracefully to no-op rather than failing daemon startup. This applies to unknown exporter names and to known exporters whose construction fails. The Aileron HTTP server keeps serving when its telemetry sidecar is misconfigured. The failure is logged at warn level so you find it without it taking the daemon down.
