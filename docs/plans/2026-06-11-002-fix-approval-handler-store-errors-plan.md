---
title: "fix: Surface intent-store failures in Approve/Deny/Modify handlers"
type: fix
status: completed
date: 2026-06-11
origin: GitHub issue #955
depth: standard
---

# fix: Surface intent-store failures in Approve/Deny/Modify handlers

## Summary

The three approval-action handlers (`ApproveRequest`, `DenyRequest`, `ModifyRequest` in `internal/app/handlers.go`) couple two persisted records: the approval (mutated by the orchestrator) and the intent (mutated by a direct store write). When the intent-store write path fails, that failure must surface as a `500 store_error` so the caller knows approval state and intent state have diverged — not a silent `200` that claims the intent moved when it did not.

Issue #955 is **partially fixed**. This plan closes the remaining gaps:

- **Deny** still swallows a `Get` failure (no `else` branch) and returns `200` with `intent_status: denied` while the persisted intent keeps its prior status.
- **All three** handlers discard the `s.intents.Update(...)` return error, so a failed write is invisible.

The plan also brings the OpenAPI spec into sync: the approve/deny/modify endpoints document no `500` response today even though Approve/Modify already return one. (Spec-sync scope confirmed with the user: sync all three endpoints via a shared response component.)

---

## Problem Frame

Approval and intent are linked records. After a successful `POST /v1/approvals/{id}/deny`:

- The caller sees `intent_status: denied` in the response envelope.
- A subsequent `GET` of the intent returns its **prior** status (`pending_approval`, `approved`, …) because the `Get`-then-`Update` block was skipped when `Get` failed.
- The approval audit trail and a dashboard querying intents directly diverge.

