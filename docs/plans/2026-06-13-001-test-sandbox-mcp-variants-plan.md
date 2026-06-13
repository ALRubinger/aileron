---
title: "test: sandbox MCP integration variants (in-container, never-approved, concurrency)"
type: test
status: active
date: 2026-06-13
issue: 960
origin: GitHub issue #960
---

# test: sandbox MCP integration variants (in-container, never-approved, concurrency)

## Summary

Extend the existing `integration_sandbox` test (`internal/app/sandbox_mcp_test.go`) with the three variants deferred from #953: a real **in-container** `aileron-mcp` run, a **never-approved** edge case (R6b), and a **concurrency** edge case (R6c). The existing host-subprocess round-trip is **kept** as the always-on path; the in-container variant runs only when Docker is on `PATH`. R6b and R6c run on the host-subprocess transport (no Docker needed), since their contract — pending-stays-pending and per-`approval_id` event attribution — is transport-independent.

This is a test-only change. No production code is modified.

---

## Problem Frame

`internal/app/sandbox_mcp_test.go` today proves the load-bearing R6/R7 contract (MCP `tools/call` → in-process daemon round-trip → session-id-stamped audit chain) with `aileron-mcp` running as a **host subprocess** pointing at the daemon's loopback address. Three variants were deliberately deferred (see the file's `TODO(#953)` block):

1. **Runtime realism** — the real deployment runs `aileron-mcp` *inside* a container reaching the daemon via `host.docker.internal`, with the binary bind-mounted read-only and `--add-host` on Linux. The host-subprocess shape never exercises container networking or the bind-mount.
2. **R6b never-approved** — an approval-required action whose approval is never decided must stay pending, emit no `execution.*` events, and not mutate daemon state.
3. **R6c concurrency** — two parallel `draft_email` calls must produce two distinct `approval_id`s, and approving them in reverse order must emit correctly attributed per-call event chains, with the upstream stub seeing exactly two requests.

The goal is coverage, not a contract change: R6/R7 already hold; these variants add container-runtime realism plus two contractual edge cases the host shape can also exercise but that were never written.

---

## Requirements

Traced to #960's scope checklist:

- **R1** — In-container round-trip: launch `aileron-mcp` via `docker run -i --rm`, binary bind-mounted at `/usr/local/bin/aileron-mcp:ro`, daemon reachable at `host.docker.internal:<port>`, `--add-host=host.docker.internal:host-gateway` on Linux. Assert the same R6/R7 chain the host transport asserts.
- **R2** — Docker skip guard: if `docker` is not on `PATH`, skip only the in-container variant (the host, R6b, and R6c variants still run).
- **R3** — Base-image pre-pull in `TestMain` (a known-good glibc image with `/bin/sh`, e.g. `debian:bookworm-slim`) to mitigate registry rate-limiting on CI runners.
- **R4** — Daemon bound to a Docker-reachable, non-loopback address (`0.0.0.0:0`) so the container can reach it; host transport keeps using a loopback-equivalent URL.
- **R5 (R6b)** — Never-approved variant: after `tools/call`, two `check_action_status` polls 100ms apart return pending; no `execution.*` events emit; the approval entry is unchanged; entry deleted as cleanup.
- **R6 (R6c)** — Concurrency variant: two parallel `tools/call draft_email` yield two distinct `approval_id`s; approving in reverse order emits the right per-`approval_id` event chain; the upstream stub records exactly two requests with the correct per-call args.
- **R7** — Update the file's `TODO`/doc comment to record what is now covered and what (if anything) remains deferred.

---

## High-Level Technical Design

The in-container variant only changes *where* `aileron-mcp` runs and *how it reaches the daemon*; the JSON-RPC-over-stdio protocol and the audit assertions are unchanged.

```mermaid
sequenceDiagram
    participant T as test (host process)
    participant D as in-process daemon<br/>(httptest on 0.0.0.0:PORT)
    participant C as docker run -i<br/>aileron-mcp (container)
    participant U as upstream stub<br/>(httptest)
    T->>C: spawn; write JSON-RPC on stdin
    C->>D: HTTP via host.docker.internal:PORT<br/>(--add-host on Linux)
    D->>U: connector call (draft_email)
    U-->>D: 200
    D-->>C: tools/call result
    C-->>T: JSON-RPC on stdout
    T->>D: read audit MemStore → assert R6/R7 chain
```

The host transport is identical except the middle two participants collapse: `aileron-mcp` runs as a host subprocess and reaches the daemon at `127.0.0.1:PORT`.

---

## Key Technical Decisions

