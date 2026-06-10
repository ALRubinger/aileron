---
date: 2026-06-10
status: active
type: feat
origin: docs/brainstorms/2026-06-10-v4-proxy-credential-injection-only-requirements.md
---

# Plan: v4 HTTPS Proxy — Credential Injection Only (Model A Migration)

## Summary

Flip the v4 sandbox HTTPS proxy from credential injection AND egress allowlist (current shipping behavior, Model B) to credential injection only (Model A). Cooperative HTTPS requests to upstreams that no installed connector spec matches are forwarded to the upstream unmodified and audited as a new `sandbox.proxy.passthrough` event family. Requests with protocol-level failures still get rejected. The credential-sealing claim sharpens from "sealed AND we refuse unknown upstreams" to "sealed, full stop, outbound to upstreams we don't know about is audited but unmediated."

This plan executes Issue M from the [origin brainstorm](docs/brainstorms/2026-06-10-v4-proxy-credential-injection-only-requirements.md). The narrowed #898 work (Issue 898N) and the deferred canonical schema doc (Issue D) are separate plans off the same brainstorm. One PR delivers the code flip, the ADR-0019 amendment, the observability docs update, and the GitHub-issue inventory updates so behavior and decision land together.

---

## Problem Frame

The v4 product claim is "credentialed third-party CLIs run in the container without raw credentials." Today the sandbox HTTPS proxy enforces a second, weaker claim alongside this one: unmatched upstreams are refused at the proxy boundary. That second claim is unenforceable against the threats it appears to address (a hostile CLI that opens raw TCP sockets bypasses the proxy entirely), and it creates customer-visible friction for legitimate calls (a CLI hitting a public API like `httpbin.org` or a package registry index gets a `sandbox.proxy.rejected` it cannot easily diagnose).

The architecture supports the smaller, honest claim cleanly. The CONNECT/TLS interception, the spec-matching machinery, and the credential-injection daemon path are all already in place. The only branches that change are the four unmatched-rejection sites in `internal/app/handlers_sandbox_forward_proxy.go`. The `recordSandboxProxyUnresolvedRejected` helper at `internal/app/handlers_connector_operations.go:638` becomes two helpers: a new passthrough recorder for the formerly-rejected match-failure cases, and a narrowed protocol-rejection recorder for the genuinely-failed protocol cases that stay rejections.

The brainstorm settled the product decision. This plan executes the code change, the ADR amendment, the docs alignment, and the GitHub-issue inventory updates needed to make the decision visible to readers and consistent across the project surface.

---

## Key Technical Decisions

### KTD-1. Passthrough recorder is additive, not a rename

A new `recordSandboxProxyPassthrough` helper is added to `internal/app/handlers_connector_operations.go` alongside the existing `recordSandboxProxyUnresolvedRejected`. The existing helper is then narrowed in scope (U3) and renamed to reflect its narrower job (protocol-level rejection only). Adding the new helper first lets the branch flip (U2) be a callsite swap rather than a callsite swap plus an in-place behavior change in a shared helper. The narrower-rename of the existing helper (U3) becomes a small cleanup once nothing references the old name in the old broad sense.

### KTD-2. Source field stays `transparent_connect_tls` for passthrough

The new `sandbox.proxy.passthrough` event sets `aileron.proxy.source` to `transparent_connect_tls` for every emission in this PR. Future passthrough emitters (e.g., the daemon-request boundary at `POST /v1/sandbox-proxy/requests` if it ever passes through) can introduce additional source values without changing the event shape contract. Today only the CONNECT/TLS path emits passthrough, so the field is constant in practice but documented as an enum to leave the option open.

### KTD-3. ADR-0019 is amended in place, not superseded

Per the project's pre-MVP convention (`feedback_adr_immutable` memory), ADRs are editable until MVP. ADR-0019's Decision section is amended to describe Model A. The Alternatives Considered section gains "Model B: proxy as egress allowlist" with rejection rationale. A new Threat Model subsection carries the cooperative-proxy scope statement from origin Decision 2. The References section gains the prior brainstorm doc as historical context and a pointer to the Model A brainstorm.

### KTD-4. One PR, sequenced units, ADR + docs ride with the code

One PR delivers U1 through U7 in order. The ADR amendment and the observability doc update land in the same PR as the behavior change so the project surface stays internally consistent at every commit. Splitting code from docs into separate PRs would create a window where the code says Model A and ADR-0019 still says Model B, which is exactly the kind of project-surface drift the GitHub-issue update track (U7) exists to prevent.

### KTD-5. GitHub-issue update track stays narrow

