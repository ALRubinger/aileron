---
title: "refactor: retire sandbox shim surface, make MCP the canonical container tool exposure"
status: completed
date: 2026-06-15
type: refactor
origin: GitHub issue #959 (parent #747 — Milestone v4)
---

# refactor: Retire the sandbox shim surface, make MCP the canonical container tool exposure

## Summary

The container today exposes agent tools through **two** surfaces: the static
shims-on-`PATH` + `/etc/aileron/tools.txt` discovery surface, and `aileron-mcp`
(MCP). Issue #959 resolves the surface choice left open by ADR-0024: **retire the
shim surface entirely** and leave `aileron-mcp` as the single canonical exposure.
The two reasons shims were once load-bearing — BYOCLI tool-catalog cost economics
and shim-based credential mediation — are both gone (BYOCLI removed; the HTTPS data
plane now injects credentials at the network boundary). All five launch agents are
MCP-capable, so no shell-only fallback is needed.

This is a **removal/refactor**, not a feature. The connector-spec loading path that
the data plane depends on (`SpecConnectorTools` and friends) is **kept**; only the
shim/`tools.txt` *rendering and mounting* is removed.

Confirmed scope decisions (this session):
- **Base image:** remove the `tools.txt` touch and `aileron-tools.sh` profile
  script, and drop the launch-time `wget`/shim preflight — but **keep `wget`
  installed** in the base image (low-risk; other tooling may rely on it).
- **Compact MCP dispatcher** (`list_actions`/`run_action`, O(1)): **deferred** per
  the issue. The O(N) one-tool-per-action catalog is acceptable at small N.

---

## Problem Frame

ADR-0024 wired `aileron-mcp` into the container as an MCP stdio subprocess to reach
parity with host launch, but deliberately left the shim surface in place as a
"complementary non-MCP-native CLI path." That created a deliberate dual surface
(ADR-0024 "Negative — surface duplication"): MCP-capable agents see the same action
operations twice and pay context-window cost for both.

The dual surface is now pure cost with no benefit:
- **No BYOCLI.** The wrap-arbitrary-CLI feature is removed; the container surface is
  curated `action.md` actions, so the catalog is small and intentional. The O(N)
  argument that justified shims no longer holds.
