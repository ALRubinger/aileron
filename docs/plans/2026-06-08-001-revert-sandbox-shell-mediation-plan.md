---
title: "revert: drop sandbox shell-layer mediation (#801)"
status: active
created: 2026-06-08
type: revert
tracking: ["#952", "#801", "#747"]
adrs: ["ADR-0021"]
predecessor_pr: "#950"
---

# revert: drop sandbox shell-layer mediation (#801)

## Summary

Fully remove the sandbox shell-layer mediation surface added by #801 slices 1–6. The strategic decision (recorded in #747 and #952, dated 2026-06-08) is that the architecture has no named user buyer and ADR-0021 acknowledges four bypass paths the deny trap does not close. The risks the trap was meant to address are covered by container isolation, git, the HTTPS proxy (#896), and tool-level HITL at the MCP/action boundary.

This is **option 1c** from the strategy conversation: a full rip-out, not a "log instead of gate" middle path. Anything that proves needed later returns under a fresh ADR.

Shape: a single PR titled `revert: drop shell-layer mediation (#801)`, squash-merged with `--admin --delete-branch`. On merge, close #801 and #952. ADR-0021 stays in the tree as `Status: Withdrawn` and the slice-6 plan flips to `status: withdrawn` — both retained as historical records.

---

## Problem Frame

Slices 1–6 of #801 introduced a shell-layer deny path: a sandbox image with `aileron-shell-wrapper` baked in as `/usr/local/bin/bash`, a `BASH_ENV`-sourced `aileron-bashrc` that installs an `extdebug` DEBUG trap, the trap calling `aileron-shell-mediator` over an HTTP decision endpoint (`/v1/sandbox-shell/decide`) backed by a regex deny pattern, and the launcher activating mediation by injecting `BASH_ENV`/`SHELL` for the agent session.

The strategic call recorded in #952 is that this surface earns no buyer: the design's own ADR-0021 names four bypass paths (env strip, recursion-guard tampering, interactive-shell spawn, `eval`/`command`/`exec`/function indirection) and the named risks are better covered elsewhere in the stack. Leaving the surface in carries ongoing maintenance, additional sandbox-image content, additional CI cost, and a misleading defense-in-depth story.

The rip-out spans six surfaces: the image layer, the daemon HTTP handler, the OpenAPI contract, the launcher env injection, the container/runtime validation plumbing, and CI. Two docs (ADR-0021 and `sandbox-composition.md`/`sandbox-agent-images.md`) are trimmed back; the slice-6 plan is reclassified.

---

## Requirements

Cited verbatim where applicable from #952's acceptance criteria; all R-IDs are plan-local.

- **R1.** No code or workflow content references the shell-mediation surface after this PR lands. `git grep -i "shell.mediator\|aileron-bashrc\|sandbox.shell\|SANDBOX_SHELL_DENY"` returns zero hits anywhere in the working tree except `docs/plans/` and the withdrawn ADR-0021 (historical records).
- **R2.** `task generate:api` regenerates cleanly with no residual diff. The `api.X` → `api.IntentStatusX` rename forced by #950 self-reverts where `pending_approval` is no longer ambiguous; any other regen drift is surfaced before continuing.
- **R3.** `task vet:go` and the full Go test suite (`go test ./...` across `internal/`, `cmd/aileron/`, `cmd/aileron-mcp/`, `cmd/aileron-enclave/`, `sdk/go/`) pass.
- **R4.** `images/sandbox-base/Containerfile` builds (`docker build -f images/sandbox-base/Containerfile -t aileron-sandbox-base:smoke images/sandbox-base`) and `docker run --rm aileron-sandbox-base:smoke bash -c 'echo hi'` runs cleanly. Inside the image, `which bash` resolves to whatever Alpine ships (`/bin/bash`) — the wrapper-vs-real-bash distinction goes away.
- **R5.** `aileron launch --sandbox=docker <agent>` smoke-launches an agent in a container with no shell-mediation env present, no wrapper validation gating startup, and no `/v1/sandbox-shell/decide` endpoint exposed.
- **R6.** ADR-0021 status is `Withdrawn` with a one-paragraph rationale pointer at the top of `Context`. `sandbox-composition.md` no longer claims shell mediation is active or planned; `sandbox-agent-images.md` no longer references the shell-mediation image contract.
- **R7.** `coderabbit review --agent --base main` returns zero findings.

---

## Scope Boundaries

In scope: every surface enumerated under Implementation Units below.

### Deferred to Follow-Up Work

- Phase 2 sandbox MCP parity (Path B1 + E2E test) — tracked in its own issue.
- Touching #802's body — separate strategic decision pending.

### Outside this product's identity

- Re-architecting the shell layer or any new shell-mediation work. Anything that returns later comes under a fresh ADR.

---

## Key Technical Decisions

### KTD1. Full removal, not soft-disable.