The user-facing prompt explicitly bounded the scoping-audit-issue creation to "if there are >3 issues needing non-trivial updates." Model A's inventory turns up exactly 3 changes (#747 body update, #898 narrowing, new Issue M creation). The plan holds the broader v4 scoping-audit issue as a deferred follow-up. The plan also makes a defensible decision NOT to update #828 (telemetry plumbing has a different scope; the new `sandbox.proxy.passthrough` event is one more `EventType*` constant that fits the existing audit-log story without changing #828's plumbing scope) and NOT to update #899 (ADR + docs reconciliation explicitly targets ADR-0008/0015/0012, not ADR-0019 amendments).

---

## High-Level Technical Design

The behavior change is concentrated in the unmatched branches of `handleSandboxForwardProxyDecrypted` and its callers. The branching shape before and after Model A:

| Match outcome | Before (Model B) | After (Model A) |
|---|---|---|
| Matched exactly 1 connector op | Inject credential, forward, audit `connector.proxy.proxied` | Same. No change. |
| Matched multiple connector ops | Reject; audit `sandbox.proxy.rejected reason=ambiguous_operation_match` | Forward unmodified; audit `sandbox.proxy.passthrough` |
| No connector op matched | Reject; audit `sandbox.proxy.rejected reason=operation_not_matched` | Forward unmodified; audit `sandbox.proxy.passthrough` |
| Protocol failure (non-CONNECT request to proxy port) | Reject; audit `sandbox.proxy.rejected reason=non_connect_proxy_request_unsupported` | Same. No change. |
| Protocol failure (session CA unavailable) | Reject; audit `sandbox.proxy.rejected reason=session_ca_unavailable` | Same. No change. |

The `aileron.proxy.reject_reason` enum loses `operation_not_matched` and `ambiguous_operation_match` from production code. The two remaining values stay.

The new event payload contract:

| Field | Required / Allowed / Forbidden | Notes |
|---|---|---|
| `aileron.proxy.boundary` | Required | Always `https_proxy` |
| `aileron.proxy.source` | Required | `transparent_connect_tls` today (see KTD-2) |
| `aileron.proxy.decision` | Required | Always `passthrough` |
| `aileron.proxy.method` | Required | HTTP method after daemon-side decryption |
| `aileron.proxy.upstream.scheme` | Required | Always `https` for CONNECT/TLS-intercepted requests |
| `aileron.proxy.upstream.host` | Required | Host plus port when present; never a full URL |
| `aileron.proxy.upstream.path` | Required | Path only; query strings forbidden |
| `aileron.proxy.upstream.status` | Required | Upstream HTTP status code (including error codes) |
| `aileron.session.id` | Allowed | Set when the request came in under a launch session |
| Credential substrings (`Bearer `, `Authorization`, binding-kind tokens) | Forbidden | Defense against accidental leakage; enforced by U4 conformance test and by the cross-family sweep that 898N-2 adds |

---

## Scope Boundaries

### In scope

- The five Key Technical Decisions above.
- Implementation units U1 through U7.
- All requirements M1 through M11 from the origin brainstorm.

### Deferred to Follow-Up Work