- **No shim-based credential mediation.** The HTTPS data plane (#896 / #978) injects
  credentials at the network boundary. Shims no longer carry security weight.
- **No shell-only agent.** Claude, Pi, Goose, OpenCode, and Codex are all MCP-capable.

The shim path still costs us: surface duplication, a `wget` runtime dependency and
its launch-time preflight, shell-shaped (vs. structured-JSON) audit, and a second
code path to maintain. Removing it collapses the container to one mental model that
already matches host launch.

---

## Scope Boundaries

### In scope
- Stop generating and mounting `/etc/aileron/tools.txt` and `/usr/local/bin/<shim>`
  scripts from launch.
- Remove the shim/`tools.txt` *renderers* from `internal/sandbox/discovery`
  (action-based and spec-based), keeping the spec→tool derivation the data plane uses.
- Drop the launch-time `wget`/shim-flag preflight and the `requiresShimHTTPClient`
  volume probe.
- Base-image cleanup: remove the `tools.txt` touch and `aileron-tools.sh` profile
  script (keep `wget`).
- Update ADR-0024 and `sandbox-composition.md` to record MCP as the sole surface.
- Update/remove all tests that assert on the shim/`tools.txt` surface.

### Out of scope (true non-goals)
- Changing how `aileron-mcp` exposes actions (it stays one-tool-per-action). The
  surface that remains is unchanged by this work.
- Any change to the HTTPS data plane or credential injection.
- Re-introducing arbitrary-CLI wrapping (the documented reversal condition).

### Deferred to Follow-Up Work
- **Compact MCP dispatcher** (`list_actions` / `search_actions` + `run_action`,
  O(1)) for catalog-cost control — build only if/when catalog cost bites at scale.
  Tracked as the deferred bullet on #959. **Trip-wire:** the deferral rests on
  curated `action.md` catalogs staying small (assume an effective ceiling on the order
  of ~20 installed action operations). If an installed catalog exceeds that, promote
  the dispatcher from deferred to required — retiring shims removes the only other
  O(1) discovery surface, so there is no fallback once this lands. State the measured
  ceiling explicitly when the dispatcher is built.
- **Dynamic discovery refresh** (`tools/list_changed` / re-discovery) so a
  newly-installed action surfaces without an MCP restart — tracked under #897. The
  shim surface hot-loaded via filesystem discovery; `aileron-mcp` caches actions once
  at boot (`cmd/aileron-mcp/main.go`), so this regression is a known, separately-tracked
  tradeoff, not part of this PR. Impact is expected to be low: action install is
  normally a pre-launch operation (per ADR-0014), so mid-session install is uncommon,
  and the workaround is a one-line agent/session restart. Confirm that frequency
  assumption before closing #897 as "won't fix."

---

## Key Technical Decisions

- **KTD1 — Retire, don't gate.** The original issue framing was "suppress shims for
  MCP-capable agents." We delete the surface outright. With every launch agent
  MCP-capable and no BYOCLI, a permanent hybrid has no consumer. (see origin: #959)
  Per-agent MCP capability is tested, not merely asserted: all five agents have real
  `ConfigureMCP` implementations and `internal/launch/agents/sandbox_mcp_matrix_test.go`
  asserts sandbox-mode MCP registration for each, with an end-to-end `tools/list`
  round-trip in `internal/app/sandbox_mcp_test.go`.

- **KTD2 — Keep `SpecConnectorTools` and the spec data-plane path; delete only the
  rendering/mounting.** `discovery.SpecConnectorTools`, `SpecConnectorTool`,
  `SpecOperationHelp`, `specOperationHelp`, `trimNonEmptyStrings`, `sanitizeToolName`,
  and the `InputHelp` type are consumed by `internal/app/handlers_connector_operations.go`
  and `internal/app/handlers_sandbox_forward_proxy.go` for HTTPS operation validation.
  These stay. Only `SpecToolsText`, `SpecShimScripts`, `specShimScript`,
  `jsonStringLiteralContent`, `specOperationEndpoint` (spec side) and `ToolsText`,
  `ShimScripts`, `shimScript`, `ConnectorTools`, `ConnectorTool`, `ActionHelp`,
  `actionHelp`, `toolName`, `uniqueToolName`, `sortedKeys`, `shellQuote`,
  `shellSingleQuote` (action side) are removed. `sanitizeToolName` is shared — it must
  survive. (`specOperationEndpoint` is referenced only by `specShimScript`, which is
  retired, so it is dead after U2; the data-plane `/connector-operations/run` route is
  registered by the API router, not via this constant.)

- **KTD2a — Session attribution is unchanged.** The retired shims set
  `X-Aileron-Session-Id` from `AILERON_SESSION_ID` on every daemon call for approval
  attribution. `aileron-mcp` already sets the same header from the same env var
  (`cmd/aileron-mcp/main.go`), so daemon-side approval gating and session-attributed
  audit have parity after removal. U1 removes only `AILERON_TOOLS_FILE` and
  `AILERON_SHIMS_DIR`; it must leave `AILERON_SESSION_ID` in the agent env.

- **KTD3 — Keep `wget` in the base image.** The `wget` install in
  `images/sandbox-base/Containerfile` is dropped only as a *preflight* requirement,
  not removed from the image. Removing the binary has a wider blast radius (rebuilds
  and republishes the base image; could break unrelated tooling) for no functional
  gain in this change. (confirmed this session) Security note: `wget` is a
  general-purpose HTTP client that, once unused by Aileron's own surface, remains a
  potential egress tool for in-container code. The enforcement layer is the sandbox's
  network egress policy (the tiered network policy / proxy boundary), not tool
  availability, so retention is acceptable under the current threat model. If the
  sandbox ever runs without an egress restriction, removing `wget` becomes worth a
  follow-up.

- **KTD4 — Renumber the validation-script positional args after removing the shim
  slot.** `internal/sandbox/container/runtime.go`'s `validationScript` uses positional
  params: `$1`=agent command, `$2`=shim/`wget` flag, `$3`=proxy bootstrap, `$4`=MCP.
  Removing the `$2` block requires renumbering proxy→`$2` and MCP→`$3` (and updating
  the arg assembly that passes `boolArg(requiresShimHTTPClient(...))`). The
  alternative — pass a constant `"0"` in the shim slot — leaves dead positional
  plumbing; renumbering is cleaner and is the chosen approach.

- **KTD5 — `requiresShimHTTPClient` must go, not just its preflight.** That helper
  returns true for *any* mount under `/usr/local/bin/`, which now matches only the
  retained `aileron-mcp` binary mount. Leaving it would keep firing the (removed)
  preflight intent against the MCP mount. Delete the helper and its call site. Note
  this is a correctness fix, not just cleanup: because the helper already matches the
  `aileron-mcp` mount, the `wget` preflight fires on *every* MCP sandbox launch today,
  so an image with MCP but without `wget` fails validation incorrectly. Removing the
  helper eliminates that incorrect trigger — an intentional, observable behavioral
  change.

---

## High-Level Technical Design

Container tool-exposure surface, before and after. Authoritative for the surface
shape; per-unit file lists below are authoritative for what changes.

```mermaid
flowchart TB
  subgraph Before["BEFORE — dual surface (ADR-0024 as shipped)"]
    direction TB
    L1[launch] -->|generates + mounts| SH["/usr/local/bin/&lt;shim&gt; + /etc/aileron/tools.txt"]
    L1 -->|mounts| MCP1["aileron-mcp (stdio MCP)"]
    SH -->|wget POST argv| DP1[daemon HTTPS data plane]
    MCP1 -->|structured tool call| DP1
    agentB[agent] --> SH
    agentB --> MCP1
  end

  subgraph After["AFTER — MCP is canonical (#959)"]
    direction TB
    L2[launch] -->|mounts| MCP2["aileron-mcp (stdio MCP)"]
    MCP2 -->|structured tool call| DP2[daemon HTTPS data plane]
    agentA[agent] --> MCP2
    note["spec loading retained:<br/>SpecConnectorTools feeds<br/>data-plane operation validation"] -.-> DP2
  end

  Before -.retire shim/tools.txt rendering + mounting + wget preflight.-> After
```

Key shape: the agent→MCP→data-plane path is unchanged. We remove the parallel
agent→shim→`wget`→data-plane path and the `tools.txt` discovery file. The
spec→`SpecConnectorTools`→data-plane validation edge (dotted) stays intact.

---

## Implementation Units

### U1. Stop emitting `tools.txt` and shim mounts from launch

**Goal:** Launch no longer generates the `tools.txt` manifest or `/usr/local/bin/<shim>`
scripts, no longer mounts them, and no longer sets the shim/tools env vars. The
`aileron-mcp` mount and all proxy wiring are untouched.

**Requirements:** Issue #959 work items 1 and 2 (stop emitting; remove mounting/
launch-time shim paths), preserving connector-spec loading.

**Dependencies:** none.

**Files:**
- `internal/launch/launcher.go` (modify) — remove `sandboxDiscoveryMounts` and
  `sandboxDiscoveryArtifacts` (and `isReservedSandboxCommand`); remove the call site
  in the runtime-mounts assembly (~line 960, 979); remove constants
  `sandboxToolsFilePath`, `sandboxShimsDirPath` (~lines 43–44); remove the
  `AILERON_TOOLS_FILE` and `AILERON_SHIMS_DIR` env assignments (~lines 404–405),
  **leaving `AILERON_SESSION_ID` in place** (still consumed by `aileron-mcp` for
  session attribution — see KTD2a); fix the ADR-0024 comment near line 721 that
  references "tools.txt and shims." Once `sandboxDiscoveryMounts`/
  `sandboxDiscoveryArtifacts`/`isReservedSandboxCommand` are gone, the `reservedNames`
  variadic threaded into `sandboxRuntimeMounts` and the discovery tempdir `cleanup`
  func return are unconsumed — drop both and update the two call sites (~lines 254,
  711) rather than leaving dead plumbing (consistent with KTD4). Confirm the retained
  `/opt/aileron/manifests` mounts are unaffected.
- `internal/launch/launcher_internal_test.go` (modify) — remove
  `sandboxDiscoveryArtifacts` tests and the runtime-mounts tests asserting the
  `tools.txt` mount and reserved-shim handling.
- `internal/launch/launcher_test.go` (modify) — remove docker-arg assertions for
  `AILERON_TOOLS_FILE`, `AILERON_SHIMS_DIR`, and `tools.txt`/shim mounts; keep the
  `aileron-mcp` mount assertions.

**Approach:** This is the behavioral core. After removal, the launch flow no longer
appends any shim or `tools.txt` mount; the container sees only the workspace, proxy,
manifests, and `aileron-mcp` mounts assembled by the existing callers. The MCP binary
mount at `/usr/local/bin/aileron-mcp` and the `sandboxMCPBinName` reservation stay.
Verify nothing else references the removed constants/env vars before deleting (grep
first).

**Patterns to follow:** Mirror the existing mount-assembly style in
`sandboxRuntimeMounts`; keep the proxy/MCP mount ordering intact.

**Test scenarios:**
- Launch with installed actions/specs present produces container args that include
  the `aileron-mcp` mount and do **not** include any `/usr/local/bin/<shim>` mount,
  any `/etc/aileron/tools.txt` mount, or the `AILERON_TOOLS_FILE` / `AILERON_SHIMS_DIR`
  env vars. (Covers issue work items 1–2.)
- Launch with no installed actions still wires `aileron-mcp` and succeeds (no
  regression from the old "skip when no tools/shims" early return).
- Proxy bootstrap env/mounts are unchanged by this removal (regression guard on the
  retained path).

**Verification:** `task test` for the launch package is green; container-arg golden
assertions show MCP-only tool exposure.

---

### U2. Remove shim/`tools.txt` renderers from the discovery package

**Goal:** Delete the action-based and spec-based shim/`tools.txt` rendering code while
keeping the spec→tool derivation the data plane uses.

**Requirements:** Issue #959 work item 2 ("keep connector-spec loading (#895) — the
data plane still needs it").

**Dependencies:** U1 (the only callers of these renderers are the launch artifact
functions removed in U1; remove callers first so the renderers are unreferenced).

**Files:**
- `internal/sandbox/discovery/tools.go` (modify) — remove `ToolsText`, `ShimScripts`,
  `shimScript`, `ConnectorTools`, `ConnectorTool`, `ActionHelp`, `actionHelp`,
  `toolName`, `uniqueToolName`, `sortedKeys`, `shellQuote`, `shellSingleQuote`. **Keep
  `sanitizeToolName`** (shared with the spec path) and the `InputHelp` type (used by
  `SpecOperationHelp`).
- `internal/sandbox/discovery/spec.go` (modify) — remove `SpecToolsText`,
  `SpecShimScripts`, `specShimScript`, `jsonStringLiteralContent`, and the
  `specOperationEndpoint` constant (its only reference was `specShimScript`, now
  removed — see KTD2). **Keep** `SpecConnectorTools`, `SpecConnectorTool`,
  `SpecOperationHelp`, `specOperationHelp`, and `trimNonEmptyStrings`.
- `internal/sandbox/discovery/tools_test.go` (modify/remove) — delete tests covering
  the removed renderers (including `TestToolNameSanitizesFallbackFQN`, which exercises
  `sanitizeToolName` only transitively through the retired `toolName`). Because no
  standalone `sanitizeToolName` test exists today, **ADD** a direct test for it so the
  retained shared helper keeps coverage (see test scenarios).
- `internal/sandbox/discovery/spec_test.go` (modify) — delete shim/`tools.txt` render
  tests; **keep** the `SpecConnectorTools` conflict/error test. Because the
  `SpecConnectorTools` happy path was only covered transitively by the deleted
  `SpecToolsText`/`SpecShimScripts` tests, **ADD** a direct happy-path
  `SpecConnectorTools` test so the retained data-plane contract keeps coverage.

**Approach:** After deletion the package's remaining purpose is deriving
`SpecConnectorTool`s from connector specs for data-plane validation, plus the shared
name sanitizer. Confirm with `go build ./...` that `internal/app` handlers still
compile against the retained symbols.

**Patterns to follow:** Keep the package doc comment accurate — it currently says the
package "renders the sandbox-side discovery surfaces agents read"; update it to
reflect that it now derives connector tools for data-plane validation.

**Test scenarios:**
- (ADD) `SpecConnectorTools` happy path: returns the expected tools/operations for a
  sample multi-spec set, sorted, with operation help populated. This is a new direct
  test — the happy path is currently covered only transitively by deleted tests.
- `SpecConnectorTools` name-conflict error path still returns the conflict error
  (retained from the existing conflict test).
- (ADD) `sanitizeToolName` returns the expected sanitized name for representative
  inputs (mixed case, spaces, dashes, leading/trailing separators). New direct test —
  no standalone coverage exists today.
- Package compiles with no references to the removed renderers (covered by build).

**Verification:** `go build ./...` clean; `internal/sandbox/discovery` and
`internal/app` package tests green.

---

### U3. Drop the `wget`/shim-flag launch-time preflight

**Goal:** Remove the `$2` shim/`wget` preflight block from the sandbox image
validation script and the `requiresShimHTTPClient` volume probe that triggered it;
renumber the remaining positional args.

**Requirements:** Issue #959 work item 3 ("Drop the `wget` / shim-flag preflight that
only existed for mounted shims").

**Dependencies:** U1 (shim mounts gone, so the probe and preflight have no remaining
purpose).

**Files:**
- `internal/sandbox/container/runtime.go` (modify) — delete the two `${2:-0} = "1"`
  blocks in `validationScript` (the `wget` presence check and the `wget --help` flag
  check, ~lines 473–489); delete `requiresShimHTTPClient` (~lines 523–530) and the
  `boolArg(requiresShimHTTPClient(opts.Volumes))` argument at the validation call
  site (~line 433); renumber proxy-bootstrap `$3`→`$2` and MCP `$4`→`$3`, adjusting
  the arg assembly to match (per KTD4).
- `internal/sandbox/container/runtime_test.go` (modify) — remove the wget/shim tests
  (`TestValidateRequiresWgetWhenShimsAreMounted`,
  `TestValidateReportsMissingWgetForShimImages`, and the `--post-data` flag-probe
  test). Update `TestValidate_RequireMCPBinary_AppendsFourthPositional`: it hard-codes
  the literal `${4:-0}` assertion and reads the script via
  `runner.args[len(runner.args)-6]`; after renumbering MCP `$4`→`$3` the assertion
  becomes `${3:-0}`, the index becomes `-5` (the arg vector shrinks by one), and the
  test should be renamed to reflect the third positional. Keep the proxy and MCP
  preflight tests; adjust their positional assertions to the new order.

**Approach:** Careful with positional renumbering — proxy and MCP preflights are still
load-bearing and must keep firing. After the edit, `$1`=agent command, `$2`=proxy
bootstrap, `$3`=MCP. The MCP binary mount at `/usr/local/bin/aileron-mcp` no longer
needs the shim probe; its own `$3`-gated `aileron-mcp --version` check remains the
correctness guard.

**Execution note:** Sequence this unit explicitly to avoid a silent-disable.
(1) Write/confirm regression tests against the *current* 4-arg script that assert the
proxy and MCP preflights fire (image missing the proxy CA helper fails; image missing
`aileron-mcp` fails) — they should pass as-is. (2) Change the `validationScript`
string and the Go call site as one atomic edit. (3) Re-run the tests. Off-by-one in
the positional args silently disables a real security gate, so the test-first ordering
is load-bearing here, not optional.

**Patterns to follow:** The existing `boolArg(...)` arg-assembly pattern at the
validation call site.

**Test scenarios:**
- Image missing `aileron-mcp` still fails validation with the MCP error (regression
  guard that the MCP preflight survived renumbering).
- Image missing proxy-bootstrap helpers still fails when proxy bootstrap is active
  (regression guard for the proxy preflight).
- A valid image with only the `aileron-mcp` mount (no shim mounts) passes validation
  without any `wget`-related failure — i.e., the shim preflight is gone.
- The validation arg vector built for a launch contains no shim/`wget` flag slot.

**Verification:** `internal/sandbox/container` tests green; the three preflight paths
(agent command, proxy, MCP) behave as before for non-shim concerns.

---

### U4. Base-image cleanup (keep `wget`)

**Goal:** Remove the `tools.txt` artifact and the tools-manifest profile script from
the sandbox base image. Leave `wget` installed (KTD3).

**Requirements:** Issue #959 work item 1 (stop emitting `tools.txt`), base-image side;
confirmed scope to keep `wget`.

**Dependencies:** U1 (nothing mounts onto `/etc/aileron/tools.txt` anymore).

**Files:**
- `images/sandbox-base/Containerfile` (modify) — remove the
  `&& touch /etc/aileron/tools.txt \` line and the
  `COPY profile.d/aileron-tools.sh /etc/profile.d/aileron-tools.sh` line. **Do not**
  remove the `wget` install.
- `images/sandbox-base/profile.d/aileron-tools.sh` (delete) — only echoed the tools
  manifest on shell init; no remaining consumer.

**Approach:** Keep `/etc/aileron/` directory creation if other artifacts (e.g. proxy
CA) live there; only the `tools.txt` touch is removed. Confirm the proxy CA path
(`/etc/aileron/proxy/ca.pem`) is created independently and is unaffected.

**Test scenarios:** This is a base-image deletion with observable failure modes
(broken login-shell init, or an accidentally-removed `/etc/aileron` proxy CA path), so
it is not coverage-free. Add or extend a container smoke assertion (locate the
existing sandbox-base/launch smoke test first; only add a new one if none asserts
container startup) that, against the rebuilt base image:
- confirms `/etc/aileron/tools.txt` is **absent**;
- confirms a launched container's login shell starts with **no error or stale banner**
  from the removed `aileron-tools.sh` profile script;
- confirms `/etc/aileron/proxy/ca.pem` is still present/creatable on a proxy-bootstrap
  launch (the proxy CA path must survive the `tools.txt` removal).

**Verification:** Sandbox-base image builds; the smoke assertion above passes; a
launched container starts cleanly with no `tools.txt` and a working proxy CA path.

---

### U5. Record MCP as the sole surface across ADRs and developer docs

**Goal:** Documentation reflects that MCP is the single canonical container tool
exposure and the shim/`tools.txt` surface is retired.

**Requirements:** Issue #959 work item 4.

**Dependencies:** U1–U4 (document the end state once the code reflects it).

**Approach to scope:** The shim/`tools.txt` surface is described as *live* in more
docs than the original three. Before editing, run
`grep -rln 'shim\|tools\.txt\|SHIMS_DIR\|AILERON_TOOLS_FILE' docs/src/content/docs`
and triage every hit into **MUST-UPDATE** (describes the surface as a current
contract) vs **KEEP-as-historical** (a past-decision record). The confirmed set below
is the floor, not the ceiling.

**Files — MUST-UPDATE (living developer/reference docs):**
- `docs/src/content/docs/adr/0024-sandbox-mcp-parity.md` (modify) — update the
  "Negative — surface duplication" consequence: the duplication is resolved by
  retiring shims, not by capability-gating. Update the Decision-section language that
  describes shims as a "complementary non-MCP-native CLI path" (and the in-ADR **KTD6**
  reference, which lives in ADR-0024's own implementation-plan KTDs, not this plan) to
  state MCP is now the sole surface. Add a short note citing #959 as the resolution of
  the surface choice ADR-0024 left open. ADRs are amended in place pre-MVP (per the ADR
  index convention), so edit rather than supersede. Keep every `ADR-NNNN` mention a
  Markdown link.
- `docs/src/content/docs/development/sandbox-composition.md` (modify) — remove the
  passage describing the generated `tools.txt` manifest, `/usr/local/bin` shim
  scripts, `--help` discovery, and the `wget` shim requirement (~lines 169–174); state
  that `aileron-mcp` is the sole in-container tool surface. Preserve the description of
  the retained connector-spec loading and the data-plane credential injection.
- `docs/src/content/docs/development/sandbox-connector-specs.md` (modify) — the page
  currently frames specs as "how specs become generated HTTPS shims" and shows a
  literal `tools.txt` entry + shim body. Rewrite to frame spec loading as
  data-plane operation validation; remove the generated-shim rendering walkthrough.
- `docs/src/content/docs/development/adding-an-agent.md` (modify) — drop the
  "complementary non-MCP-native CLI surface" claim so it no longer mis-teaches new
  agent authors that shims are a surface they must account for.
- `docs/src/content/docs/development/binary-architecture.md` (modify) — remove
  "generated sandbox shims" / `tools.txt` from the daemon-consumer description; MCP is
  the in-container tool path.
- `docs/src/content/docs/development/sandbox-agent-images.md` (modify) — remove the
  BYO-image requirement that images need `wget` "when Aileron mounts generated
  connector shims"; that contract no longer exists.
- `docs/src/content/docs/adr/0017-sandbox-composition.md` (modify) — update the
  live-surface prose (base-image "discovery files", `AILERON_SHIMS_DIR`/
  `AILERON_TOOLS_FILE` env hints, `wget` validation) to reflect the retired surface,
  per the amend-in-place convention.
- `docs/src/content/docs/adr/index.md` (modify, if needed) — refresh the ADR-0024 /
  ADR-0017 one-line summaries if they mention the dual surface.

**Files — triage decision required (likely KEEP-as-historical):**
- `docs/src/content/docs/adr/0019-v4-https-data-plane.md`,
  `docs/src/content/docs/adr/0020-v4-connector-specs-and-shims.md` — these record past
  v4 decisions. Decide explicitly whether to amend in place (pre-MVP convention) or
  leave as historical record; default to leaving them as historical and adding a
  one-line forward pointer to #959 / ADR-0024 rather than rewriting the decision.

**Approach:** Follow the documentation writing voice rules (no em-dashes, one thought
per sentence, no "not just X, Y"). Keep the reversal condition visible: re-introducing
arbitrary-CLI wrapping would revive the shim case.

**Test scenarios:**
- `Test expectation: none — documentation.` Verify links resolve and the docs site
  builds (`task` docs build if present).

**Verification:** Docs build clean; after the sweep,
`grep -rn 'tools\.txt\|SHIMS_DIR\|AILERON_TOOLS_FILE\|generated.*shim' docs/src/content/docs`
returns only historical-ADR mentions and the reversal-condition note — no doc
describes the shim/`tools.txt` surface as a current contract.

---

## Risks & Dependencies

- **Positional-arg off-by-one (U3).** Renumbering the validation script could silently
  disable the proxy or MCP preflight. Mitigation: regression tests for both preflights
  land in U3 and must pass.
- **Hidden consumer of `tools.txt` / shims.** Code and docs both reference the surface.
  The *code* consumers are enumerated (U1–U3) and gated by `go build ./...`. The *docs*
  surface is broader than first scoped: at least seven pages describe shims/`tools.txt`/
  `wget` as a live contract (see U5), including `adding-an-agent.md` and
  `sandbox-agent-images.md`, which would mis-teach contributors and BYO-image authors
  after merge. Mitigation: U5's grep-driven triage covers docs; a repo-wide grep for
  `tools.txt`, `AILERON_TOOLS_FILE`, `AILERON_SHIMS_DIR`, `/usr/local/bin/<shim>`, and
  `requiresShimHTTPClient` across code *and* docs is the completion gate. Also check the
  three connector repos (bluebubbles/google/slack) for any doc that assumes the shim
  contract.
- **Spec path accidentally over-deleted (U2).** Deleting a symbol the data plane uses
  breaks `internal/app`. Mitigation: KTD2 enumerates the exact KEEP set;
  `go build ./...` after U2 is the gate.
- **Known regression (accepted, deferred):** newly-installed actions require an MCP
  restart to surface (no more filesystem hot-load). Tracked under #897; out of scope.

## Sources & Research

- GitHub issue #959 — decision, work checklist, reversal condition.
- `docs/src/content/docs/adr/0024-sandbox-mcp-parity.md` — "Negative — surface
  duplication," Decision-section shim framing, and ADR-0024's own KTD6 (the
  surface-duplication decision the ADR left open; not a KTD of this plan).
- `docs/src/content/docs/adr/index.md` — ADR editability convention (amend in place
  pre-MVP).
- Code map (this session): `internal/launch/launcher.go`,
  `internal/sandbox/discovery/{tools,spec}.go`,
  `internal/sandbox/container/runtime.go`, `internal/app/handlers_connector_operations.go`,
  `internal/app/handlers_sandbox_forward_proxy.go`, `images/sandbox-base/`.