The rip-out deletes files and code rather than gating them behind a feature flag or "log instead of gate" mode. Rationale: leaving the surface dormant carries the same maintenance and image-content cost as keeping it active, and the decision to remove is strategic (no named buyer), not technical. A flag would invite re-enablement under a stale rationale. Anything that returns later returns under a fresh ADR per #952.

### KTD2. OpenAPI spec is the source of truth — strip the spec first, then regenerate.

Per `CLAUDE.md`, the spec drives `internal/api/gen/server.gen.go`. The plan strips `/v1/sandbox-shell/decide` and the `SandboxShellDecisionRequest` / `SandboxShellDecisionResponse` schemas from `internal/api/openapi.yaml` first, then runs `task generate:api`. The generated diff is verified before continuing to dependent surfaces.

No `api.IntentStatusX` self-revert is expected. `pending_approval` remains in at least three other enums after the removal — the long `IntentStatus` enum (`openapi.yaml:4189`), the `ActionApprovalRequiredResponse.status` discriminator (`:6207`), and the `ActionApprovalResult.status` enum (`:6247`) — so `oapi-codegen`'s type-prefix disambiguation stays. The expected regen diff is the removal of `SandboxShellDecisionRequest`, `SandboxShellDecisionResponse`, and `SandboxShellDecisionResponseDecision`, plus the corresponding handler interface method on the generated server. Any other drift is surfaced and reviewed before continuing.

### KTD3. ADR-0021 stays in the tree as `Status: Withdrawn`; slice-6 plan flips to `status: withdrawn`.

Both documents are retained as historical records. ADR-0021 preserves link history for anyone clicking through old PRs; deleting it would break those references. The slice-6 plan (`docs/plans/2026-06-05-001-feat-sandbox-shell-deny-plan.md`) keeps its body intact and only the frontmatter `status` flips from `completed` to `withdrawn`. The acceptance criterion R1 explicitly excludes `docs/plans/` and the withdrawn ADR from the zero-trace grep.

### KTD4. Plan scope expands beyond the issue body to fully zero `git grep`.

The issue body's daemon-layer scope does not name `RequireShellMediation` on `ValidateOptions` and the `boolArg(opts.RequireShellMediation)` plumbing in `internal/sandbox/container/runtime.go`, the corresponding `${4:-0}` mediator-check block in its embedded validation script, or `internal/sandbox/container/shellmediator_test.go`. The plan absorbs these into U4 because R1 ("zero hits") cannot be honored otherwise. Same intent as the issue, broader surface coverage. Confirmed with the issue author at planning time.

### KTD5. One PR, ordered by dependency for review readability.

The PR is a single conventional-commit `revert: drop shell-layer mediation (#801)`. Within the PR, work lands spec-first (so server.gen.go regen anchors the daemon changes), then daemon → launcher → container runtime → image → CI → docs → verification. The ordering is for review clarity, not separability — the PR is squash-merged.

---

## High-Level Technical Design

The current shell-mediation surface and the rip-out targets:

```mermaid
flowchart TD
    subgraph Launch[Host / Launcher]
      A[aileron launch] --> B[launcher.go inject<br/>BASH_ENV, SHELL, MEDIATION env]
      B --> C[container/runtime.go<br/>RequireShellMediation validate]
    end
    subgraph Image[sandbox-base image]
      D[Containerfile bakes<br/>aileron-shell-wrapper as /usr/local/bin/bash]
      E[aileron-bashrc<br/>extdebug DEBUG trap]
      F[aileron-shell-mediator<br/>--check + decide call]
    end
    subgraph Daemon[Aileron daemon]
      G[/v1/sandbox-shell/decide/<br/>handlers_sandbox_shell.go]
      H[sandboxShellDenyPattern regex]
      I[EventTypeSandboxShellDecided emit]
    end
    subgraph Spec[OpenAPI]
      J[openapi.yaml<br/>SandboxShellDecisionRequest / Response]
      K[server.gen.go]
    end
    subgraph CI[GitHub Actions]
      L[ci.yml Shellcheck sandbox shell scripts]
      M[sandbox-base.yml smoke<br/>allow stub + deny stub + pty cases]
    end

    B --> E
    E --> F
    F --> G
    G --> H
    G --> I
    J --> K
    G --> K
    classDef rm fill:#fee,stroke:#c00,color:#900;
    class B,C,D,E,F,G,H,I,J,L,M rm;
```

Every red node is deleted. The post-state has:

- `internal/api/openapi.yaml` without the `/v1/sandbox-shell/decide` path or `SandboxShellDecision*` schemas.
- `internal/app/` without the handler file, the test file, the `apiServer.sandboxShellDenyPattern` field, the `loadSandboxShellDenyPattern()` call, the `regexp` import, and the `EventTypeSandboxShellDecided` constant.
- `internal/launch/launcher.go` without the `sandboxShellRCPath` / `sandboxShellWrapper` constants, the `sandboxShellMediationEnv` constant, the `sandboxShellMediationEnabled()` helper, the `BASH_ENV` / `SHELL` injection block, and `RequireShellMediation` passthrough.
- `internal/sandbox/container/runtime.go` without the `RequireShellMediation` field on `ValidateOptions`, the `boolArg(opts.RequireShellMediation)` arg, and the `${4:-0}` mediator-check block in the embedded validation script.
- `images/sandbox-base/Containerfile` without the three `COPY` lines and the lines that bake the wrapper as `/usr/local/bin/bash` and `/usr/local/bin/sh`.
- `.github/workflows/ci.yml` without the `Shellcheck (sandbox shell scripts)` step.
- `.github/workflows/sandbox-base.yml` reverted to the slice-5-era smoke step (allow + closed-port + mediation-off), with the deny stub, deny smoke cases, and `script -qfc` pty case removed.

---

## Implementation Units

### U1. Strip the OpenAPI spec and regenerate

**Goal.** Remove the `/v1/sandbox-shell/decide` path and the `SandboxShellDecisionRequest` / `SandboxShellDecisionResponse` schemas from the spec, then regenerate the Go server interface.

**Requirements.** R1, R2.

**Dependencies.** None — first work because the spec is the source of truth (KTD2).

**Files.**

- `internal/api/openapi.yaml` (modify)
- `internal/api/gen/server.gen.go` (regenerated, not hand-edited)

**Approach.**

- Delete the `/v1/sandbox-shell/decide` path entry from `paths:` (around `openapi.yaml:2769`).
- Delete the `SandboxShellDecisionRequest` and `SandboxShellDecisionResponse` schema definitions from `components.schemas:` (around `openapi.yaml:6414` and `:6439`).
- Run `task generate:api`.
- Diff `server.gen.go`. Expected: the path and the `SandboxShellDecision*` types disappear and the handler interface method goes with them. The `api.IntentStatusX` disambiguation forced by #950 is **not** expected to self-revert — `pending_approval` remains in three other enums (see KTD2). Any drift beyond the named removals is surfaced before continuing.

**Patterns to follow.** Existing `task generate:api` workflow; never hand-edit `server.gen.go`.

**Test scenarios.**

- Covers R2. `task generate:api` is rerun after the spec edit; the resulting `server.gen.go` has no `SandboxShellDecision*` types and no `/v1/sandbox-shell/decide` route. A second `task generate:api` produces no diff (idempotent).
- After regeneration, `go build ./...` compiles cleanly across all packages that previously imported the removed types.

**Verification.** `task generate:api` exits 0 with no remaining diff after a second run.

---

### U2. Remove the daemon shell-decision handler and supporting plumbing

**Goal.** Delete the HTTP handler, its test file, the regex field on `apiServer`, the loader call, the event-type constant, and any now-unused imports.

**Requirements.** R1, R3.

**Dependencies.** U1 (the generated interface no longer declares the handler method).

**Files.**

- `internal/app/handlers_sandbox_shell.go` (delete)
- `internal/app/handlers_sandbox_shell_test.go` (delete)
- `internal/app/handlers.go` (modify — strip `regexp` import if unused, strip `sandboxShellDenyPattern` field and its doc comment around `:125`–`:131`)
- `internal/app/app.go` (modify — strip the `loadSandboxShellDenyPattern()` call around `:396` and the `sandboxShellDenyPattern:` assignment around `:405` in `NewHandlerWithConfig`; if `loadSandboxShellDenyPattern` was defined locally, delete its definition and unit tests too)
- `internal/model/model.go` (modify — delete `EventTypeSandboxShellDecided EventType = "sandbox.shell.decided"` around `:311`)

**Approach.**

- Confirm `regexp` is no longer used anywhere else in `handlers.go` before removing the import (`goimports`/`task vet:go` will catch this if missed).
- Confirm `loadSandboxShellDenyPattern` is not referenced anywhere else; if it lived in its own file with its own unit test, delete that file and its test as well. Surface for review if removal scope expands beyond `app.go`.
- Confirm `model.EventTypeSandboxShellDecided` has zero remaining references after U2 (it had two: `handlers_sandbox_shell.go` and `handlers_sandbox_shell_test.go`, both deleted).

**Patterns to follow.** Existing handler-deletion patterns from prior #525 daemon-side Slack removal (see memory `reference_slack_daemon_vs_connector`).

**Test scenarios.**

- Covers R1, R3. `go test ./internal/app/...` passes after the deletions.
- `grep -rn "sandboxShellDenyPattern\|loadSandboxShellDenyPattern\|EventTypeSandboxShellDecided" internal/app/ internal/model/` returns zero matches.
- `grep -n "\"regexp\"" internal/app/handlers.go` returns zero matches if no other code in the file uses it.

**Verification.** Both files in scope compile and test cleanly; the daemon no longer serves `/v1/sandbox-shell/decide` (the generated interface no longer requires it).

---

### U3. Remove launcher env injection and mediation opt-in plumbing