- **KTD1 — Transport seam, keep both.** Introduce a transport notion (`host` | `container`) that `spawnMCP` switches on. The host path is the existing `exec.CommandContext(mcpBinaryPath)`; the container path is `docker run -i --rm ... <image> /usr/local/bin/aileron-mcp`. Both return the same `*mcpProcess` wrapping stdin/stdout pipes, so every downstream helper (`initializeMCP`, `callTool`, `request`) is transport-agnostic. Rationale: preserves the fast, Docker-free round-trip coverage while adding runtime realism; the alternative (replace host with container) would make the whole suite Docker-only and lose dev-machine/CI-without-Docker coverage. (User-confirmed: keep both.)
- **KTD2 — Bind the daemon to `0.0.0.0:0` for reachability.** `httptest.NewServer` binds `127.0.0.1`, which a container cannot reach. Use `httptest.NewUnstartedServer` + a custom `net.Listen("tcp", "0.0.0.0:0")` listener, then `Start()`. Derive two URLs from the chosen port: `127.0.0.1:PORT` for the host transport, `host.docker.internal:PORT` for the container transport. Rationale: one daemon serves both transports; the bind address is the only networking change.
- **KTD3 — R6b/R6c run on the host transport.** Their contract (pending-stays-pending; per-`approval_id` attribution) is independent of where `aileron-mcp` runs, and the host transport is Docker-free and faster/less flaky. Only the round-trip variant (R1) needs the container to prove networking + bind-mount. Rationale: maximizes coverage per CI minute and keeps the Docker-gated surface minimal.
- **KTD4 — Per-`approval_id` event correlation via a new helper.** Events already carry `approval_id` in `Payload` (see `internal/approval/action_queue.go:611`). Add `eventChainForApproval(store, approvalID)` mirroring the existing `eventChainForSession` but keying on `Payload["approval_id"]`. Rationale: R6c needs per-call attribution within one session; session-keyed filtering can't distinguish the two concurrent calls.
- **KTD5 — Docker gating via `exec.LookPath("docker")`.** Skip the in-container variant (not the whole test) when Docker is absent. Rationale: simplest guard, no new import; matches #960's "skip the in-container variant" wording. (`internal/sandbox/container.ResolveRuntime` is the launch-side equivalent but pulls a heavier dependency into `package app`'s test; `LookPath` is sufficient here.)

---

## Implementation Units

### U1. Transport seam, 0.0.0.0 daemon bind, Docker skip guard, base-image pre-pull

**Goal:** Establish the harness changes every variant builds on: a `host|container` transport seam in `spawnMCP`, a Docker-reachable daemon bind, a skip guard, and a `TestMain` image pre-pull.

**Requirements:** R2, R3, R4 (enables R1).

**Dependencies:** none.

**Files:**
- `internal/app/sandbox_mcp_test.go` (modify: `TestMain`, `newDaemonHarness`, `spawnMCP`)

**Approach:**
- Add a `dockerAvailable()` helper (`exec.LookPath("docker")`) and a `skipWithoutDocker(t)` guard.
- In `TestMain`, when Docker is available, `docker pull debian:bookworm-slim` (a glibc image with `/bin/sh`) once, before `m.Run()`; log-and-continue on failure so a pull failure surfaces as a clear skip rather than a per-test hang.
- Change `newDaemonHarness` to start the daemon on a `0.0.0.0:0` listener via `httptest.NewUnstartedServer` + custom `net.Listener`, then expose both a `loopbackURL` (`127.0.0.1:PORT`) and a `containerURL` (`host.docker.internal:PORT`). Existing tests use `loopbackURL`.
- Refactor `spawnMCP(t, sessionID, token)` to `spawnMCP(t, transport, sessionID, token)` (or a small options struct). `transport=host` keeps today's behavior; `transport=container` runs `docker run -i --rm --add-host=host.docker.internal:host-gateway` (Linux only for `--add-host`; gate via `runtime.GOOS`) `-v <mcpBinaryPath>:/usr/local/bin/aileron-mcp:ro -e AILERON_URL=<containerURL> -e AILERON_SESSION_ID=... -e AILERON_TOKEN=... <image> /usr/local/bin/aileron-mcp`, wiring the container's stdin/stdout into the same `*mcpProcess`.

**Patterns to follow:**
- `internal/launch/run_with_proxy_ca_integration_test.go` and `internal/launch/authspec_bindmount_integration_test.go` — Docker skip-guard, `docker run` argv assembly, and container-stdio handling under the same `integration_sandbox` tag.
- The existing `spawnMCP` / `mcpProcess` (lines ~189-338) for the stdio wrapper that must stay transport-agnostic.

**Test scenarios:**
- Existing host-transport tests continue to pass unchanged through the refactored `spawnMCP(host, ...)` and `loopbackURL` (regression guard).
- `dockerAvailable()` returns false → in-container call sites skip cleanly (verified indirectly via U2).
- Test expectation: no new standalone assertions in U1 beyond preserving the existing suite; U1 is harness scaffolding consumed by U2-U4.

