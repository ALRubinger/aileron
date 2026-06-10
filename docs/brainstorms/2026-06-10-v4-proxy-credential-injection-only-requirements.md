---
date: 2026-06-10
topic: v4-proxy-credential-injection-only
---

# v4 HTTPS Proxy: Credential Injection Only

## Summary

Reframe the v4 sandbox HTTPS proxy from a credential injector AND egress allowlist (the current shipping behavior, "Model B") to a credential injector only ("Model A"). Under Model A, cooperative HTTPS requests from inside the container to upstreams that no installed connector spec matches are forwarded unmodified to the upstream and audited. They are not refused at the proxy boundary. The v4 credential-sealing claim sharpens from "sealed AND we refuse unknown upstreams" to "sealed, full stop. Outbound traffic to upstreams we don't know about goes through unmodified and is audited."

This decision also reshapes [issue #898](https://github.com/ALRubinger/aileron/issues/898). The work splits into three pieces:

1. **Model A migration (Issue M).** Replace the unmatched-rejection paths with passthrough emission. Add a new `sandbox.proxy.passthrough` event family. Amend [ADR-0019](docs/src/content/docs/adr/0019-v4-https-data-plane.md) to reflect the new decision and its threat-model scope.
2. **Narrowed #898 (Issue 898N).** Ship the model-robust pieces of #898's six gates: the `sandbox.proxy.disabled` conformance test, the cross-family no-credential-leak sweep, the redaction-rules prose. Document the "not first-class" decisions for sandbox session lifecycle (gate B) and non-proxy preflight failures (gate C). Defer the canonical audit-schema reference doc (gate D).
3. **Deferred canonical audit-schema doc (Issue D).** Write `docs/src/content/docs/reference/audit-schema.md` after the Model A migration lands so the doc reflects the new event taxonomy in one pass. Update [ADR-0019](docs/src/content/docs/adr/0019-v4-https-data-plane.md)'s References section to cite it.

Issues M and 898N can ship in either order. Future egress-allowlisting work, if a customer-discovery conversation surfaces a real need, is additive to Model A and does not require restructuring the proxy.

---

## Problem Frame

The v4 product claim is "credentialed third-party CLIs run in the container without raw credentials." Twelve implementation slices under the [#896](https://github.com/ALRubinger/aileron/issues/896) umbrella made that claim shippable. Under what currently lives on `main`:

1. The sandbox container has a session-local CA installed in its trust store. The launcher injects `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY` into agent env (see `internal/launch/proxy_bootstrap.go:203`).
2. Cooperative CLIs (curl, gh, aws, git, wget, most language HTTP clients) honor `HTTPS_PROXY` and route CONNECT through the daemon.
3. The daemon terminates TLS, decrypts the request, and matches it against installed connector specs.
4. Matched upstreams get credential injection daemon-side, the request is forwarded with `Authorization`, the response is streamed back, audited as `connector.proxy.proxied`.
5. Unmatched upstreams are refused at the proxy. The daemon emits `sandbox.proxy.rejected` with `reject_reason: operation_not_matched` (or `ambiguous_operation_match`), and the CLI inside the container gets a proxy-level error (see `internal/app/handlers_sandbox_forward_proxy.go:163-172`).

Step 5 makes the proxy an egress allowlist in addition to a credential injector. That second job is the problem.

The architecture cannot actually enforce an egress allowlist. A CLI that does not honor `HTTPS_PROXY`, or that opens raw TCP sockets and does its own TLS handshake, bypasses the proxy entirely. The sandbox container runs with default network policy (see `internal/sandbox/container/runtime.go:445`). The egress-allowlist promise is enforceable only against cooperative, well-behaved CLIs that already pose minimal exfiltration risk. The promise is theater for the threat it appears to address, which is a hostile agent CLI exfiltrating data to attacker-controlled domains.

The behavior also creates customer-visible friction with no compensating guarantee. A user running an agent in the sandbox who wants the agent to call a public API (`api.weather.com`, `httpbin.org`, a CRL distribution point, a package registry index), or who is debugging by hitting their own services from inside the container, sees a `sandbox.proxy.rejected` they cannot easily diagnose. The promise the rejection encodes is the promise the architecture cannot keep.

The v4 credential-sealing claim is independent of the egress-allowlist behavior. Sealing means credentials live in the host vault, are attached at the daemon boundary, and never reach the container. That claim holds whether the unmatched branch refuses or passes through. Decoupling the two lets the credential-sealing claim stand on its own. It stops the proxy from making a hard-egress promise it cannot keep.

The right product shape is Model A: the proxy is a credential injector only. Egress allowlisting is a separate concern. If a future customer conversation surfaces a real need, it lives in a policy layer added on top of the existing proxy without restructuring it.

---

## Key Decisions

### 1. The v4 HTTPS proxy is a credential injector, not an egress allowlist

Cooperative-proxy mediation continues to terminate TLS, decrypt requests, and attempt to match each request against installed connector specs. The behavior on each branch changes:

- **Matched upstream.** Look up the user's credential for the matched connector binding. Inject it daemon-side. Forward the request to the upstream. Audit as `connector.proxy.proxied`. This is the existing behavior, unchanged.
- **Unmatched upstream.** Forward the request to the upstream **unmodified**. No headers are added. No credentials are looked up. Stream the response back. Audit as `sandbox.proxy.passthrough` (new). The CLI inside the container observes a normal HTTPS response from the upstream.
- **Protocol-level proxy failures.** `sandbox.proxy.rejected` narrows to actual protocol failures the daemon cannot process: `non_connect_proxy_request_unsupported` and `session_ca_unavailable`. The `operation_not_matched` and `ambiguous_operation_match` reject reasons are removed from production code.

**Rationale.** The credential-sealing claim is the v4 product promise. The egress-allowlist promise is unenforceable against the threats it appears to address. Removing it sharpens the claim Aileron makes and removes friction for legitimate outbound calls from cooperative agents.

**Reversibility.** Egress rules are additive. A future policy lives in the unmatched branch as `consult policy; reject (emitting a new `sandbox.proxy.blocked` event class) or passthrough`. That is one `if` branch and one event constant. It is not a restructuring of the proxy.

### 2. Threat model is explicit and disclosed

The credential-sealing claim is honest about its scope. The authoritative scope statement is:

> Aileron's v4 sandbox HTTPS proxy ensures that credentials Aileron manages on behalf of the user are attached only at the daemon boundary and never written into the container's environment, files, or process memory. The proxy does not restrict outbound network access from the container. Cooperative CLIs honoring `HTTPS_PROXY` route through the proxy and are audited. CLIs that bypass `HTTPS_PROXY` reach the network directly via the container's default networking and are not audited.

This statement appears verbatim in ADR-0019's amended Decision section, in the BYO contract docs at `docs/src/content/docs/development/sandbox-agent-images.md` if it references egress restrictions today, and in the eventual canonical audit-schema reference doc (Issue D).

**Rationale.** An honest disclosure of the boundary's coverage is what makes the credential-sealing claim trustworthy. Promising a hard egress boundary the architecture cannot enforce is the failure mode this brainstorm corrects.

### 3. New event family `sandbox.proxy.passthrough` joins the schema

A new audit event constant `EventTypeSandboxProxyPassthrough` is added to `internal/model/model.go` with the string value `sandbox.proxy.passthrough`. The payload mirrors the relevant subset of `connector.proxy.proxied`:

- **Required.** `aileron.proxy.boundary` (always `https_proxy`), `aileron.proxy.source` (initially `transparent_connect_tls`; other sources possible later), `aileron.proxy.decision` (always `passthrough`), `aileron.proxy.method`, `aileron.proxy.upstream.scheme`, `aileron.proxy.upstream.host`, `aileron.proxy.upstream.path`, `aileron.proxy.upstream.status`.
- **Allowed.** `aileron.session.id`.
- **Forbidden substrings.** Credential bytes per binding kind, `Bearer `, `Authorization` literal. Same forbidden set as the matched-proxied family.

The match attempt still runs (the daemon needs to know whether to inject credentials), but the unmatched outcome is no longer a rejection.

### 4. #898 is narrowed; canonical schema doc is deferred

#898 ships the model-robust slice of its six gates:

- **A.** Pin `sandbox.proxy.disabled` with a conformance test. Mirrors the existing `internal/app/sandbox_proxy_audit_shape_test.go` pattern. Holds regardless of Model A vs Model B.
- **E.** Cross-family no-credential-leak sweep test. Walks every audit-event emission helper with controlled credential inputs and asserts no leak in any payload. Holds regardless of model. Gains importance under Model A because the new `sandbox.proxy.passthrough` family adds another payload surface.
- **F.** Redaction-rules prose in code-adjacent docs. Forbidden substrings, host-only-not-URL discipline, header allowance rules. Used as the contract the sweep test enforces.
- **B.** Sandbox session lifecycle events. **Documented-defer.** The `sessions` store at `internal/app/handlers_sessions.go` is the system of record for session start, end, exit code, and orphan reaping. Every existing event family already carries `aileron.session.id` for join. No `sandbox.session.*` event constants are added.
- **C.** Non-proxy preflight failures. **Documented-defer.** The launcher exits non-zero with stderr on every refusal class (agent command missing, workspace not writable, mcp binary missing, shim runtime missing). The signal is already operator-visible. No `sandbox.launch.validation_failed` event is added.

Deferred:

- **D.** Canonical audit-schema reference doc. Defer until after the Model A migration so the doc reflects the new event taxonomy (including `sandbox.proxy.passthrough`) and the narrowed `sandbox.proxy.rejected` semantics. Writing the doc before the migration means writing it twice.

The narrowed #898 (Issue 898N below) is a small PR that holds across either model. It can ship in any order relative to the Model A migration.

### 5. ADR-0019 is amended, not superseded

The Decision section of [ADR-0019](docs/src/content/docs/adr/0019-v4-https-data-plane.md) is amended to describe Model A. The Alternatives Considered section gains "Model B: proxy as egress allowlist" with the explicit rationale for why it was tried and rejected: unenforceable against non-cooperative CLIs, friction for legitimate use, and the credential-sealing claim sharpens when the egress promise is removed.

Pre-MVP convention is to amend ADRs in place. Superseding via a new ADR is a post-MVP operation.

---

## Requirements

The work splits into three issues. All three ship from this requirements doc.

### Issue M: Model A migration

- **M1.** Cooperative HTTPS requests inside the sandbox whose upstream matches no installed connector spec are forwarded to the upstream unmodified (no `Authorization` injected, no other headers added) and audited as `sandbox.proxy.passthrough`.
- **M2.** Cooperative HTTPS requests whose upstream matches a connector spec ambiguously (multiple connector ops match) are forwarded to the upstream unmodified and audited as `sandbox.proxy.passthrough`. The ambiguity is logged via the existing daemon log. No credential is injected.
- **M3.** `sandbox.proxy.rejected` narrows to protocol-level failures only: `non_connect_proxy_request_unsupported` and `session_ca_unavailable`. The reject reasons `operation_not_matched` and `ambiguous_operation_match` are removed from production code.
- **M4.** `EventTypeSandboxProxyPassthrough` is added to `internal/model/model.go`. The recorder at `recordSandboxProxyUnresolvedRejected` in `internal/app/handlers_connector_operations.go` is renamed and reshaped to a passthrough recorder. Callsites in `internal/app/handlers_sandbox_forward_proxy.go` lines 163-172 are updated.
- **M5.** A shape conformance test in `internal/app/sandbox_proxy_audit_shape_test.go` pins `sandbox.proxy.passthrough` against its documented schema. Mirrors the pattern of the three existing conformance tests.
- **M6.** [ADR-0019](docs/src/content/docs/adr/0019-v4-https-data-plane.md) is amended. The Decision section reflects Model A. The Alternatives Considered section gains "Model B: proxy as egress allowlist" with rejection rationale. A new Threat Model subsection carries the explicit cooperative-proxy scope statement from Decision 2.
- **M7.** `docs/src/content/docs/guides/observability.md` "Sandbox HTTPS data plane" section is updated. The `sandbox.proxy.passthrough` event family is added. The `sandbox.proxy.rejected` table narrows to the protocol-level reasons.
- **M8.** `docs/src/content/docs/development/sandbox-agent-images.md` (the BYO contract) is updated if it currently references egress restrictions. The credential-injection-only framing is the new authoritative claim.
- **M9.** A new integration test exercises an end-to-end CONNECT/TLS interception of a request to an unmatched upstream against a fake upstream server. Asserts the request reached the upstream unmodified, the response was streamed back to the client, and a `sandbox.proxy.passthrough` event was recorded with the documented shape.
- **M10.** Existing integration tests under `internal/app/sandbox_forward_proxy_test.go` and the CLI matrix integration test from PR #972 are updated to assert passthrough where they currently assert rejection on unmatched upstreams.
- **M11.** `task vet:go` clean. Coverage holds at ≥80% on changed packages.

### Issue 898N: narrowed #898

- **898N-1.** Extend `internal/app/sandbox_proxy_audit_shape_test.go` with a `sandboxProxyDisabledShape` entry mirroring the docs table. Required: `aileron.proxy.source=launcher`, `aileron.proxy.boundary=https_proxy`, `aileron.proxy.decision=disabled`, `aileron.proxy.disabled_reason`, `aileron.session.id`. Allowed: `aileron.sandbox.mode`, `aileron.sandbox.image`. Forbidden: credential substrings, `Bearer `, `Authorization`. Add a test that invokes `(*apiServer).RecordSandboxProxyDisabled` directly with each of the three reason enum values (`user_opt_out`, `preflight_failed`, `unsupported_sandbox_mode`) as subtests.
- **898N-2.** Add a cross-family no-credential-leak sweep test that walks every audit-event emission helper currently in `internal/app/` with a controlled credential value (for example `lin_secret_test_XYZ` and `Bearer test-token-XYZ`). Asserts no serialized event payload contains the controlled substring across any emitted event. The sweep is a single test function with a table of (emitter, controlled inputs) entries.
- **898N-3.** Write the redaction rules as code-adjacent prose in `docs/src/content/docs/guides/observability.md`. Lists forbidden substrings (credential bytes per binding kind, `Bearer `, `Authorization` literal), upstream URL discipline (scheme, host, path; never query strings or full URLs), and header discipline (header names may appear in payload metadata; header values never appear). The 898N-2 sweep test enforces these in code.
- **898N-4.** The decisions "sandbox session lifecycle events are not first-class" and "non-proxy preflight failures are not first-class" are documented in `docs/src/content/docs/guides/observability.md` with rationale: the sessions store is the system of record for lifecycle, and stderr plus non-zero exit is the system of record for preflight refusals.
- **898N-5.** `task vet:go` clean. Coverage holds at ≥80% on changed packages.

### Issue D: deferred canonical audit-schema doc (after Model A lands)

- **D1.** A canonical audit-schema reference doc is written at `docs/src/content/docs/reference/audit-schema.md` (placement decided then; `guides/` is the lighter alternative if no other reference content is queued). The doc enumerates every `EventType*` with: string value, emitter helper, required fields, allowed-optional fields, forbidden substrings, example payload. The doc cites Decision 2's scope statement so readers understand what audit covers.
- **D2.** The dispersed field tables in `guides/observability.md` are moved into the reference doc. The guide retains a short overview and a link.
- **D3.** `docs/src/lib/navigation.ts` is updated to surface the reference doc. A new `Reference` section is added if `audit-schema.md` is one of multiple planned entries. Otherwise it appears as a single Guides entry.
- **D4.** ADR-0019's References section is updated to cite the canonical schema doc.

---

## Scope Boundaries

### In scope (this brainstorm)

- The five Key Decisions above.
- The split of work into M (Model A migration), 898N (narrowed #898), and D (deferred canonical doc).
- The threat-model scope statement (Decision 2).
- The new event family taxonomy under Model A.

### Deferred for later

- **D1 through D4.** The canonical audit-schema reference doc. Done after Model A lands so the doc reflects the new event taxonomy.
- **Container-level network hardening.** The sandbox container runs with default network today. If a future need surfaces, hardening (docker `--network` restrictions, egress firewall, network namespace policies) is additive defense-in-depth. It lives separately from the proxy decision here.
- **Egress allowlist as opt-in policy.** If a customer-discovery conversation surfaces a real need ("we run agents in regulated environments and need to assert no outbound traffic to non-allowlisted domains"), an opt-in policy layer can be added to the unmatched branch of the proxy. That work is a single new event family (`sandbox.proxy.blocked`), a policy file, and the policy branch. Not now.

### Outside this product's identity

- **The v4 sandbox HTTPS proxy as a default-on egress allowlist.** Rejected in this brainstorm. See Decision 1 and Problem Frame for the rationale.
- **A guarantee that the proxy is a hard egress boundary against hostile agent CLIs.** The cooperative-proxy model cannot enforce this. Promising it is the failure mode this brainstorm corrects. Egress hardening, if added, lives at the container or network layer. The proxy's job is and remains credential injection.

---

## Open Questions

These do not block the doc or the work. The implementation PRs resolve them.

1. **Naming of the new event family.** `sandbox.proxy.passthrough` is consistent with existing `sandbox.proxy.*` naming and clearly distinguishes it from `connector.proxy.proxied` (which means "matched and proxied with credential injection"). Alternatives: `sandbox.proxy.forwarded` or `connector.proxy.passthrough`. The implementation PR can finalize.
2. **Whether passthrough emission is one event type or two** (separate success and error subtypes for upstream HTTP status). Leaning toward one type with `aileron.proxy.upstream.status` carrying the upstream's HTTP status code, including error codes. Mirrors how `connector.proxy.proxied` handles upstream errors today.
3. **Whether to surface the passthrough decision to the operator at session start or in startup banners.** Today operators see `sandbox.proxy.disabled` events when the proxy is off. Under Model A, passthrough events appear when CLIs hit unmatched upstreams. The observability guide should help readers understand passthrough is expected, not a credential-sealing failure.

---

## Risks & Assumptions

- **Risk: existing tests that assert `operation_not_matched` rejection are extensive.** The integration tests at `internal/app/sandbox_forward_proxy_test.go` and the CLI matrix integration test from PR #972 assert on the rejection behavior. Migration M flips these to assert passthrough. The diff is concentrated but visible. Coverage holds because the new event family gains its own shape conformance test (M5) and the existing tests are updated alongside.
- **Risk: passthrough behavior could surprise users who relied on the rejection signal.** Mitigation: ADR-0019's amendment includes a brief note that the rejection behavior was a previous design that was changed. The audit log makes passthrough visible to operators who want to monitor outbound traffic from their sandboxes.
- **Assumption: the `internal/sandbox/discovery` matcher continues to return a clean "no match" outcome.** No structural change is needed here. Only the consumer of the outcome changes.
- **Assumption: no current customer or design partner expects Model B's hard-egress promise.** Per the project's v4 ICP hypothesis, validation is gated to future discovery conversations. No commitments to specific customers have been made yet.
- **Assumption: the cooperative-proxy model is acceptable for the v4 milestone.** This assumption already shipped under #896 and is not revisited here. If a future need for non-cooperative coverage surfaces, container-level network hardening (deferred above) is the layer that addresses it.

---

## Sources / Cross-References

- [Issue #898](https://github.com/ALRubinger/aileron/issues/898) — runtime audit schema for sandbox/data-plane flows. Narrowed by this brainstorm.
- [ADR-0019](docs/src/content/docs/adr/0019-v4-https-data-plane.md) — v4 HTTPS data plane decision. To be amended under M6.
- [Issue #896](https://github.com/ALRubinger/aileron/issues/896) — closed v4 HTTPS proxy umbrella (twelve implementation slices).
- [Prior brainstorm 2026-06-09](docs/brainstorms/2026-06-09-v4-sandbox-proxy-default-on-requirements.md) — defined Model B's default-on behavior. Superseded in spirit by this brainstorm; ADR-0019's "Accepted" status is preserved, the Decision content is amended.
- [Prior plan 2026-06-09-001](docs/plans/2026-06-09-001-feat-v4-proxy-finishing-plan.md) — finishing plan that delivered Model B's current implementation.
- `internal/app/handlers_sandbox_forward_proxy.go` — transparent CONNECT/TLS interception. Current rejection paths at lines 61, 114, 163, 167, 172.
- `internal/app/handlers_connector_operations.go:638` — `recordSandboxProxyUnresolvedRejected`. To be reshaped under M4.
- `internal/app/handlers_sandbox_proxy_disabled.go` — the existing `sandbox.proxy.disabled` recorder. 898N-1 pins it with a conformance test.
- `internal/app/sandbox_proxy_audit_shape_test.go` — the conformance test pattern. 898N-1 and M5 extend it.
- `internal/model/model.go:307-313` — existing `EventTypeSandboxProxy*` constants. The new `EventTypeSandboxProxyPassthrough` is added here under M4.
- `internal/sandbox/container/runtime.go:445` — sandbox container runs with default network (no `--network none`). Confirms the cooperative-proxy model.
- `internal/launch/proxy_bootstrap.go:203-205` — `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` injection into agent env.
- `docs/src/content/docs/guides/observability.md` — current schema docs. Updated by M7 and 898N-3.