**Goal.** Strip the `BASH_ENV` / `SHELL` injection block, the `AILERON_SANDBOX_SHELL_MEDIATION` opt-in, the wrapper-presence validation, and supporting constants and helpers from `launcher.go`. Update `launcher_test.go` to drop the corresponding assertions.

**Requirements.** R1, R3, R5.

**Dependencies.** None against U1/U2 (separate package) — sequenced after them for review readability.

**Files.**

- `internal/launch/launcher.go` (modify)
- `internal/launch/launcher_test.go` (modify)

**Approach.**

- Delete the constants block carrying `sandboxShellRCPath` and `sandboxShellWrapper` around `launcher.go:44`–`:45`.
- Delete the `sandboxShellMediationEnv` constant around `:48`.
- Delete the `sandboxShellMediationEnabled()` helper around `:414`–`:416`.
- Delete the mediation env injection block around `:344`–`:353` (`if sandboxShellMediationEnabled() { agentEnv["BASH_ENV"] = ..., agentEnv["SHELL"] = ..., AILERON_SHELL_RCFILE = ... }`).
- Delete the `RequireShellMediation: agentEnv[sandboxShellMediationEnv] == "1"` line at `:239` (or, equivalently, set `RequireShellMediation: false`; cleaner to remove the line if U4 also removes the field — see KTD4).
- Update `launcher_test.go` to drop assertions on `BASH_ENV=`, `SHELL=/usr/local/bin/bash`, `AILERON_SANDBOX_SHELL_MEDIATION=1`, `AILERON_SHELL_RCFILE=`, and `AILERON_REAL_SHELL=` env-presence checks added across slices 5–6 (around `:320`, `:366`–`:372`, `:388`, `:422`–`:424`).

**Patterns to follow.** Existing slice-5 negative-test patterns at `launcher_test.go:388`–`:424` that already assert the *absence* of these env entries when mediation is off — extend the assertion to the always-absent case and delete the now-unreachable mediation-on cases.

**Test scenarios.**

- Covers R5. Default-path launcher test asserts: no `BASH_ENV` in agent env, no `SHELL=/usr/local/bin/bash` override, no `AILERON_SANDBOX_SHELL_MEDIATION` value present, no `AILERON_SHELL_RCFILE` present.
- `SHELL=/bin/zsh` from the user's environment still flows through unchanged (preserve the existing slice-5 assertion at `:105`–`:107`).
- The historical mediation-on path no longer exists, so any test that previously set `t.Setenv("AILERON_SANDBOX_SHELL_MEDIATION", "1")` is removed entirely rather than inverted — the env name no longer carries meaning.
- `go test ./internal/launch/...` passes.

**Verification.** `go test ./internal/launch/...` is clean; the test file no longer references any shell-mediation symbol.

---

### U4. Remove sandbox/container runtime mediation validation plumbing

**Goal.** Strip `RequireShellMediation` from `ValidateOptions`, drop the `boolArg(opts.RequireShellMediation)` argument from the validation script invocation, and delete the `${4:-0}` mediator-check block from the embedded validation script. Delete `shellmediator_test.go`, `shellrc_test.go`, and `shellrc_alpine_test.go` entirely — all three files exist only because of slices 4–6 and contain no non-mediation coverage.

**Requirements.** R1, R3, R5.

**Dependencies.** U3 (launcher no longer sets `RequireShellMediation`).

**Files.**