**Verification:** `task test:integration:sandbox` still green with the refactored harness; the three existing tests run via `transport=host`.

---

### U2. In-container round-trip variant (R1)

**Goal:** Prove the R6/R7 round-trip with `aileron-mcp` running inside a real container.

**Requirements:** R1 (advances #960 in-container checkbox).

**Dependencies:** U1.

**Files:**
- `internal/app/sandbox_mcp_test.go` (add: `TestSandboxMCP_InContainer_NoApproval_RoundTripsAndEmitsAuditChain` or a transport-parameterized run of the existing no-approval round-trip)

**Approach:** Reuse the existing no-approval round-trip assertions, driven through `spawnMCP(container, ...)`. Guard with `skipWithoutDocker(t)`. The daemon uses `containerURL`. Assert the same chain the host variant asserts; the only new surface is networking + bind-mount, so the assertion set is intentionally identical.

**Patterns to follow:** `TestSandboxMCP_NoApproval_RoundTripsAndEmitsAuditChain` (line ~440) for the assertion shape; mirror its `waitForChain` / `eventChainForSession` usage.

**Test scenarios:**
- Covers F (sandbox MCP parity) / R6. Docker present: `tools/call draft_email` in-container returns non-error; audit chain for the session equals the host variant's chain (`execution.started → execution.succeeded`, no approval events for the no-approval manifest), stamped with the launch session id.
- Docker absent: the test skips with a clear reason (no failure).
- Container cannot reach daemon (negative realism): if `--add-host` / bind is wrong the round-trip times out — the existing `waitForChain` timeout surfaces it; ensure the failure message names the container transport.

**Verification:** With Docker present, the in-container round-trip passes on macOS (Docker Desktop) and Linux (`--add-host`); without Docker it skips.

---

### U3. Never-approved variant (R6b)

**Goal:** Prove an approval-required action left undecided stays pending with no execution side effects.

**Requirements:** R5.

**Dependencies:** U1.

**Files:**
- `internal/app/sandbox_mcp_test.go` (add: `TestSandboxMCP_NeverApproved_StaysPendingNoExecution`)

**Approach:** Use the approval-required manifest (`draftEmailManifestApproval`), host transport. `tools/call draft_email` → capture `approval_id`. Poll `check_action_status` twice, 100ms apart; assert each returns the pending status word and never a terminal payload. Never call `Decide`. Assert the session's audit chain contains `approval.requested` but no `execution.*` event, and the approval entry is still pending in the store. Delete the entry as cleanup.

**Patterns to follow:** `TestSandboxMCP_Approval_RoundTripsWithApprovedDecide` (line ~484) for obtaining the entry/`approval_id` and the approval store handle (`h.srv.actionApprovals`); `check_action_status` tool behavior in `cmd/aileron-mcp/main.go:887` (returns a status-word text block; `pending` for undecided).

**Test scenarios:**
- Covers R6b. After `tools/call`: `check_action_status` poll #1 → "pending"; 100ms later poll #2 → still "pending".
- `eventChainForSession` contains `approval.requested` and contains **no** `execution.started` / `execution.succeeded` / `execution.failed`.
- The approval entry remains in pending state in `actionApprovals` (no spurious decide/state change).
- Cleanup: deleting the pending entry succeeds and leaves no lingering state.

**Verification:** Test passes without Docker; asserts pending + absence of execution events deterministically (no reliance on timing beyond the two bounded polls).

---

### U4. Concurrency variant (R6c)

**Goal:** Prove two concurrent approval-required calls get distinct `approval_id`s and correctly attributed per-call event chains.

**Requirements:** R6.

**Dependencies:** U1; adds the `eventChainForApproval` helper (KTD4).

**Files:**
- `internal/app/sandbox_mcp_test.go` (add: `TestSandboxMCP_Concurrency_DistinctApprovalsAttributedCorrectly`; add helper `eventChainForApproval`)

**Approach:** Issue two `tools/call draft_email` requests concurrently (host transport), with distinct per-call args (e.g. different recipients) so the upstream stub can distinguish them. Collect the two `approval_id`s; assert they differ. Approve in **reverse** order via `h.srv.actionApprovals.Decide`. Assert each `approval_id`'s event chain (via `eventChainForApproval`) is the correct `approval.requested → approval.approved → execution.started → execution.succeeded`. Assert the upstream stub recorded exactly two requests with the correct per-call args.

**Approach note (deferred to implementation):** Achieving genuine concurrency over JSON-RPC stdio requires either id-multiplexing over one `mcpProcess` connection or two connections/processes. The contract under test (two distinct `approval_id`s + per-`approval_id` attribution) is independent of the multiplexing mechanism — pick the mechanism during implementation; if one connection's synchronous `request` cannot overlap calls, use two `spawnMCP(host, ...)` connections in the same session. Record the choice in a code comment.

**Patterns to follow:** `eventChainForSession` (line ~402) as the template for `eventChainForApproval` (swap `Payload["aileron.session.id"]` for `Payload["approval_id"]`); the upstream stub's request recording in `newDaemonHarness` for the "exactly two requests" assertion.

**Test scenarios:**
- Covers R6c. Two concurrent `tools/call` → two non-empty, **distinct** `approval_id`s.
- Approve B then A (reverse order): `eventChainForApproval(A)` and `eventChainForApproval(B)` each equal `[approval.requested, approval.approved, execution.started, execution.succeeded]`; no cross-attribution (A's chain contains no B events).
- Upstream stub recorded exactly two requests; each carries the recipient passed to its originating call (per-call arg fidelity).
- Edge: if both calls collapsed to one `approval_id`, the distinct-id assertion fails loudly (guards against accidental dedup).

**Verification:** Test passes without Docker; per-`approval_id` chains are attributed correctly regardless of approval order.

---

### U5. Update TODO and doc comment

**Goal:** Make the file's header reflect reality — what is now covered, what remains deferred.

**Requirements:** R7.

**Dependencies:** U2, U3, U4.

**Files:**
- `internal/app/sandbox_mcp_test.go` (modify: the package doc comment + `TODO(#953)` block)

**Approach:** Replace the `TODO(#953)` block with a short statement that the in-container, R6b, and R6c variants now exist; carry forward only genuinely deferred items (e.g. per-agent E2E beyond `aileron-mcp`, CI-pipeline integration) with their tracking refs. Note the host transport remains the always-on path and the container variant is Docker-gated.

**Test scenarios:** Test expectation: none — documentation-only change.

**Verification:** The header accurately describes the variants and gating; no stale "deferred" claims for work now done.

---

## Risks & Dependencies

- **Registry rate-limiting on CI (R-risk5, from #960).** Mitigated by pulling the base image once in `TestMain` and skipping (not failing) when Docker/the pull is unavailable. Use a small known-good glibc image (`debian:bookworm-slim`); distroless static images lack `/bin/sh` the MCP process path may need.
- **Container → daemon reachability differs by OS.** macOS/Windows Docker Desktop provide `host.docker.internal` natively; Linux needs `--add-host=host.docker.internal:host-gateway`. Gate the `--add-host` flag on `runtime.GOOS == "linux"`. Binding the daemon to `0.0.0.0:0` is required for any container reachability.
- **Concurrency over stdio (U4).** A single synchronous JSON-RPC connection may not overlap calls; the mechanism (id-multiplexing vs. two connections) is deferred to implementation. The asserted contract is mechanism-independent.
- **0.0.0.0 bind exposure.** The test daemon listens on all interfaces on an ephemeral port for the test's lifetime only. Acceptable for a build-tag-gated integration test; do not carry this binding into non-test code.
- **Flakiness.** Container startup adds latency; reuse the existing `waitForChain` timeout (bump only if needed) and ensure skip/cleanup paths don't leak containers (`--rm` + context cancellation on the `docker run`).

---

## Scope Boundaries

**In scope:** the three variants above, the harness changes that enable them, and the doc/TODO update — all within `internal/app/sandbox_mcp_test.go` (plus the one new helper in that file).

**Out of scope (from #960):**
- Per-agent E2E variants beyond `aileron-mcp` (Codex / Goose / OpenCode / Pi as MCP client) — unit coverage + the manual recipe (#962) cover those.
- CI-pipeline integration of the `integration_sandbox` tag — filed separately once the variants land locally.

### Deferred to Follow-Up Work
- Wiring `task test:integration:sandbox` (with the new Docker-gated variant) into a CI job — separate issue, after these land and prove stable locally.

---

## Sources & Research

- #960 (issue body) — the variant checklist and explicit out-of-scope.
- `internal/app/sandbox_mcp_test.go` — existing harness: `TestMain` (builds `aileron-mcp`), `newDaemonHarness` (loopback `httptest` daemon + audit `MemStore` + upstream stub), `spawnMCP`/`mcpProcess` (JSON-RPC over stdio), `eventChainForSession`/`waitForChain`, and the three existing R6/R7 tests.
- `cmd/aileron-mcp/main.go:285,477,887` — `check_action_status` tool (always available; requires `approval_id`; returns a status-word text block) confirming R6b is expressible.
- `internal/approval/action_queue.go:611` — approval events carry `approval_id` in `Payload`, confirming R6c per-call correlation is expressible via a sibling of `eventChainForSession`.
- `internal/launch/run_with_proxy_ca_integration_test.go`, `internal/launch/authspec_bindmount_integration_test.go` — established `integration_sandbox` Docker-gated patterns (skip guard, `docker run` argv, base-image build/pull, container stdio) to mirror.