- **Issue 898N** (narrowed #898): pin `sandbox.proxy.disabled` with a conformance test, add the cross-family no-credential-leak sweep, write the redaction-rules prose, document the "not first-class" decisions for sandbox session lifecycle and non-proxy preflight failures. Separate plan; can ship before or after this one.
- **Issue D** (canonical schema doc): the consolidated audit-schema reference doc, deferred until after Model A lands so the doc reflects the new event taxonomy in one pass.
- **v4 scoping-audit issue**. The broader question "is every v4 child issue's scope/title/body current?" is orthogonal to Model A. Held as a follow-up the user can request separately.

### Deferred for later (origin brainstorm)

Carried from the brainstorm verbatim:

- Container-level network hardening (docker `--network` restrictions, egress firewall, network namespace policies).
- Egress allowlist as opt-in policy. The architecture supports adding it later as one branch in the unmatched arm plus one new event family (`sandbox.proxy.blocked`).

### Outside this product's identity (origin brainstorm)

Carried from the brainstorm verbatim:

- The v4 sandbox HTTPS proxy as a default-on egress allowlist.
- A guarantee that the proxy is a hard egress boundary against hostile agent CLIs.

---

## Implementation Units

### U1. Add `sandbox.proxy.passthrough` event family and recorder helper

**Goal.** Add the new event constant and the daemon-side recorder helper. Purely additive; no production behavior changes in this unit. After this unit, the recorder exists but no callsite invokes it.

**Requirements.** M3, M4 (additive portion).

**Dependencies.** None.

**Files.**
- `internal/model/model.go` — add `EventTypeSandboxProxyPassthrough EventType = "sandbox.proxy.passthrough"` in the existing const block near the `EventTypeSandboxProxyRejected` and `EventTypeSandboxProxyDisabled` entries.
- `internal/app/handlers_connector_operations.go` — add a new `recordSandboxProxyPassthrough(r *http.Request, source, method string, upstream *url.URL, status int) string` helper. Mirrors the shape of `recordSandboxProxyUnresolvedRejected` (at line 638) but emits the new event type with the field contract from HTD above. The session id is pulled from the `X-Aileron-Session-Id` header same as the existing helpers.
- `internal/app/handlers_connector_operations_test.go` — add unit test for the new recorder.

**Approach.**
- New event constant goes in the same const block as `EventTypeSandboxProxyDisabled`, with a comment noting the credential-injection-only framing.
- The new recorder helper takes the same first four args as `recordSandboxProxyUnresolvedRejected` plus an int `status` for the upstream HTTP status code. It does NOT take a `reason` arg (passthrough has no reject reason).
- Payload construction follows the HTD field table: every required field populated, `aileron.session.id` populated when the request header is non-empty.

**Patterns to follow.**
- `recordSandboxProxyUnresolvedRejected` at `internal/app/handlers_connector_operations.go:638` for shape and audit-recorder invocation.
- `recordSandboxProxyProxied` at `internal/app/handlers_connector_operations.go:598` for the upstream-status field pattern.

**Test scenarios.**
- Happy path: invoking `recordSandboxProxyPassthrough` with a controlled request (session id `session-passthrough-test`, source `transparent_connect_tls`, method `GET`, upstream `https://api.unknown.test/v1/resource`, status `200`) produces exactly one audit event of type `EventTypeSandboxProxyPassthrough` with every required field populated to the expected values.
- Edge: invoking with no `X-Aileron-Session-Id` header produces an event with the `aileron.session.id` field omitted (not present in the payload map).
- Forbidden-substring: invoking with a request whose URL or method does not contain `Bearer ` or `Authorization` produces a payload whose JSON serialization does not contain those substrings. This is the defensive check for the no-leak invariant the cross-family sweep (898N-2) will later run across every emitter.
- Status passthrough: invoking with a non-2xx upstream status (e.g., 404, 502) records that status verbatim in `aileron.proxy.upstream.status`. The recorder does not filter by status.

**Verification.**
- `go test ./internal/app/... -run RecordSandboxProxyPassthrough` passes.
- `go vet ./...` clean.
- `task vet:go` clean.

---

### U2. Flip unmatched-rejection branches to passthrough emission

**Goal.** Replace the two unmatched-rejection callsites in `handleSandboxForwardProxyDecrypted` with passthrough emission. Cooperative HTTPS requests to upstreams that no connector spec matches now forward through to the upstream unmodified and audit as `sandbox.proxy.passthrough`. Behavior change.

**Requirements.** M1, M2.

**Dependencies.** U1.

**Files.**
- `internal/app/handlers_sandbox_forward_proxy.go` — replace the `recordSandboxProxyUnresolvedRejected` callsites at lines 163 (`ambiguous_operation_match`) and 172 (`operation_not_matched`) with calls to the new `recordSandboxProxyPassthrough` helper. The handler also stops returning the rejection HTTP response on these branches and instead forwards the original decrypted request to the upstream, streams the response back to the client, and uses the upstream's status code as the audit field. Line 167 is the catch-all `sandboxForwardProxyMatchErrorReason(err)` branch; this stays as a rejection only if `err` represents a true error (matcher implementation failure) rather than a no-match outcome. If `sandboxForwardProxyMatchErrorReason` only returns no-match reasons today, this branch also flips to passthrough.
- `internal/app/handlers_sandbox_forward_proxy.go` — the upstream-forwarding logic for the passthrough case can reuse the daemon's existing HTTP client (`s.upstreamClient` or equivalent). Stream the response body back without buffering when possible; reuse the credentialed-path's response-streaming code if it's already factored out.

**Approach.**
- The matched-success branch is untouched. Only the no-match and ambiguous-match branches change.
- The forwarded request keeps the decrypted request's method, path, query string, headers, and body. No headers are added. No credential lookup is performed.
- The upstream's response status, headers, and body are streamed back through the established TLS connection to the in-container client. The client observes a normal HTTPS response, including the upstream's exact status code.
- Failures in the upstream forwarding itself (TCP error, TLS error reaching the upstream) produce a `sandbox.proxy.rejected` event with a new narrow reason (proposed: `passthrough_upstream_unreachable`); the in-container client gets a 502 from the proxy. This case is rare but must be auditable; defer the exact reason name to implementation if a cleaner one surfaces.

**Patterns to follow.**
- The matched-credentialed-success path's upstream forwarding and response streaming in `handlers_sandbox_forward_proxy.go`. The passthrough path is the same code with credential injection omitted.
- `recordSandboxProxyProxied` at `internal/app/handlers_connector_operations.go:598` for capturing the upstream status code after the response returns.

**Test scenarios.**
- Happy path (no match): an in-container client making `GET https://api.unknown.test/anything` through the proxy receives the upstream's response body and status verbatim; one `sandbox.proxy.passthrough` event is recorded with upstream host `api.unknown.test`, path `/anything`, status matching the upstream's response.
- Happy path (ambiguous match): an in-container client making a request that matches two connector specs receives the upstream's response unmodified; one `sandbox.proxy.passthrough` event is recorded; no credential is injected.
- Method coverage: passthrough works for `GET`, `POST` (with body), `DELETE`, `HEAD`, `PATCH`, `PUT`. The body, where present, reaches the upstream unmodified.
- Upstream error: when the upstream returns 404 or 500, the response is passed back unmodified and the audit event records the actual status. No `sandbox.proxy.rejected` event fires.
- Upstream unreachable: when the upstream TCP connection fails, the proxy returns 502 to the in-container client and emits `sandbox.proxy.rejected` with the new narrow reason. (Defer exact reason name; see Approach.)
- No credential leak: a request to an unmatched upstream whose path contains `Bearer test-token` does not produce an audit event whose serialized payload contains that substring. (The proxy doesn't add `Authorization`; this verifies the request's own content also doesn't leak into the audit.)
- Session id propagation: requests under a launch session emit passthrough events with `aileron.session.id` set; requests without a session id omit the field.

**Verification.**
- All test scenarios above pass.
- `go test ./internal/app/...` passes.
- `task vet:go` clean.

---

### U3. Narrow `sandbox.proxy.rejected` to protocol-level failures only

**Goal.** Update `recordSandboxProxyUnresolvedRejected` to a narrower scope (protocol-level rejection only) and remove the dead reason-string constants (`operation_not_matched`, `ambiguous_operation_match`) from production code. The narrower helper handles only the genuinely-failed protocol cases.

**Requirements.** M3 (cleanup portion).

**Dependencies.** U2 (must run before this; U2's callsites no longer reference the broad helper for match failures).

**Files.**
- `internal/app/handlers_connector_operations.go` — rename `recordSandboxProxyUnresolvedRejected` to `recordSandboxProxyProtocolRejected` (or a similarly-narrower name; the renamed helper still records `sandbox.proxy.rejected` event type but only for protocol failures).
- `internal/app/handlers_sandbox_forward_proxy.go` — update the two surviving callers at lines 61 and 114 to use the renamed helper. Update the inline reason strings to the narrower set: `non_connect_proxy_request_unsupported`, `session_ca_unavailable`.
- `internal/app/handlers_connector_operations.go` — remove any constants or comments that named `operation_not_matched` or `ambiguous_operation_match` if they exist as constants. Inline strings are checked too.
- `internal/app/sandbox_proxy_audit_shape_test.go` — update the `sandboxProxyRejectedShape` allowed reasons set to reflect the narrower enum.

**Approach.**
- The rename is mechanical; the helper's body shape stays the same. The narrowness lives in callsite reachability after U2.
- `sandboxForwardProxyMatchErrorReason(err)` at line 167 may still exist if the matcher returns true errors (not just no-match outcomes). That function's reason values are kept only if they represent actual matcher failures, not no-match. The implementer reviews the function's body to decide; the plan does not assume.

**Patterns to follow.**
- The existing rename convention in the codebase. The previous PR #971 renamed `data_plane_not_implemented` to `operation_not_proxyable`; that's the pattern reference.

**Test scenarios.**
- The existing `TestSandboxProxyAuditShape_SandboxProxyRejectedConforms` in `sandbox_proxy_audit_shape_test.go` still passes after the rename and the reason set narrowing.
- A test asserting that a non-CONNECT request to the proxy endpoint still produces `sandbox.proxy.rejected reason=non_connect_proxy_request_unsupported`.
- A test asserting that a CONNECT request when the session CA file is missing still produces `sandbox.proxy.rejected reason=session_ca_unavailable`.
- Negative: a grep across `internal/` for the literal strings `operation_not_matched` and `ambiguous_operation_match` returns zero matches in production code (matches may remain in code comments referencing the historical behavior or in changelog-like doc strings; those are reviewed case-by-case).

**Verification.**
- All scenarios above pass.
- `task vet:go` clean.
- Manual grep confirms no production-code references to the removed reason strings.

---

### U4. Shape conformance and integration tests for passthrough

**Goal.** Pin the `sandbox.proxy.passthrough` event family with a shape conformance test mirroring the existing pattern, and add an end-to-end integration test that exercises the new passthrough behavior through real CONNECT/TLS interception against a fake upstream.

**Requirements.** M5, M9.

**Dependencies.** U2 (the behavior must exist before it can be integration-tested).

**Files.**
- `internal/app/sandbox_proxy_audit_shape_test.go` — extend the existing pattern with a new `sandboxProxyPassthroughShape` entry mirroring the HTD field table. Add `TestSandboxProxyAuditShape_SandboxProxyPassthroughConforms` that invokes `recordSandboxProxyPassthrough` directly and validates the shape.
- `internal/app/sandbox_forward_proxy_passthrough_test.go` (new) or extension to an existing forward-proxy integration test — end-to-end test that starts a fake upstream HTTPS server, runs a CONNECT/TLS-intercepted request through the proxy to an unmatched upstream, and asserts the response was streamed back unmodified plus a `sandbox.proxy.passthrough` event with the documented shape was recorded.

**Approach.**
- The shape conformance test follows the exact pattern in `sandbox_proxy_audit_shape_test.go` lines 156-217 (the three existing `TestSandboxProxyAuditShape_*Conforms` tests). Required fields, allowed fields, forbidden substrings.
- The integration test reuses the fake-upstream and proxy-setup helpers that already exist for `sandbox_proxy_curl_integration_test.go`. The integration test is gated with the `integration_sandbox` build tag if that's the existing convention; otherwise it's a normal `_test.go` file using `httptest.NewTLSServer`.
- For `forbiddenSubstrs`: include `Bearer ` and `Authorization` (matching the existing shape tests) plus an Aileron-specific credential prefix like `lin_secret` for parity with the existing `connectorProxyProxiedShape`.

**Patterns to follow.**
- `TestSandboxProxyAuditShape_SandboxProxyRejectedConforms` at `internal/app/sandbox_proxy_audit_shape_test.go:200`. This is the precise pattern for the shape test.
- `internal/app/sandbox_proxy_curl_integration_test.go` for the integration-test scaffolding (build tag, fake upstream, proxy setup).

**Test scenarios.**

Shape conformance:
- Happy path: invoke `recordSandboxProxyPassthrough` with a controlled request; the recorded event passes the `sandboxProxyPassthroughShape.validate` check.
- Required-fields check: omitting any required field causes the validate helper to fail with a clear "missing required field" message. (This is implicit in the existing `validate` implementation at line 29.)
- Forbidden-substring check: invoke with controlled inputs that include `Bearer ` somewhere in the URL or method; the validate helper fails. (Confirms the no-leak invariant is enforced.)

Integration:
- Happy path: in-container client `GET https://fake-upstream.test/hello` against an unmatched fake upstream. Response body and status (200, "hello world") reach the client verbatim. One `sandbox.proxy.passthrough` event recorded with the documented shape.
- POST with body: in-container client `POST https://fake-upstream.test/echo` with a JSON body against an unmatched fake upstream. The fake upstream echoes the body; the response reaches the client verbatim; one passthrough event recorded.
- Upstream error: the fake upstream returns 503; the response reaches the client verbatim; the audit event records `aileron.proxy.upstream.status=503`.
- No-leak: in-container client request whose path or query (the proxy strips query for the audit field, verify this) contains a credential-shaped string does not produce an audit event whose payload contains that string.

**Verification.**
- All scenarios above pass.
- `task vet:go` clean.
- Coverage on `internal/app/handlers_sandbox_forward_proxy.go` and `internal/app/handlers_connector_operations.go` remains ≥80%.

---

### U5. Update existing rejection-asserting tests to assert passthrough

**Goal.** Update existing tests that asserted `sandbox.proxy.rejected reason=operation_not_matched` or `ambiguous_operation_match` to assert the new `sandbox.proxy.passthrough` behavior. These tests live in two files and must be flipped in lockstep with U2 so the test suite stays green.

**Requirements.** M10.

**Dependencies.** U2 (the behavior change must be in place).

**Files.**
- `internal/app/handlers_sandbox_forward_proxy_test.go` — update tests that exercise the unmatched and ambiguous-match paths to assert passthrough behavior instead of rejection. Tests that exercise protocol failures (lines 61, 114) stay unchanged.
- `internal/app/sandbox_proxy_curl_integration_test.go` — update any case that uses an unmatched upstream to assert the in-container curl receives a normal response instead of a proxy error. The test's structure stays; only the assertions on the no-match case change.

**Approach.**
- Each updated test gets the same scenario name but the new assertions. If a test name still encoded the rejection semantics (e.g., `TestSandboxForwardProxy_NoMatchRejects`), rename it (e.g., `TestSandboxForwardProxy_NoMatchPassesThrough`).
- The CLI matrix integration test (`sandbox_proxy_curl_integration_test.go`) probably has a "deliberately unmatched upstream" case that expected a rejection. The implementer reviews the test's controlled inputs and decides what the passthrough assertion looks like. If the test's fake upstream doesn't respond at all on unmatched URLs, the implementer adds a fake upstream that does respond, so the passthrough case has a concrete response to assert.

**Patterns to follow.**
- The U4 integration test scaffolding. U5's updates align to whatever shape U4 establishes.

**Test scenarios.**

This unit's test scenarios ARE the updates to existing tests; no new scenarios beyond what the file changes describe. Concretely:
- `handlers_sandbox_forward_proxy_test.go` no-match test: previously asserted `sandbox.proxy.rejected`; now asserts `sandbox.proxy.passthrough` with the upstream's response status visible.
- `handlers_sandbox_forward_proxy_test.go` ambiguous-match test: previously asserted `sandbox.proxy.rejected reason=ambiguous_operation_match`; now asserts `sandbox.proxy.passthrough` (no credential injected; ambiguity is logged but not surfaced to the client).
- `sandbox_proxy_curl_integration_test.go` unmatched-upstream case: previously asserted the in-container curl saw a proxy error; now asserts curl saw a normal HTTP response from the fake upstream, plus a passthrough event recorded.

**Verification.**
- `go test ./internal/app/...` passes.
- `go test -tags integration_sandbox ./internal/app/...` (or whatever the existing integration-test invocation is) passes.
- `task vet:go` clean.

---

### U6. Amend ADR-0019 and update observability and BYO contract docs

**Goal.** Bring the docs surface in line with the Model A behavior. ADR-0019 records the architectural decision; the observability guide documents the new event family and the narrower rejection enum; the BYO contract docs lose any egress-allowlist framing they currently carry.

**Requirements.** M6, M7, M8.

**Dependencies.** None strictly required; can land before U2 in time. In the same PR, lands alongside the code change so the project surface stays internally consistent at every commit.

**Files.**
- `docs/src/content/docs/adr/0019-v4-https-data-plane.md` — amend the Decision section to describe Model A. The cooperative-proxy behavior is named explicitly. The `sandbox.proxy.passthrough` event family is named in the Decision text. Add "Model B: proxy as egress allowlist" to the Alternatives Considered section with the rejection rationale (unenforceable against non-cooperative CLIs; friction for legitimate use; the credential-sealing claim sharpens when the egress promise is removed). Add a new "Threat Model" subsection (or extend the existing one) carrying the origin brainstorm's Decision 2 scope statement verbatim. The References section gains a link to `docs/brainstorms/2026-06-10-v4-proxy-credential-injection-only-requirements.md`.
- `docs/src/content/docs/guides/observability.md` — add a `sandbox.proxy.passthrough` row to the existing "Sandbox HTTPS data plane" event family description. Update the `sandbox.proxy.rejected` row to list only the narrower reason set. Add a short paragraph after the field tables explaining the cooperative-proxy framing (mirrors ADR-0019's amended Threat Model subsection, kept brief; the canonical statement lives in the ADR).
- `docs/src/content/docs/development/sandbox-agent-images.md` — review for egress-restriction framing. The file already references `--sandbox-proxy=off` at line 103 as an opt-out; that framing is consistent with Model A (the opt-out is for the credential-sealing layer, not an egress layer). Reword only if the file makes a hard-egress claim somewhere. Today the file does not appear to (based on grep for `egress`/`outbound`/`allowlist`/`refuse`); confirm during implementation.

**Approach.**
- ADR amendment carries the origin brainstorm's Decision 1 and Decision 2 prose verbatim or near-verbatim (the brainstorm doc is the durable artifact for the rationale).
- Observability guide is concise; readers go to ADR-0019 for the threat-model statement.
- The "no em-dashes, no 'not just X, Y', one thought per sentence" docs voice rule from project memory applies to the new docs prose.

**Patterns to follow.**
- ADR-0019's current section structure (Status, Context, Decision, Consequences, Alternatives Considered, References, Shipped PRs). The Threat Model subsection slots under Decision or as a sibling at the implementer's discretion.
- The existing field-table style in `guides/observability.md` lines 162-192 for the new passthrough table.

**Test scenarios.**

Test expectation: none, this is documentation. A non-test verification step is included in the unit's Verification field below.

**Verification.**
- ADR-0019's Decision section accurately describes Model A behavior including the new event family and the no-egress-allowlist framing.
- Observability guide's "Sandbox HTTPS data plane" section names `sandbox.proxy.passthrough` with the field shape from HTD.
- `sandbox.proxy.rejected` documentation in observability.md does NOT list `operation_not_matched` or `ambiguous_operation_match` as reasons (the narrowed set only).
- BYO contract docs do not make a hard-egress claim. If grep found such language during implementation, it's reworded; otherwise documented as unchanged.
- A docs preview build (Astro local dev server) renders cleanly without broken links.

---

### U7. GitHub issue updates and new Issue M creation

**Goal.** Create the new Issue M GitHub issue for the Model A migration. Update #898's body to reflect the narrowed 898N scope (it stops carrying gates B/C/D/M-overlap; it carries only A, E, F + documented-defer for B/C). Update #747's "active" list to acknowledge the Model A migration as a new tracked issue. Every issue body follows the convention: title states what + why clearly; body's top section is a "what this is and why" paragraph a human reader can grok in 30 seconds; the rest of the body is the agent instruction set following #898's existing style.

**Requirements.** User directives 1 and 2 from the planning prompt.

**Dependencies.** Ideally U1 through U6 land first so the new Issue M body can reference the merge commit / PR (M's body says "this is the issue that landed the Model A migration"). If sequencing constraints make that hard, the Issue M body can land first with "implementation PR pending."

**Files.**
- None in the codebase. This unit operates on GitHub via `gh` CLI.

**Approach.**

Three operations:

1. **Create new Issue M.** Title: `v4 HTTPS proxy: credential injection only (Model A migration)`. Body structure:
   - Parent: #747, related: #896 (closed), #898 (narrowed by the same brainstorm), [ADR-0019](docs/src/content/docs/adr/0019-v4-https-data-plane.md).
   - "What this is and why" preamble: 2-3 sentence summary of the Model A pivot, the credential-sealing claim sharpening, and why the egress-allowlist behavior is being removed. References the brainstorm doc.
   - Status (current date) section noting the migration shipped (or is in flight) and pointing at the merged PR.
   - "What's shipping" section: enumerated gates mirroring M1-M11 from the brainstorm.
   - Codebase anchors: file paths and line numbers of the key changes (mirrors #898's anchor-table style).
   - Acceptance criteria: bulleted list from M11 plus the no-credential-leak invariant tests.
   - Out of scope: telemetry export, container network hardening, egress allowlist as opt-in policy (carried from brainstorm's Outside this product's identity and Deferred).
   - References: brainstorm doc, ADR-0019, #896, #898, #747.

2. **Update #898 body.** Replace the existing body with the narrowed 898N scope. Keep the "Status" preamble pattern. The new body covers only gates A (sandbox.proxy.disabled conformance test), E (cross-family no-credential-leak sweep), F (redaction rules), plus documented-defer for B (session lifecycle) and C (validation failures). Gate D (canonical schema doc) moves to "deferred until after Model A lands" with a pointer to a future issue. Title can stay as-is (`v4 runtime audit schema for sandbox/data-plane flows`) since the narrowed work still fits.

3. **Update #747 body.** In the "Active / follow-on" list, add the new Issue M number and one-line description. Update #898's one-line description to reflect the narrowing (e.g., "narrowed to regression-pinning + cross-family invariant sweep after the Model A pivot; see Issue M"). In the "What is not done yet" list, replace the existing #898 line with the narrowed version.

**Patterns to follow.**
- #898's existing body structure (Status preamble, What's already shipped table, Remaining work gates with anchors, Acceptance criteria, Out of scope, References). Use the same shape for Issue M.
- The `feedback_pr_body_no_escape_backticks` memory: use `gh issue create --body-file` or quoted heredocs to preserve backticks and quotes literally. Do not `\\`-escape.

**Test scenarios.**

Test expectation: none, this unit operates on GitHub. Verification is by manual inspection of the rendered issue bodies.

**Verification.**
- The new Issue M is visible at its issue URL with the title and body structure above.
- #898's body reflects the narrowed scope; gates A, E, F remain as the active work; gates B, C are documented-defer; gate D is deferred-until-after-Model-A.
- #747's "Active / follow-on" list includes the new Issue M and an updated one-line description for #898.
- No GitHub issue body references `operation_not_matched` or `ambiguous_operation_match` as production reject reasons (these are historical only).
- Each updated/new issue body has a "what this is and why" paragraph readable in 30 seconds at the top.

---

## Risks & Dependencies

- **Risk: passthrough behavior change is visible to operators who relied on the rejection signal as a debugging tool.** Mitigation: ADR-0019's amendment includes a one-line note that the rejection behavior was prior design and was changed; the audit log makes passthrough events visible to operators who want to monitor outbound traffic from their sandboxes. The observability guide explains how to query for them.
- **Risk: existing CLI matrix integration tests are extensive and the passthrough update is mechanical-but-error-prone.** Mitigation: U5 is its own implementation unit with explicit test-by-test scope so the implementer reviews each existing test rather than batch-flipping with sed.
- **Risk: the implementer discovers that the upstream-forwarding logic for the passthrough case requires more than reusing existing helpers (e.g., the credentialed-success path is tightly coupled to credential lookup).** If this surfaces, the unit U2 grows to factor out the forwarding logic into a shared helper before reusing it. The plan does not pre-judge this; the implementer's discovery during U2 sets the actual shape.
- **Risk: `sandboxForwardProxyMatchErrorReason(err)` at line 167 turns out to return reasons that are NOT no-match outcomes after all (e.g., matcher implementation errors).** If so, those reasons stay as `sandbox.proxy.rejected` paths, not passthrough. U3's narrowing is more surgical than the plan assumes; the implementer reviews the function before flipping its callsite.
- **Dependency: ADR-0019 is editable (pre-MVP convention).** Per `feedback_adr_immutable` memory, this is fine; ADRs are amended in place until MVP ships. If a project conventions change before this PR lands, the implementer switches to a supersede-with-ADR-0019a model.
- **Assumption: no current customer or design partner expects Model B's hard-egress promise.** Per the brainstorm's risks section, no commitments to specific customers have been made yet.

---

## Open Questions

These do not block the plan. They are resolved during implementation.

1. **Exact name for the new narrow rejection reason when passthrough-forwarding fails to reach the upstream.** Proposed: `passthrough_upstream_unreachable`. The implementer finalizes after seeing the actual error shape `http.Client.Do` returns in this case.
2. **Whether `sandboxForwardProxyMatchErrorReason(err)` (line 167) returns true matcher errors or only no-match outcomes.** Read the function body during U2 to decide whether line 167 flips to passthrough or stays as rejection.
3. **Whether the CLI matrix integration test's unmatched-upstream case needs a fake upstream to actually respond.** If today the test asserts on the proxy's rejection signal without ever having an upstream to talk to, U5 either adds a fake upstream or removes the case.
4. **Renamed helper name in U3.** Proposed: `recordSandboxProxyProtocolRejected`. The implementer can pick a better name if one is clearer.

---

## Sources & Research

- [Origin brainstorm](docs/brainstorms/2026-06-10-v4-proxy-credential-injection-only-requirements.md) — the durable artifact for the Model A pivot, the threat-model framing, and the work split into Issue M, Issue 898N, and Issue D.
- [Prior brainstorm (Model B default-on)](docs/brainstorms/2026-06-09-v4-sandbox-proxy-default-on-requirements.md) — historical context. Superseded in spirit by the new brainstorm; ADR-0019's `Accepted` status is preserved, Decision content is amended.
- [Prior plan (v4 proxy finishing)](docs/plans/2026-06-09-001-feat-v4-proxy-finishing-plan.md) — finishing plan that delivered Model B's current shipping behavior.
- [Issue #747](https://github.com/ALRubinger/aileron/issues/747) — Milestone v4 umbrella. Updated by U7.
- [Issue #898](https://github.com/ALRubinger/aileron/issues/898) — runtime audit schema. Narrowed body shipped by U7; the test work shipped by Issue 898N (separate plan).
- [Issue #896 (closed)](https://github.com/ALRubinger/aileron/issues/896) — v4 HTTPS proxy umbrella (twelve implementation slices). Historical; not edited.
- [ADR-0019](docs/src/content/docs/adr/0019-v4-https-data-plane.md) — v4 HTTPS data plane decision. Amended by U6.
- `internal/app/handlers_sandbox_forward_proxy.go` — transparent CONNECT/TLS interception. The four rejection sites at lines 61, 114, 163, 167, 172 are the central anchors for U2 and U3.
- `internal/app/handlers_connector_operations.go:638` — `recordSandboxProxyUnresolvedRejected`. Renamed and narrowed under U3.
- `internal/app/handlers_connector_operations.go:598` — `recordSandboxProxyProxied`. Pattern reference for U1's new recorder.
- `internal/app/sandbox_proxy_audit_shape_test.go` — the shape-conformance test pattern that U4 extends.
- `internal/app/sandbox_proxy_curl_integration_test.go` — the CLI matrix integration test that U5 updates.
- `internal/model/model.go:307-313` — existing `EventTypeSandboxProxy*` constants. The new `EventTypeSandboxProxyPassthrough` is added here under U1.
- `internal/sandbox/container/runtime.go:445` — sandbox container runs with default network (no `--network none`). Confirms the cooperative-proxy model: bypass is possible at the network layer, the proxy is not a hard egress boundary.
- `internal/launch/proxy_bootstrap.go:203-205` — `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` injection into agent env.
- `docs/src/content/docs/guides/observability.md` — current schema docs. Updated by U6.
- `docs/src/content/docs/development/sandbox-agent-images.md` — BYO contract docs. Reviewed by U6; updated only if it makes a hard-egress claim.
- AGENTS.md / project memory — no em-dashes, no "not just X, Y", one thought per sentence (docs voice); ADRs editable pre-MVP; Codecov ≥80% on changed packages; conventional commits; squash-merge with `--admin --delete-branch`.