- `internal/sandbox/container/runtime.go` (modify — remove the `RequireShellMediation bool` field on `ValidateOptions` around `:106`, the `boolArg(opts.RequireShellMediation)` arg around `:296`, and the `${4:-0}` block in the embedded `validationScript` around `:366`–`:371`)
- `internal/sandbox/container/runtime_test.go` (modify — drop any test cases that exercise the now-removed validate arg / script block)
- `internal/sandbox/container/shellmediator_test.go` (delete entirely)
- `internal/sandbox/container/shellrc_test.go` (delete entirely — created by #948 and extended by #950; every test exercises the bashrc DEBUG trap and mediator interaction, and the file depends on helpers defined in `shellmediator_test.go`)
- `internal/sandbox/container/shellrc_alpine_test.go` (delete entirely)

**Approach.**

- Remove `RequireShellMediation` from `ValidateOptions` and the corresponding positional `boolArg` in the validation command.
- In the embedded `validationScript`, delete the `if [ "${4:-0}" = "1" ]; then ... aileron-shell-mediator --check ... fi` block. The script's positional arg count drops from 4 to 3; review any other call sites to confirm none pass a fourth arg.
- Delete `shellmediator_test.go`, `shellrc_test.go`, and `shellrc_alpine_test.go` outright. Each was created during slices 4–6 (verify via `git log --diff-filter=A --oneline -- internal/sandbox/container/shellmediator_test.go internal/sandbox/container/shellrc_test.go internal/sandbox/container/shellrc_alpine_test.go`) and exists only to test the bashrc DEBUG trap and mediator interaction.

**Patterns to follow.** Existing `boolArg` usage at `runtime.go:294`–`:295` for `requiresShimHTTPClient` and `RequireProxyTrust` stays in place; only the third call (mediation) is removed.

**Test scenarios.**

- Covers R3, R5. `go test ./internal/sandbox/container/...` passes.
- `grep -rn "RequireShellMediation\|aileron-shell-mediator" internal/sandbox/container/` returns zero matches.
- The validation script's positional args reduce from 4 to 3 cleanly; any test that exercises validation against the sandbox-base image still passes against the post-U5 image (no `aileron-shell-mediator --check` is needed).

**Verification.** `go test ./internal/sandbox/container/...` is clean; the deleted files no longer exist; the remaining test files reference no mediation symbol.

---

### U5. Strip the image-layer shell wrapper, mediator, and bashrc

**Goal.** Delete the three shell-mediation scripts under `images/sandbox-base/` and remove the `COPY` and `chmod` lines that baked them into the image. Confirm no `apk add` packages were added solely for shell mediation.

**Requirements.** R1, R4.

**Dependencies.** U4 (the launcher and validate path no longer reference the image's shell-mediation contract).

**Files.**

- `images/sandbox-base/shell/aileron-bashrc` (delete)
- `images/sandbox-base/bin/aileron-shell-mediator` (delete)
- `images/sandbox-base/bin/aileron-shell-wrapper` (delete)
- `images/sandbox-base/Containerfile` (modify)

**Approach.**

- Delete the three script files.
- In `Containerfile`, remove the `COPY` lines for `shell/aileron-bashrc`, `bin/aileron-shell-mediator`, and `bin/aileron-shell-wrapper`. Remove the lines that bake the wrapper as `/usr/local/bin/bash` and `/usr/local/bin/sh` (and any `RUN` block doing the symlink / rename). Trim the `chmod` line accordingly.
- In the `adduser` RUN block (around `Containerfile:20`–`:22`), trim `/etc/aileron/shell` from the `mkdir -p` argument list — the directory has no remaining user once `aileron-bashrc` is gone, and leaving the empty dir in the published image keeps the mediation contract discoverable.
- Review the `apk add` line. Per the issue body, none of the listed packages were added solely for shell mediation (`bash` and `wget` predate #801), so no `apk add` package removal is expected. If review reveals otherwise, surface the package(s) before stripping.
- Confirm `images/sandbox-base/bin/` still contains the proxy-bootstrap scripts (`aileron-install-proxy-ca`, `aileron-run-with-proxy-ca`) — those are unrelated and stay.

**Patterns to follow.** Existing slice-1-era Containerfile layout before #801 introduced the wrapper baking. Reference: `git log -- images/sandbox-base/Containerfile` to identify the pre-slice-1 shape.

**Test scenarios.**

- Covers R1, R4. `docker build -f images/sandbox-base/Containerfile -t aileron-sandbox-base:smoke images/sandbox-base` succeeds.
- `docker run --rm aileron-sandbox-base:smoke bash -c 'echo hi'` exits 0 and prints `hi`.
- Inside the image, `docker run --rm aileron-sandbox-base:smoke which bash` resolves to `/bin/bash` (Alpine default), not `/usr/local/bin/bash`.
- The image no longer contains `aileron-shell-mediator`, `aileron-shell-wrapper`, or `aileron-bashrc` (smoke check: `docker run --rm aileron-sandbox-base:smoke sh -c 'ls /usr/local/bin/aileron-shell-* /etc/aileron/shell/aileron-bashrc 2>/dev/null'` returns nothing).
- The `/etc/aileron/shell/` directory is absent from the image (smoke check: `docker run --rm aileron-sandbox-base:smoke sh -c '[ ! -d /etc/aileron/shell ] && echo absent'` prints `absent`).

**Verification.** Local Docker smoke build is clean; image is functionally identical to the pre-#801 sandbox-base.

---

### U6. Trim CI workflows

**Goal.** Remove the `Shellcheck (sandbox shell scripts)` step from `ci.yml`. Revert the `sandbox-base.yml` smoke step to its pre-#801 / slice-5-era shape: drop the deny stub (port 8098), the deny smoke cases, and the `script -qfc` pty case. The allow + closed-port + mediation-off cases either stay (if they still make sense against the post-rip-out image) or are dropped entirely with the rest of the mediation smoke matrix.

**Requirements.** R1, R7.

**Dependencies.** U5 (the image no longer has the wrapper, so any remaining smoke step must not assume it does).

**Files.**

- `.github/workflows/ci.yml` (modify — remove the `Shellcheck (sandbox shell scripts)` step around `:74`–`:76` and the supporting comment block around `:66`–`:73`)
- `.github/workflows/sandbox-base.yml` (modify — revert smoke step to pre-#801 minimal version)

**Approach.**

- Strip the `Shellcheck (sandbox shell scripts)` step from `ci.yml`. Leave any other `shellcheck` usage (e.g., for connector or proxy scripts) untouched.
- For `sandbox-base.yml`, the cleanest path is to diff against the pre-#801 smoke step using `git log -- .github/workflows/sandbox-base.yml` and replace the current block with the pre-#801 minimal version. Specifically: delete the `stub_deny.py` block, the deny-stub backgrounding, the deny smoke cases (allow + deny + closed-port + pty matrix), and the `script -qfc` pty case. Keep the image build step.
- If the pre-#801 smoke step had no shell-mediation matrix at all, the post-rip-out step is effectively "build image + run `bash -c 'echo hi'` smoke" — that satisfies R4.
- Confirm via local `act` or by inspecting the workflow file that no remaining step references `aileron-shell-mediator`, `AILERON_SANDBOX_SHELL_MEDIATION`, or port 8098.

**Patterns to follow.** Slice-5-era smoke step shape; pre-#801 image-build-only shape.

**Test scenarios.**

- Covers R1, R7. `grep -i "shell.mediator\|aileron-bashrc\|sandbox.shell\|SANDBOX_SHELL_DENY" .github/workflows/` returns zero matches.
- The `sandbox-base` workflow's smoke step no longer references port 8098, the deny stub, or `script -qfc`.
- The `ci.yml` workflow no longer has a `Shellcheck (sandbox shell scripts)` step.
- A CI run against the PR branch passes both workflows.

**Verification.** Pushed branch passes `sandbox-base.yml` and `ci.yml` against the post-rip-out tree.

---

### U7. Withdraw ADR-0021 and trim development docs

**Goal.** Flip ADR-0021's HTML Status row to `Withdrawn` with a rationale pointer at the top of `Context`; trim `sandbox-composition.md`'s active shell-mediation prose; remove the shell-mediation image contract from `sandbox-agent-images.md`; rewrite the ADR-0021 / shell-mediation forward-pointers in five sibling ADRs and two development docs; update the ADR landing page; flip the slice-6 plan's frontmatter `status` from `completed` to `withdrawn`.

**Requirements.** R1, R6.

**Dependencies.** None against U1–U6 (docs-only).

**Files.**

- `docs/src/content/docs/adr/0021-v4-shell-layer-mediation.md` (modify — status row + Context rationale paragraph)
- `docs/src/content/docs/adr/index.md` (modify — change the ADR-0021 bullet's status label from `*Proposed.*` to `*Withdrawn.*`)
- `docs/src/content/docs/adr/0015-launch-audit-scope.md` (modify — rewrite the forward-pointer at `:18` to reference ADR-0021 as Withdrawn)
- `docs/src/content/docs/adr/0017-sandbox-composition.md` (modify — rewrite the forward-pointers at `:91` and `:113`)
- `docs/src/content/docs/adr/0018-v4-single-binary-runtime.md` (modify — rewrite the two #801 / shell-mediation references at `:19` and `:36`)
- `docs/src/content/docs/adr/0019-v4-https-data-plane.md` (modify — rewrite the `:66` "#801 can use the same policy/audit pipeline, but shell mediation is not a prerequisite" sentence to reflect the descope)
- `docs/src/content/docs/adr/0022-v4-tiered-network-policy.md` (modify — rewrite the `:28` "becomes load-bearing when … #801 adds shell mediation" sentence)
- `docs/src/content/docs/development/sandbox-composition.md` (modify — line 9 enumeration, line 188 BYO-image paragraph, plus the "What This Does Not Do Yet" section)
- `docs/src/content/docs/development/sandbox-agent-images.md` (modify — lines 88 and 90 shell-mediation references)
- `docs/src/content/docs/development/adding-an-agent.md` (modify — `:21` and `:229` forward-pointers)
- `docs/src/content/docs/development/binary-architecture.md` (modify — `:54` cross-reference)
- `docs/plans/2026-06-05-001-feat-sandbox-shell-deny-plan.md` (modify — frontmatter only)

**Approach.**

- **ADR-0021.** Status lives in an HTML `<tr><th>Status</th><td>Proposed</td></tr>` row inside a `<div class="meta">` block (around `:9`), not in YAML frontmatter. Change `<td>Proposed</td>` to `<td>Withdrawn</td>`. Add a one-paragraph note at the top of the existing `## Context` section pointing at #952 (rip-out issue), #747 (Milestone v4), and the rationale: container isolation + git + HTTPS proxy (#896) + tool-level HITL cover the named risks; no named buyer for shell-layer mediation. Do NOT delete the ADR body — it preserves link history.
- **ADR landing page (`adr/index.md:31`).** Change the ADR-0021 bullet's status label from `*Proposed.*` to `*Withdrawn.*` so the landing page matches the ADR.
- **Five sibling ADRs.** Each carries a forward-pointer phrased as if shell mediation is live or imminent (e.g., "Container-only shell mediation is tracked in #801 and ADR-0021"). Replace each with a Withdrawn-aware rewrite: "Container-only shell mediation was prototyped under #801 and withdrawn in #952; see [ADR-0021](/adr/0021-v4-shell-layer-mediation/) (Withdrawn) for history." Adapt phrasing per host ADR but keep the substance consistent.
- **`sandbox-composition.md`.** Three trim sites: (a) line 9, drop "and shell mediation" from the follow-on runtime layers enumeration; (b) line 188, delete the BYO-image paragraph that names `aileron-shell-mediator`, `aileron-bashrc`, and `AILERON_SANDBOX_SHELL_MEDIATION`; (c) the "What This Does Not Do Yet" section (around `:196`), trim back to its pre-#801 state. Reference: `git log -L /What This Does Not Do Yet/,/##/:docs/src/content/docs/development/sandbox-composition.md`.
- **`sandbox-agent-images.md`.** At line 88, drop "shell mediation or " so the sentence reads "It does not add live discovery refresh." At line 90, delete the entire paragraph describing the shell-mediation image contract.
- **`adding-an-agent.md` (lines 21 and 229) and `binary-architecture.md` (line 54).** Same Withdrawn-aware rewrite pattern as the ADRs.
- **Slice-6 plan.** Change frontmatter `status: completed` to `status: withdrawn`. Body left intact.

**Patterns to follow.** Existing ADR template's HTML status table shape (consistent across ADR-0019, ADR-0020, ADR-0022).

**Test scenarios.**

- Covers R6. `grep -n "<th>Status</th><td>Withdrawn</td>" docs/src/content/docs/adr/0021-*.md` returns one match.
- `grep -n "ADR-0021" docs/src/content/docs/adr/index.md` shows the `*Withdrawn.*` label, not `*Proposed.*`.
- `grep -rn "shell mediation\|shell-mediation\|ADR-0021" docs/src/content/docs/development/ docs/src/content/docs/adr/ | grep -v "(Withdrawn)" | grep -v "0021-v4-shell-layer-mediation.md"` returns zero hits with active forward-pointer phrasing (manual sweep — the heuristic is "no doc says shell mediation is planned/tracked/imminent").
- `grep -n "^status:" docs/plans/2026-06-05-001-feat-sandbox-shell-deny-plan.md` returns `status: withdrawn`.
- `task build:docs` succeeds without broken-link warnings against ADR-0021.

**Verification.** Local docs build passes; the rendered ADR-0021 visibly shows `Withdrawn` status with the rationale note; no other doc claims shell mediation is active or planned.

---

### U8. Verify zero-trace and clean CodeRabbit pass

**Goal.** Final gate before opening the PR: confirm `git grep` returns zero shell-mediation hits outside historical records; `task vet:go` and the full test suite pass; `coderabbit review --agent --base main` returns zero findings; local `task generate:api` is idempotent.

**Requirements.** R1, R2, R3, R7.

**Dependencies.** U1–U7.

**Files.** None modified by this unit — verification only.

**Approach.**

- Run `git grep -i "shell.mediator\|aileron-bashrc\|sandbox.shell\|SANDBOX_SHELL_DENY"`. Expected: hits only in `docs/plans/` (the slice-6 plan, withdrawn) and `docs/src/content/docs/adr/0021-v4-shell-layer-mediation.md` (the withdrawn ADR). Any other hit is a surface gap and routes back to U2–U7.
- Run `task generate:api` once more; expected zero diff.
- Run `task vet:go`.
- Run `go test ./...` across `internal/`, `cmd/aileron/`, `cmd/aileron-mcp/`, `cmd/aileron-enclave/`, `sdk/go/`.
- Run `coderabbit review --agent --base main`. Expected zero findings. If CodeRabbit surfaces advisory findings unrelated to the rip-out, evaluate each on merit — file the ones that are real follow-ups as their own issues, not as PR blockers.
- Per CLAUDE.md, run the local `/code-review` skill before opening the PR.

**Test scenarios.** This unit has no behavior of its own — its outcome is the union of R1–R3 and R7 holding simultaneously.

**Test expectation: none -- this is a verification gate, not a behavior change.**

**Verification.** All four commands exit clean; `git grep` returns only the two expected historical hits.

---

## Risks & Dependencies

- **Risk: `task generate:api` produces unexpected drift beyond the `IntentStatusX` self-revert.** Mitigation: U1 explicitly diffs and surfaces any unexpected change before continuing. If the generated code has cross-package callers that the rename revert breaks, those become an in-PR fixup commit; if the surface is unexpectedly large, this is a route back to the user for guidance.
- **Risk: leftover `regexp` import in `internal/app/handlers.go` if other code in the file uses it.** Mitigation: confirm by `grep` before stripping; `task vet:go` and `goimports` catch a missed cleanup.
- **Risk: scope expansion via KTD4 (runtime.go, shellmediator_test.go) lands the rip-out closer to "fully zero" than the issue body literally enumerates.** Mitigation: confirmed with the issue author at planning time; the PR body cites this expansion explicitly so review is unsurprised.
- **Risk: CI's `sandbox-base.yml` smoke step had functional value beyond mediation (image build, allow stub, mediation-off cases).** Mitigation: U6 keeps the image build and a minimal `bash -c 'echo hi'` smoke; the allow/mediation-off cases drop only because the surface they exercised no longer exists.
- **Dependency: nothing else in the repo depends on `EventTypeSandboxShellDecided` for audit-log filtering or downstream tooling.** Verified by `grep -rn "EventTypeSandboxShellDecided" --include="*.go"` returning only the two files U2 deletes plus the constant definition.
- **Dependency: nothing else depends on `/v1/sandbox-shell/decide` from outside the sandbox image.** The endpoint is internal-only (sandbox-side helper calling the host daemon over the shared host network); no external consumer exists.

---

## Acceptance Examples

Cited from #952's acceptance criteria; AE-IDs are plan-local. Each AE is enforced by at least one test scenario in U1–U7 and the verification gate in U8.

- **AE1** (covers R1). `git grep -i "shell.mediator\|aileron-bashrc\|sandbox.shell\|SANDBOX_SHELL_DENY"` returns hits only in `docs/plans/` and the withdrawn ADR-0021. Enforced by U8 verification.
- **AE2** (covers R2). After U1's spec edit, `task generate:api` regenerates and a second run produces zero diff. Enforced by U1 test scenario.
- **AE3** (covers R3). `task vet:go` and `go test ./...` pass across all named modules. Enforced by U2, U3, U4 test scenarios and U8 verification.
- **AE4** (covers R4). `docker build -f images/sandbox-base/Containerfile -t aileron-sandbox-base:smoke images/sandbox-base` succeeds and `docker run --rm aileron-sandbox-base:smoke bash -c 'echo hi'` exits 0; `which bash` resolves to `/bin/bash`. Enforced by U5 test scenarios.
- **AE5** (covers R5). `aileron launch --sandbox=docker <agent>` launches an agent with no `BASH_ENV`, no `SHELL=/usr/local/bin/bash`, no `AILERON_SANDBOX_SHELL_MEDIATION`, and no daemon endpoint at `/v1/sandbox-shell/decide`. Enforced by U2 (endpoint removal), U3 (env injection removal), and a final manual smoke at U8.
- **AE6** (covers R6). ADR-0021's `Status:` is `Withdrawn` with a rationale paragraph; `sandbox-composition.md` and `sandbox-agent-images.md` no longer claim shell mediation. Enforced by U7 test scenarios.
- **AE7** (covers R7). `coderabbit review --agent --base main` returns zero findings. Enforced by U8.

---

## Operational / Rollout Notes

- **Single PR, conventional-commit title** `revert: drop shell-layer mediation (#801)`. Body summarizes the strategic decision (no named buyer; ADR-0021 bypass paths; container + git + HTTPS proxy + tool-level HITL cover the risks) and links #747, #801, #952, and the slice-6 plan.
- **Squash-merge with `--admin --delete-branch`** per the aileron-family CLAUDE.md.
- **On merge:** close #801 and #952. Update #747's body to reflect that the shell-mediation track is fully descoped.
- **No staged rollout, no migration, no deprecation shim.** The shell-mediation endpoint had no external consumers; the wrapper had no host-side dependency beyond the launcher this PR rewrites. Per memory `feedback_no_backwards_compat`, no transition gate is added.

---

## Sources & Research

- **#952 (this issue).** The issue body itself is the implementation spec; this plan structures it into ordered units with test scenarios and surfaces the scope expansion in KTD4.
- **#801.** Original umbrella for the shell-mediation work; full slice history (slices 1–5 merged via #946, #948, #949, #950; slice 6 was the deny semantics work).
- **#747.** Milestone v4 umbrella; body owes an update on merge to reflect the descope.
- **ADR-0021** at `docs/src/content/docs/adr/0021-v4-shell-layer-mediation.md`. Names the four bypass paths in its `Consequences` / limits section; rationale for the rip-out.
- **#896.** HTTPS proxy / session CA work that, together with container isolation and tool-level HITL, covers the risks the deny trap was meant to address.
- **Slice-6 plan** at `docs/plans/2026-06-05-001-feat-sandbox-shell-deny-plan.md`. Reclassified to `status: withdrawn` by U7; body retained as a historical record.
- **Memory: `project_shell_mediation_descoped`.** Records the 2026-06-08 strategic call.
- **Memory: `project_container_mcp_model`.** Captures the related B1 decision (Aileron is one MCP server, not a gateway) — relevant orientation but not load-bearing for this PR.
- **Local surface scan** (planning-time, this worktree). Verified the in-scope file list and surfaced two gaps absorbed in KTD4: `internal/sandbox/container/runtime.go`'s `RequireShellMediation` field and the `${4:-0}` validation-script block, plus `internal/sandbox/container/shellmediator_test.go`.

External research was not run for this plan. The work is a pure rip-out of code the team wrote in the last three weeks; no external guidance shapes the decisions, and local patterns (prior reverts, the existing OpenAPI-first regen workflow, the existing ADR-withdrawal convention) are sufficient.