This is correctness drift, not style. The fix is detection plus a truthful `500` — not atomic rollback. The orchestrator-side approval mutation has already happened by the time the intent write runs; a stronger transactional invariant is explicitly out of scope (see issue #955).

Current state in `internal/app/handlers.go`:

| Handler | Get-failure path | Update error |
|---|---|---|
| `ApproveRequest` (`:475-492`) | Handled — `else { writeError(500, "store_error"); return }` at `:489-492` | Discarded at `:488` |
| `DenyRequest` (`:535-539`) | **Swallowed** — no `else`, returns `200` | Discarded at `:538` |
| `ModifyRequest` (`:577-594`) | Handled — `else` at `:591-594` | Discarded at `:590` |

---

## Requirements

- **R1** — A `Get` failure in `DenyRequest` must return `500` with `error.code = "store_error"` and must not return `200` with a `denied` intent status. (Issue #955 remaining work (a).)
- **R2** — An `Update` failure in `ApproveRequest`, `DenyRequest`, and `ModifyRequest` must return `500` with `error.code = "store_error"`. (Issue #955 remaining work (b).)
- **R3** — On any of the above store failures, the orchestrator-side approval state mutation is **not** rolled back. The failure mode is detection + `500`, not atomic rollback. (Issue #955 "Tests" and "Suggested fix" notes.)
- **R4** — The OpenAPI spec documents the `500` response for the approve, deny, and modify endpoints, keeping the spec authoritative over the code per `CLAUDE.md`.
- **R5** — Existing passing behavior is preserved: success paths still return `200` with the correct envelope; the already-fixed Approve/Modify `Get`-failure paths still return `500`.

---

## Key Technical Decisions

### KTD1 — Restructure the Deny block to the guard-clause shape, don't bolt on an `else`

The issue's suggested fix replaces the `if err == nil { … }` form with the early-return guard shape already used by `GetIntent` (`:372`) and `AppendIntentEvidence` (`:410`):

```
intent, err := s.intents.Get(ctx, apr.IntentID)
if err != nil { writeError(w, 500, "store_error", err.Error()); return }
intent.Status = api.Denied
intent.UpdatedAt = time.Now().UTC()
if err := s.intents.Update(ctx, intent); err != nil { writeError(w, 500, "store_error", err.Error()); return }
```

*Directional guidance, not final code.* This is the established in-file pattern and reads better than nesting. For Approve and Modify, the existing `if … ; err == nil { … } else { writeError… }` form already surfaces the `Get` failure correctly, so the minimal change there is to add the `Update`-error check **inside** the success branch rather than restructure the whole block. Both shapes are acceptable; match each handler's existing structure to keep the diff small and reviewable.

### KTD2 — Widen `apiServer.intents` from `*mem.IntentStore` to the `store.IntentStore` interface

Testing R2 requires a store where `Get` succeeds but `Update` fails. The `mem.IntentStore` cannot express that (its `Update` only fails on not-found, which would also fail `Get`), and it has no fault-injection hooks. The field at `internal/app/handlers.go:53` is concretely typed `*mem.IntentStore`, but **all** production and test usages call only interface methods (`Create`/`Get`/`Update`/`List`), and both assignment sites (`app.go:253`/`:402` and the test server at `handlers_execution_test.go:78`) assign `mem.NewIntentStore()`, which satisfies `store.IntentStore`.

Decision: change the field type to `store.IntentStore`. This is the idiomatic Go "accept interfaces" posture, is a zero-behavior-change refactor, and unlocks a test-only failing-store stub. **Rejected alternative:** adding `FailGet`/`FailUpdate` fields to the production `mem.IntentStore` — pollutes production code with test hooks (anti-pattern, and conflicts with the user's preference for clean abstractions).

### KTD3 — Add a reusable `InternalServerError` response component in the spec

No shared `500` response component exists today; other endpoints inline their `500` blocks. Per the user's confirmed scope (sync all three endpoints) and the preference for reusable abstractions over inline duplication, define one `InternalServerError` response under `components/responses` (mirroring `BadRequest`/`NotFound`, schema `#/components/schemas/Error`) and `$ref` it from approve, deny, and modify. Regenerate the server interface via `task generate:api`. Adding a documented response does not change the Go handler signatures, but the spec must lead the code per `CLAUDE.md`.

---

## Scope Boundaries

In scope:
- Deny `Get`-failure fix; `Update`-error surfacing in all three handlers.
- `apiServer.intents` field-type widening to the interface.
- Spec: shared `InternalServerError` component + `500` refs on approve/deny/modify; regenerate.
- Unit tests proving R1–R3 and R5.

### Deferred to Follow-Up Work
- Transactional atomicity across the orchestrator mutation + intent update (atomic rollback). Explicitly out of scope per issue #955; would be a larger refactor.
- Auditing other handlers that discard `s.intents.Update(...)` returns (e.g. `:360`, `:365`, `:426`). Real but tangential; not part of this issue.

### Outside this change's identity
- Reconciliation/retry machinery for re-attempting a failed intent update after the `500`. The issue notes "the next reconciliation pass (or a retry) re-attempts the intent update" as an existing expectation, not new work to build here.

---

## Implementation Units

### U1. Widen `apiServer.intents` to the `store.IntentStore` interface

- **Goal:** Enable injection of a failing intent store in tests without touching production behavior.
- **Requirements:** Enables R2 testing.
- **Dependencies:** none.
- **Files:**
  - `internal/app/handlers.go` (field declaration at `:53`)
- **Approach:** Change the struct field type from `*mem.IntentStore` to `store.IntentStore`. Confirm `store` is already imported (it is — used for filters). No call-site changes: all usages are interface methods, and both assignment sites pass `mem.NewIntentStore()`. Build to confirm the type-check passes.
- **Patterns to follow:** Other `apiServer` store fields; the `store.IntentStore` interface at `internal/store/store.go:18`.
- **Test scenarios:** `Test expectation: none — pure type-widening refactor with no behavioral change; covered transitively by the full `internal/app` suite still passing (`task test` / `go test ./internal/app/...`).`
- **Verification:** `task test` package compiles and the existing approval/execution tests still pass.

### U2. Fix `DenyRequest` to surface intent-store failures

- **Goal:** A `Get` or `Update` failure in Deny returns `500 store_error` instead of a misleading `200`.
- **Requirements:** R1, R2 (deny), R3, R5.
- **Dependencies:** U1 (for the test in U5; the handler edit itself is independent).
- **Files:**
  - `internal/app/handlers.go` (`DenyRequest`, `:534-539`)
- **Approach:** Replace the `if intent, err := s.intents.Get(...); err == nil { … }` block with the guard-clause shape from KTD1: early-return `500 store_error` on `Get` error, then check and early-return on `Update` error. The orchestrator `Deny` call above stays untouched, satisfying R3. Leave the trace-event emission and the `200` response envelope below unchanged for the success path.
- **Patterns to follow:** Guard-clause error handling at `handlers.go:372` and `:410`; the `else` branch already present in `ApproveRequest:489`.
- **Test scenarios:** see U5 (`Deny_GetFailure`, `Deny_UpdateFailure`, plus the preserved success path).
- **Verification:** Deny success still returns `200` with `intent_status: denied`; injected `Get`/`Update` failures return `500` with `error.code = "store_error"`.

### U3. Surface the discarded `Update` error in `ApproveRequest` and `ModifyRequest`

- **Goal:** A failed `s.intents.Update(...)` in Approve and Modify returns `500 store_error` instead of being discarded.
- **Requirements:** R2 (approve, modify), R3, R5.
- **Dependencies:** U1 (for tests).
- **Files:**
  - `internal/app/handlers.go` (`ApproveRequest:488`, `ModifyRequest:590`)
- **Approach:** Inside each handler's existing `Get`-success branch, wrap the `s.intents.Update(ctx, intent)` call in `if err := …; err != nil { writeError(w, 500, "store_error", err.Error()); return }`. Keep the surrounding grant-issuance and `else` `Get`-failure handling intact. Do not restructure these blocks (their `Get`-failure path already works) — only the `Update` line changes.
- **Patterns to follow:** The `grants.Create` error check immediately above each `Update` (`:481-484`, `:583-586`) already uses this exact shape.
- **Test scenarios:** see U5 (`Approve_UpdateFailure`, `Modify_UpdateFailure`).
- **Verification:** Approve/Modify success paths still return `200` with grant + `intent_status: approved`; injected `Update` failure returns `500 store_error`.

### U4. Document the `500` response on approve/deny/modify in the OpenAPI spec

- **Goal:** Spec matches the handlers' actual `500 store_error` behavior; spec stays authoritative.
- **Requirements:** R4.
- **Dependencies:** none (independent of U1–U3).
- **Files:**
  - `internal/api/openapi.yaml` (new `InternalServerError` under `components/responses` near `:4006`; `500` refs on the three endpoints at `:1521`, `:1492`, `:1550`)
  - `internal/api/gen/server.gen.go` (regenerated — **never hand-edit**)
- **Approach:** Add `InternalServerError` response component mirroring `BadRequest`/`NotFound` (description "Internal server error", content `application/json` schema `#/components/schemas/Error`). Add `"500": { $ref: "#/components/responses/InternalServerError" }` to the `responses` map of `approveRequest`, `denyRequest`, and `modifyRequest`. Run `task generate:api` and commit the regenerated `server.gen.go`.
- **Patterns to follow:** Existing response components `BadRequest`/`Unauthorized`/`Forbidden`/`NotFound` at `:4006-4029`; inline `500` blocks elsewhere in the spec confirm the `Error` schema is the right body.
- **Test scenarios:** `Test expectation: none — spec/doc change. Verified by `task generate:api` producing a clean diff and the package still compiling.`
- **Verification:** `task generate:api` regenerates without manual edits; `git diff` on `server.gen.go` is consistent with the spec change; build passes.

### U5. Unit tests proving the failure paths

- **Goal:** Prove R1–R3 and R5 with regression tests that fail before U2/U3 and pass after.
- **Requirements:** R1, R2, R3, R5.
- **Dependencies:** U1 (failing-store injection), U2, U3.
- **Files:**
  - `internal/app/handlers_execution_test.go` (add a failing-store stub + new test functions)
- **Approach:** Add a test-only stub implementing `store.IntentStore` that wraps a real `mem.IntentStore` and returns a configured error from `Get` and/or `Update` on demand (e.g. `failingIntentStore{inner, failGet, failUpdate error}`). Build each test on the existing `TestApproveRequest_IssuesGrantCapability` / `TestModifyRequest_IssuesBoundedGrantCapability` setup (real `mem.NewApprovalStore()` + `approval.NewInMemoryOrchestrator`, seeded intent + approval), then swap `s.intents` for the failing stub before invoking the handler. Assert status `500` and decode the `api.Error` envelope to check `error.code == "store_error"`. For R3, after the `500` assert the orchestrator/approval record still reflects the denied/approved mutation via `s.approvals.Get(...)`.
- **Patterns to follow:** `TestApproveRequest_MissingIntentAfterApproval` (`:321`) and `TestModifyRequest_MissingIntentAfterApproval` (`:397`) for the orchestrator+approval scaffolding and the `500` assertion; `writeError` envelope shape (`{"error":{"code":…}}`) at `handlers.go:141`; `decodeBody`/`json.NewDecoder` response decoding used throughout the file.
- **Test scenarios:**
  - **Happy path (regression guard, R5):** `Deny` with a healthy store returns `200` and `intent_status: denied`; the persisted intent's status is `denied` on a follow-up `Get`. (Covers R1's success counterpart.)
  - **Deny `Get` failure (R1):** stub `Get` returns an error → response is `500`, `error.code == "store_error"`; intent is **not** reported as denied. This is the core new regression — it fails before U2.
  - **Deny `Update` failure (R2):** stub `Get` succeeds, `Update` returns an error → `500`, `error.code == "store_error"`.
  - **Approve `Update` failure (R2):** stub `Get` succeeds, `Update` returns an error after grant creation → `500`, `error.code == "store_error"`. Fails before U3.
  - **Modify `Update` failure (R2):** same shape as Approve for the modify path → `500 store_error`. Fails before U3.
  - **Orchestrator state preserved (R3):** in the Deny `Get`-failure and an `Update`-failure case, assert `s.approvals.Get(ctx, approvalID)` still shows the denied/modified approval state after the `500` — proving detection-not-rollback.
- **Execution note:** Write the Deny `Get`-failure test first and confirm it fails against current `main` (the swallowed-error bug), then apply U2 and watch it pass.
- **Verification:** `go test ./internal/app/...` (and `task test`) green; new tests fail when U2/U3 are reverted.

---

## System-Wide Impact

- **API consumers:** Deny now returns `500` on intent-store failure where it previously returned a misleading `200`. This is a corrected failure contract, not a breaking change to the success path. Documented in the spec (U4).
- **`apiServer` struct:** `intents` field becomes interface-typed (U1). No behavioral change; other store fields could follow the same pattern later but that's not in scope.
- **Audit/reconciliation:** Behavior now matches the issue's stated expectation — a `500` signals divergence and the next reconciliation/retry re-attempts the intent update. No new reconciliation code is added.

---

## Risks & Dependencies

- **Risk: field-type widening breaks an unseen concrete-type usage.** Mitigation: grep confirmed only interface methods are used and both assignment sites pass `mem.NewIntentStore()`. U1 is build-verified before dependent units.
- **Risk: `task generate:api` produces unrelated diff churn.** Mitigation: review the `server.gen.go` diff is limited to the three endpoints' `500` additions; if codegen reformats more, raise it rather than hand-editing.
- **Dependency: regeneration tooling.** U4 requires `task generate:api` per `CLAUDE.md`; never hand-edit `server.gen.go`.

---

## Sources & Research

- GitHub issue #955 — locations, suggested fix, and test expectations (origin).
- `internal/app/handlers.go:141` (`writeError` envelope), `:372`/`:410` (guard-clause pattern), `:457-604` (the three handlers).
- `internal/app/handlers_execution_test.go:321,397` (orchestrator+approval test scaffolding to extend).
- `internal/store/store.go:18` (`IntentStore` interface); `internal/store/mem/intent.go` (no fault hooks — motivates KTD2).
- `internal/api/openapi.yaml:1492/1521/1550` (endpoints), `:4006-4029` (response components — motivates KTD3).
- `CLAUDE.md` — OpenAPI spec is source of truth; regenerate via `task generate:api`; bug fixes require regression tests.
