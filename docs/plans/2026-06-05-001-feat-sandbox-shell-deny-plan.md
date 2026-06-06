---
title: "feat: deny semantics for sandbox shell mediation (#801 slice 6)"
status: completed
created: 2026-06-05
type: feat
tracking: ["#801", "#747"]
adrs: ["ADR-0021", "ADR-0017", "ADR-0015"]
predecessor_pr: "#949"
---

# feat: deny semantics for sandbox shell mediation (#801 slice 6)

## Summary

Sixth implementation slice of #801. Wire deny semantics through the existing
daemon decision endpoint and make a denied command actually halt the agent's
non-interactive `bash -c` model with a nonzero exit and the existing
`[Aileron] denied: <reason>` stderr message — without regressing the allow path
that the fifth slice (PR #949) just shipped.

**Scope and posture.** The slice ships detection and defense-in-depth against
a *non-adversarial* agent (the everyday case: an LLM that occasionally generates
a destructive command an operator wants to block). It is **not** a hard
boundary against a determined attacker. The shell-layer mediation architecture
as designed is bypassable in known ways by an agent that can manipulate its own
environment (env strip, recursion-guard tampering, interactive-shell spawn) or
that wraps commands behind `eval`/`command`/`exec`/function indirection. Those
limits are named in the new `Threat model and limits` section below; closing
them is a separate architecture track (container-enforced exec via a supervisor,
or seccomp/LSM), not this slice.

The work is gated on a single design experiment: prove which DEBUG-trap halt
mechanism actually halts `bash -c "denied && next"` with a nonzero exit against
real bash inside the Alpine sandbox-base image, since slice 5's tests document
explicitly that returning nonzero from an `extdebug` DEBUG trap suppresses the
side effect but leaves the shell at exit 0. That gate (U1) precedes daemon and
container wiring. KTD1 carries a working hypothesis so reviewers can stress-test
the chosen mechanism today; U1 falsifies the hypothesis if real-bash behavior
disagrees.

Approval-pending policy, the approval queue, result drain, `aileron wait <id>`,
the TUI surface ([#802](https://github.com/ALRubinger/aileron/issues/802)), the
HTTPS proxy / session CA ([#896](https://github.com/ALRubinger/aileron/issues/896)),
and host shell interception ([ADR-0015](/adr/0015-launch-audit-scope/)) remain
out of scope for this slice.

---

## Problem Frame

The fifth slice routed the live agent session's shell through the mediator and
proved (in `internal/sandbox/container/shellrc_test.go:219-229` test comments
and via Docker experiments) that the DEBUG-trap veto under `shopt -s extdebug`
**suppresses a denied command's side effect but leaves the shell exit at 0** in
both interactive and non-interactive bash. Consequences today:

- `bash -c "denied && next"` still runs `next` because the chain does not halt.
- The shell's exit code is 0 even though the command was blocked.

This blocks #801's core acceptance criterion: "a denied command returns nonzero
with a clear `[Aileron] denied: <reason>` message." Returning nonzero from the
trap is not sufficient. The slice must replace the veto-to-skip mechanism with
one that genuinely halts non-interactive `bash -c` and yields a nonzero shell
exit, while preserving the existing allow path, fail-closed behavior, and the
sanitized audit shape.

The daemon side is the smaller half. Today's handler at
`internal/app/handlers_sandbox_shell.go` is hard-coded allow-only. The endpoint
must become capable of returning `decision: deny` with a reason, audit it the
same way as allow (carrying the latency_ms field added in slice 5), and stay
spec-first via `internal/api/openapi.yaml`.

---

## Requirements

| R-ID | Requirement | Verified by |
|---|---|---|
| R1 | The daemon at `POST /v1/sandbox-shell/decide` can return `decision: "deny"` with a populated `reason`. | U2 handler tests |
| R2 | A denied command does **not** run AND the shell observes a **nonzero exit** for the agent's non-interactive `bash -c` model. | U1 real-bash tests; U4 Docker smoke |
| R3 | `bash -c "denied && next"` does not run `next` and exits nonzero. | U1 real-bash tests; U4 Docker smoke |
| R4 | The `[Aileron] denied: <reason>` message is preserved on stderr (slice 5 already prints it from the mediator). | U1 / U4 |
| R5 | The `sandbox.shell.decided` audit event records `decision="deny"`, `reason` in the stable format committed in KTD2, `aileron.shell.matched_pattern` (the matched regex source), and `aileron.shell.latency_ms` — without leaking env, output, or secrets. | U2 handler tests |
| R6 | The allow path is byte-for-byte unchanged versus slice 5 when `AILERON_SANDBOX_SHELL_DENY_PATTERN` is unset. No new daemon call shape, no new env, no new audit fields on allow. | U2 + U3 regression tests; existing slice-5 tests stay green |
| R7 | Fail-closed behavior is preserved end-to-end on unreachable daemon, bad body, missing token, AND on a present-but-unparseable `AILERON_SANDBOX_SHELL_DENY_PATTERN`. The daemon refuses to start when the pattern is set but doesn't compile (KTD2). | U1, U2, U4 — extended fail-closed cases plus a U2 startup-refusal test |
| R8 | Interactive and non-interactive shell behavior on deny are explicitly decided and documented (interactive default: soft veto, REPL alive; non-interactive: halt with nonzero). Interactive contract is exercised under a real pty in CI via `script -qc` (U4), not just unit-tested. | U1 records the decision; U4 covers interactive under a real pty; U5 documents it |
| R9 | OpenAPI contract reflects deny semantics (description + decision enum tightened); generated code regenerated via `task generate:api`. | U2 spec change |
| R10 | ADR-0021 amended in place (pre-MVP) — including the `Threat model and limits` named in this plan — and `docs/src/content/docs/development/sandbox-composition.md` updated to describe this slice and its honest scope. | U5 |
| R11 | Existing invariants from slices 4-5 hold: lazy `extdebug` (enabled inside trap), recursion guard `_AILERON_IN_TRAP` (named as defeatable by an adversarial agent in the Threat model section but preserved for non-adversarial coverage), exactly-one daemon call per top-level command, wrapper ahead of real shells on PATH. | U1 / U3 keep-green tests |
| R12 | Shell scripts stay `shellcheck -s sh` clean. A shellcheck step is wired into the `.github/workflows/ci.yml` lint job as part of this slice. | New CI step added by U3 |

---

## Threat model and limits

This slice mediates shell commands for a **non-adversarial agent**: the LLM
agent is treated as cooperative but fallible — it can generate destructive or
incorrect commands an operator wants to block, but it is not actively trying to
escape the boundary. The architecture as designed is bypassable by a
determined attacker (or a future LLM that is trained to evade mediation) in
known ways. Closing those holes is a separate architecture track. The plan
ships honestly about what this slice does and does not deliver.

### Known limits in scope to *acknowledge*, not close, this slice

**TM-L1 — Env strip disables mediation.** The agent process inherits
`AILERON_SANDBOX_SHELL_MEDIATION=1` and `BASH_ENV=/etc/aileron/shell/aileron-bashrc`
as plain environment variables (`internal/launch/launcher.go:344-353`). An
adversarial agent can `unset AILERON_SANDBOX_SHELL_MEDIATION` or `unset BASH_ENV`
before spawning child shells. The rcfile guard at `images/sandbox-base/shell/aileron-bashrc:7-9`
returns inert if the variable is absent, so subsequent non-interactive bash
children execute without the DEBUG trap. **Mitigation track:** per-exec
injection by a supervisor process that owns the container's exec path, so the
agent never sees the env vars as inheritable state. Out of scope for this
slice.

**TM-L2 — Recursion guard is attacker-controlled.** The trap checks
`[ -n "${_AILERON_IN_TRAP:-}" ]` (`aileron-bashrc:16-18`) and allows-without-daemon-call
if the variable is set. An agent that exports `_AILERON_IN_TRAP=1` once defeats
the guard for every subsequent command in every child shell — no daemon call is
ever made. **Mitigation track:** move the guard off env (a file descriptor,
`/run/aileron/in-trap` lock, or a bash-internal-only state) so the agent cannot
read or set it. Out of scope for this slice.

**TM-L3 — Interactive bash spawn bypasses the halt mechanism.** KTD3 splits
behavior on `[[ $- == *i* ]]` so a real human's REPL stays alive on a deny.
That split is the right call for a human at a terminal, but it means an agent
runtime that ever spawns `bash -ic 'rm -rf /'` (some tools do this to inherit
user aliases or PS1 setup) gets the soft-veto branch — the side effect is
suppressed but the exit code is 0 and the agent continues as if the command
succeeded. **Mitigation track:** the wrapper script detects agent context
(e.g., `AILERON_SESSION_ID` present) and refuses `-i`/`-ic` regardless of the
caller's intent, forcing the non-interactive halt path. Out of scope for this
slice; the wrapper-layer enforcement is itself a small enough change that it
could fold into the policy-engine slice if the maintainer prefers.

**TM-L4 — `$BASH_COMMAND` regex matching has structural blind spots.** The
mediator sends `$BASH_COMMAND`, which is the *surface* command bash is about to
run. For `eval "$x"` the regex sees `eval "$x"`, not the inner command. For
`command rm -rf /` the regex must literally match the prefix `command rm`. For
`exec rm -rf /` likewise. For a shell function `f() { rm -rf /; }; f`, the
outer trap sees `f`; bash *does* fire a nested DEBUG trap for the inner `rm`,
but the recursion-guard suppresses it (R11). For base64-encoded payloads
decoded then eval'd, the regex receives only the decode/eval call. A real
policy DSL (the next #801 slice) inherits these blind spots unless the
architecture changes — they are not "tests we forgot to write"; they are
properties of the regex-against-`$BASH_COMMAND` design. **Mitigation track:**
shallow command unwrapping in the mediator (recognize `eval`, `command`,
`exec`, simple function wrappers) plus a policy DSL that defines its match
surface explicitly. Out of scope for this slice.

### What slice 6 *does* deliver against this threat model

Against a non-adversarial agent that simply generates an unwanted command, the
boundary holds: the trap installs, the regex matches, the daemon responds
deny, the trap halts the shell with nonzero exit, the `[Aileron] denied:`
message reaches the user, and the audit event records the matched pattern.
That is real, durable value for the common case the slice exists to address.
The honest framing is "detection plus reliable veto when the agent
cooperates," not "untrusted code containment."

---

## Scope Boundaries

### In scope (this slice)

- Daemon handler can return `decision: deny` with a reason.
- A single mechanism in the daemon to **trigger** a deny in this slice, so deny is exercisable end-to-end without bringing in a policy DSL (see KTD2).
- Bash trap halts with nonzero exit on deny under non-interactive `bash -c`.
- Interactive behavior decided explicitly (default: soft veto + REPL alive).
- Real-bash tests for the chosen halt mechanism.
- Real-image Docker smoke proof in `.github/workflows/sandbox-base.yml`.
- Audit event records the deny decision and reason; latency_ms preserved.
- ADR-0021 + sandbox-composition.md prose updated.

### Deferred to Follow-Up Work (planned, next #801 slices)

- Approval-pending policy: `decision: "pending_approval"`, approval id payload,
  approval queue surface, and `aileron wait <id>` result draining.
- Real policy engine / policy DSL replacing the env-driven deny trigger in
  KTD2.
- Container-observed round-trip latency telemetry (server-side latency was
  added in slice 5; container-side is still future work).
- Bypass attempts and subshell-inheritance test coverage.

### Outside this product's identity (not planned)

- Approval TUI / terminal surface ([#802](https://github.com/ALRubinger/aileron/issues/802)).
- HTTPS proxy / session CA work ([#896](https://github.com/ALRubinger/aileron/issues/896)).
- Host shell interception ([ADR-0015](/adr/0015-launch-audit-scope/) removed
  it; stays removed).

---

## Key Technical Decisions

### KTD1 — U1 is a real-bash falsification gate against a pre-committed hypothesis

The plan commits to a **working hypothesis** so reviewers can stress-test the
chosen mechanism today, and runs U1 as a falsification experiment that can
overturn the hypothesis if real-bash behavior disagrees:

> **Working hypothesis (to falsify in U1):** the DEBUG trap calls
> `exit <code>` on deny when `[[ $- != *i* ]]`. This produces a chain-halt
> (subsequent `&&` and `;` chained commands do not run) and a nonzero shell
> exit, exactly satisfying R2/R3. Under `[[ $- == *i* ]]` (interactive) the
> trap returns 1 instead, matching slice 5's soft-veto behavior so a real
> human's REPL stays alive.

U1 runs the **three candidates** the slice owner enumerated against the
real-bash test harness already present at
`internal/sandbox/container/shellrc_test.go`, pinned to the **Alpine
`images/sandbox-base` image via `docker run`** (not the dev's local
glibc-bash — see "Pin to Alpine" below). The three candidates:

- **C1** (the working hypothesis above): trap calls `exit <code>` on deny under
  non-interactive bash, `return 1` under interactive.
- **C2**: trap returns 2 under extdebug (bash documents this as "simulate a
  return" — U1 verifies actual `bash -c` behavior on Alpine bash 5.x).
- **C3**: trap unconditionally enables `set -e` and returns 1, relying on
  `set -e` + `&&` short-circuit semantics to halt the chain.

If U1 falsifies C1 on Alpine bash, the `Resolved by U1` section at the bottom
of this plan records the override and U3 picks whichever candidate (C2 or C3)
passed; if all three fail, plan re-opens before U3 ships. The other two
candidates that didn't win are rejected in prose with a one-sentence reason
recorded in `Resolved by U1`.

**Pin to Alpine.** U1's candidate selection runs inside
`docker run --rm images/sandbox-base bash -c …` (or against a locally-built
`aileron-sandbox-base:smoke` tag), so the bash version, BusyBox-vs-glibc
coreutils, and tini PID 1 the agent actually hits are part of the experiment.
Running U1 only on the developer's local Linux bash + glibc would risk a
candidate that passes locally and fails inside the image (`exit` behavior
inside DEBUG traps and `extdebug` semantics around `return 2` were tightened
between bash 4.x and 5.x). The local `requireBashTooling` test harness still
runs — but it's complementary fast-iteration coverage; the canonical gate is
Alpine.

**Stderr cleanliness assertion.** U1 includes a test that, after a denied
command, asserts stderr contains `[Aileron] denied:` AND does **not** contain
the raw `$BASH_COMMAND` text or any of its arguments. This protects against
the `exit`-from-trap candidate (C1) leaking sensitive command args via bash's
own exit diagnostics under `set -eu` or xtrace. If C2 or C3 is chosen instead,
the assertion still applies — no candidate should leak the command text to
stderr.

### KTD2 — Daemon deny trigger is a single env-driven regex this slice; no policy DSL

To exercise the deny wire end-to-end without expanding scope, the handler reads
`AILERON_SANDBOX_SHELL_DENY_PATTERN` from the process environment and, when set,
treats the value as a regex. A match returns `decision: "deny"` with a reason
in the stable format committed below. An unset variable preserves the
allow-only baseline byte-for-byte (R6). This matches the daemon's existing
config convention (inline `os.Getenv("AILERON_*")` at call site, examples in
`internal/app/handlers_status.go:40`, `internal/app/app.go:317`,
`internal/app/app.go:543-546`). A real policy DSL/engine is the next #801 slice;
this env-driven seam is deliberately minimal — slice 6 verifies the wire
shape, not the daemon's ability to host a real policy engine.

**Stable deny-reason format (committed in this slice, asserted by U2 tests):**

> `matched deny pattern: <pattern>`

where `<pattern>` is the verbatim source of the matched regex (the value of
`AILERON_SANDBOX_SHELL_DENY_PATTERN`). The format is locked now because the
reason text flows into the daemon's HTTP response body, the audit payload, and
the user-facing stderr via the mediator — once shipped, downstream consumers
will pattern-match on it. Future slices may extend the format (e.g.,
`matched deny rule "<rule-id>"` once a real DSL exists), but the
`matched deny <kind>:` prefix is intended as the stable observable shape across
the #801 epic.

**Bad-regex behavior (fail-closed, by refusing to start):** if
`AILERON_SANDBOX_SHELL_DENY_PATTERN` is set but does not compile as a regex,
the daemon **exits at startup** with a clear error message naming the env var
and the regex error. The daemon does not start, serve `/v1/sandbox-shell/decide`,
or accept any other request when its deny pattern is misconfigured. This
matches R7's fail-closed posture (transport errors AND policy-config errors
both fail closed) and prevents the silent-fail-open footgun where a typo'd
pattern leaves a sandbox the operator believed was locked down. The case is
covered by a U2 startup-refusal test. When the pattern is intentionally unset,
the daemon starts and the allow path is byte-for-byte unchanged (R6).

### KTD3 — Interactive vs non-interactive split: halt non-interactive, soft-veto interactive

Non-interactive `bash -c` (R2/R3): trap halts the shell with a nonzero exit,
suppressing the about-to-run command. Interactive `--rcfile -ic` (and login):
trap suppresses the side effect, prints `[Aileron] denied: <reason>`, and
returns control to the prompt with the prompt's own exit code unchanged. The
trap distinguishes the two modes by `[[ $- == *i* ]]` (bash's interactive flag).
Rationale: the agent's invocation model is non-interactive `bash -c`; that path
must halt to satisfy #801. A real human typing in a sandbox shell should not
have their REPL killed by a single denied command. U1 confirms or revises this
split based on the experimental outcome.

### KTD4 — One audit event constant covers allow and deny; payload carries decision and matched pattern

`model.EventTypeSandboxShellDecided` already exists. Per repo convention
(`internal/model/model.go:311`, mirroring `EventTypePolicyEvaluated`),
allow/deny semantics are folded into a single `*.decided` event with the
decision in the payload. The slice does **not** introduce
`EventTypeSandboxShellDenied`. Field keys reused verbatim from slice 5:
`aileron.shell.boundary`, `aileron.shell.command`, `aileron.shell.decision`,
`aileron.shell.reason`, `aileron.shell.latency_ms`, plus optional
`aileron.shell.cwd`, `aileron.shell.path`, `aileron.shell.pid`,
`aileron.shell.ppid`, `aileron.session.id`.

**One new field on deny:** `aileron.shell.matched_pattern` carries the verbatim
source of the matched regex (the value of `AILERON_SANDBOX_SHELL_DENY_PATTERN`)
when `decision="deny"`. Absent on allow and on startup-refusal cases. This
gives incident reviewers attribution without replaying the pattern against the
logged command — important now while the deny trigger is a single regex, and
forward-compatible with a future field like `aileron.shell.matched_rule_id`
once the policy DSL lands. The matched-pattern field is NOT sensitive: the
pattern source already lives in the daemon's env and is not credential
material. U2's existing payload-leak test (`for _, leaked := range
[]string{"leak-me", "env", "output"}`) continues to hold against the deny
path; the matched-pattern field name is unrelated to those guards.

### KTD5 — Mediator script exit codes: allow=0, nonzero=halt-with-message

Today the mediator (`images/sandbox-base/bin/aileron-shell-mediator`) exits 0
on allow and nonzero on anything else (deny, error, missing config). The bashrc
trap currently doesn't distinguish between these — both vetoed today. Slice 6
keeps a uniform "any nonzero = halt with whatever stderr message the mediator
already printed" contract from the trap's point of view. The mediator stays the
source of truth for the user-facing message text (`[Aileron] denied:` vs
`[Aileron] mediation unavailable:`); the trap only decides *how* to halt based
on shell mode (KTD3), not *why*. This keeps the mediator-vs-trap responsibility
split unchanged and minimizes the diff.

### KTD6 — OpenAPI: tighten `decision` to a real enum

`SandboxShellDecisionResponse.decision` is currently `type: string` with the
prose mentioning `deny` and `pending_approval` as future values. The slice
promotes it to `enum: [allow, deny, pending_approval]` so generated Go code
gives the daemon type-safety against typos, and updates the description prose
to say "`allow` and `deny` are accepted; `pending_approval` is a future value."
This is the only spec change and triggers `task generate:api`.

---

## High-Level Technical Design

### Trap dispatch on mediator exit (U3, after U1 locks the mechanism)

The non-prescriptive sketch below shows the dispatch shape the trap takes after
U1 chooses the halt mechanism. The exact bash syntax is implementation detail;
this is for review.

```text
                    ┌─────────────────────────────┐
DEBUG trap fires →  │ recursion guard set?        │── yes ──→ return 0 (allow self-calls)
                    └──────────────┬──────────────┘
                                   no
                                   ▼
                    ┌─────────────────────────────┐
                    │ enable extdebug (lazy)      │
                    │ set _AILERON_IN_TRAP=1      │
                    │ rc = mediator intercept     │
                    │ unset _AILERON_IN_TRAP      │
                    └──────────────┬──────────────┘
                                   ▼
                    ┌─────────────────────────────┐
                    │ rc == 0 ?                   │── yes ──→ return 0 (command runs)
                    └──────────────┬──────────────┘
                                   no   (mediator already printed the message)
                                   ▼
                    ┌─────────────────────────────┐
                    │ interactive shell?  $- == *i* │
                    └──────────────┬──────────────┘
                          yes      │      no
                                   ▼
                    return 1            ◀── (KTD1 hypothesis: `exit <code>`,
                    (soft veto:           candidates C2/C3 also evaluated.
                     side effect          U1 falsifies on Alpine and picks.
                     suppressed,          Whichever wins, the contract is:
                     REPL alive)          "command did NOT run AND shell exit
                                          is nonzero" for bash -c "denied && next",
                                          stderr does NOT leak $BASH_COMMAND text.)
```

### Deny request/response sequence (U2 wire)

```text
agent process inside container
  │
  ▼
bash -c "cmd"  ──→ DEBUG trap installed by BASH_ENV
                      │
                      ▼
                aileron-shell-mediator intercept "cmd"
                      │
                      ▼  POST /v1/sandbox-shell/decide  { command, cwd, shell, pid, ppid }
                      │  Authorization: Bearer <launch token>
                      │  X-Aileron-Session-Id: <session>
                      ▼
                daemon handler (handlers_sandbox_shell.go)
                      │
                      │  (daemon refused to start if AILERON_SANDBOX_SHELL_DENY_PATTERN
                      │   was set but did not compile — KTD2 / R7 startup fail-closed.
                      │   So at request time, pattern is either nil or a valid regex.)
                      │
                      ├── regex matches AILERON_SANDBOX_SHELL_DENY_PATTERN ?
                      │      │
                      │  no  │  yes
                      │      ▼
                      │   200 OK { status:"decided", decision:"deny",
                      │           audit_id:"…",
                      │           reason:"matched deny pattern: <pattern>",
                      │           matched_pattern:"<pattern>" }
                      │      │
                      │      ▼
                      │   audit RecordSuccess(EventTypeSandboxShellDecided,
                      │       { boundary, command, decision:"deny", reason,
                      │         matched_pattern, latency_ms,
                      │         cwd?, shell?, pid?, ppid?, session.id? })
                      ▼
                   200 OK { status:"decided", decision:"allow", ... }   (slice-5 path, unchanged)
```

---

## Implementation Units

### U1. Real-bash gate: falsify or confirm the halt-with-nonzero mechanism on Alpine

**Goal:** Falsify or confirm the KTD1 working hypothesis (C1: trap `exit` under
non-interactive, trap `return 1` under interactive) via real-bash tests against
the **Alpine `images/sandbox-base` image**, not the developer's local bash.
Compare against the two alternate candidates (C2, C3) if C1 fails. Record the
outcome in `Resolved by U1` and feed it into U3.

**Requirements:** R2, R3, R8, R11.

**Dependencies:** none. Blocks U3, U5. U4 depends on U1's chosen mechanism for
its smoke step.

**Execution note:** Experiment-first. Build the failing test cases against the
*current* rcfile first to confirm they fail (validates that the wall the user
described actually exists in this branch), then iterate on rcfile branches
**inside `docker run --rm aileron-sandbox-base:smoke bash -c …`** and pick the
mechanism. Do not touch the daemon (U2) or docs (U5) before U1 settles.

**Files:**

- `internal/sandbox/container/shellrc_test.go` — new failing tests + final
  passing tests
- `internal/sandbox/container/shellrc_alpine_test.go` (new) — Alpine-pinned
  variant that wraps the same scenarios in `docker run aileron-sandbox-base:smoke`
  and skips when Docker is unavailable. Build tag `//go:build sandbox_smoke` if
  the team prefers a build-tagged opt-in, or a plain `requireDocker(t)` skip
  helper otherwise.
- `images/sandbox-base/shell/aileron-bashrc` — scratch variants during
  exploration (final version lands in U3)
- (Local-only scratch script is fine; no commits of throwaway branches.)

**Approach:**

1. Add failing tests against today's rcfile that prove the wall (run both
   locally via `requireBashTooling` and against Alpine via the new
   Docker-backed helper):
   - `TestBashrcNonInteractiveDenyHaltsChain` — `bash -c "touch X && touch Y"`
     with deny stub for the first command, asserts `X` is NOT created (existing
     slice-5 assertion), AND `Y` is NOT created (new), AND `cmd.Wait()` returns
     a nonzero exit (new). Expected to FAIL on `main`.
   - `TestBashrcNonInteractiveDenyExitsNonzero` — `bash -c "denied-cmd"` (no
     chain), asserts exit nonzero. Expected to FAIL on `main`.
   - `TestBashrcInteractiveDenyKeepsRepl` — interactive `--rcfile -ic` with
     deny stub, asserts side effect suppressed (existing) AND bash exit is 0.
     Expected to PASS on `main`; defines KTD3's interactive contract.
   - `TestBashrcDenyDoesNotLeakCommandToStderr` (new — stderr cleanliness):
     after a denied command, stderr contains `[Aileron] denied:` AND does NOT
     contain the raw `$BASH_COMMAND` text or any of its arguments. Protects
     against the `exit`-from-trap candidate (C1) leaking sensitive args via
     bash's own exit diagnostics under `set -eu` or xtrace.
2. Run the failing tests against three candidates inside Alpine
   `images/sandbox-base`:
   - **C1 (working hypothesis from KTD1)**: trap calls `exit <code>` on deny
     under non-interactive (gated on `[[ $- != *i* ]]`).
   - **C2**: trap `return 2` under extdebug (bash documents "simulate a return"
     — verify behavior on Alpine bash 5.x).
   - **C3**: trap unconditionally enables `set -e` + `return 1` (verify
     interaction with `&&` short-circuit and chain halt on Alpine).
3. Confirm or override the KTD1 hypothesis. If C1 passes, mark it confirmed and
   move to U3. If C1 fails on Alpine, pick whichever of C2/C3 passes; if all
   three fail, plan re-opens — `Resolved by U1` records the falsification
   evidence and the slice owner decides next steps before U3 starts. Document
   rejected candidates with a one-sentence reason.
4. Fill in the `Resolved by U1` section at the bottom of the plan before U3
   begins.

**Patterns to follow:** `internal/sandbox/container/shellrc_test.go` helpers
already in place — `bashrcPath`, `installMediatorBin`, `stubDaemon`,
`runBashEnv`, `runBashRC`, `mediationEnv`. Reuse them; do not introduce a new
harness. `requireBashTooling` skips on macOS; the BusyBox/Alpine gate is U4.

**Test scenarios:**

- *Covers R2/R3.* `bash -c "echo first && echo second"` with deny stub for
  `echo first` → `first` not in stdout, `second` not in stdout, exit nonzero,
  `[Aileron] denied:` on stderr.
- *Covers R2.* `bash -c "echo only"` with deny stub → `only` not in stdout,
  exit nonzero, `[Aileron] denied:` on stderr.
- *Covers R6 (no regression).* `bash -c "echo allowed"` with allow stub →
  `allowed` in stdout, exit 0, exactly one daemon call (recursion guard
  preserved).
- *Covers R8 (interactive softness).* `bash --rcfile <rc> -ic "touch X"` with
  deny stub → `X` not created, `[Aileron] denied:` on stderr, bash exit 0 (the
  interactive REPL completed cleanly).
- *Covers R7.* `bash -c "echo X && echo Y"` with the stub daemon torn down
  (closed port) → neither `X` nor `Y` in stdout, exit nonzero, `[Aileron]
  mediation unavailable:` on stderr. (Existing fail-closed test extended with
  chain assertion + exit-code assertion.)
- *Covers KTD1 stderr cleanliness.* `bash -c "denied-cmd --arg=SECRET-12345"`
  with deny stub → stderr contains `[Aileron] denied:` AND does NOT contain
  the literal string `denied-cmd`, `--arg`, `SECRET-12345`, or any other
  substring of the original `$BASH_COMMAND`. The mediator's reason line is
  the only stderr content carrying command-related text.
- *Edge.* `bash -c "denied || echo recovered"` with deny stub → behavior of
  recovery branch under the chosen mechanism is asserted explicitly. Plan
  position: with C1 (`exit`), the recovery branch is also halted (the shell is
  dead); with C2 (`return 2`), the recovery branch may run. **U1 records the
  observed behavior and chooses the candidate that best matches the user's
  intent for `&&` chains; `||` recovery semantics are documented either way.**
- *Edge.* `bash -c "denied; echo next"` (semicolon, not `&&`) → behavior
  recorded. Plan position: under any halt mechanism, the semicolon-separated
  `next` does not run; this matches user intent ("a denied command actually
  blocks").
- *Alpine pin.* All scenarios above are run under `docker run --rm
  aileron-sandbox-base:smoke bash -c …` (via the new Alpine-pinned test
  helper). Local-bash runs via `requireBashTooling` are complementary fast
  iteration; the canonical gate is Alpine.

**Verification:** All new tests are green against the chosen rcfile mechanism;
all existing slice-5 tests in `shellrc_test.go` remain green; `Resolved by U1`
plan section is filled in.

---

### U2. Daemon: deny trigger + audit + spec polish

**Goal:** Make the handler at `internal/app/handlers_sandbox_shell.go`
return `decision: "deny"` with a populated `reason` when a configured pattern
matches the command. Audit it through the existing recorder with no new
attribute keys. Tighten the spec to enumerate `decision` values.

**Requirements:** R1, R5, R6, R9.

**Dependencies:** none. Independent of U1 — runs in parallel with the U1 gate.

**Files:**

- `internal/api/openapi.yaml` — tighten `SandboxShellDecisionResponse.decision`
  to a real enum
- `internal/api/gen/server.gen.go` — regenerated via `task generate:api`
  (never hand-edited per `CLAUDE.md`)
- `internal/app/handlers_sandbox_shell.go` — env-driven deny logic + adjusted
  reason text on allow
- `internal/app/handlers_sandbox_shell_test.go` — deny / allow / bad-pattern /
  no-pattern test cases

**Approach:**

1. **Spec.** Add `enum: [allow, deny, pending_approval]` to
   `SandboxShellDecisionResponse.decision` and update its description to
   "`allow` and `deny` are accepted; `pending_approval` is a future value."
   Add an optional `matched_pattern` field at the schema level (string, omitted
   when allow). Update the path-level description in `/v1/sandbox-shell/decide`
   to drop "allow-only" wording and reflect deny. Run `task generate:api`.
2. **Handler.** At daemon startup (in the `apiServer` constructor or via a
   sync.Once-guarded helper read at the same point env-driven config is read
   today — see `internal/app/handlers_status.go:40`, `internal/app/app.go:317`,
   `internal/app/app.go:543-546` for the convention):
   - Read `AILERON_SANDBOX_SHELL_DENY_PATTERN`.
   - If unset, store `nil` and skip compilation. Allow path is byte-identical
     to slice 5 (R6).
   - If set, compile the regex. **If compilation fails, the daemon refuses to
     start** — return a fatal error from `app.New` (or the equivalent
     constructor) naming the env var and the regex error, so the operator sees
     the misconfig immediately. Do not log-and-continue (KTD2). This is the
     R7 fail-closed posture extended to policy-config errors.
   - If compilation succeeds, store the `*regexp.Regexp` on `apiServer` (or
     in a daemon-scoped config struct if the team prefers).

   On request:
   - Empty stored pattern → allow path **byte-identical** to slice 5 (R6). Keep
     the existing `sandboxShellDecisionReasonAllowOnly` string only when the
     pattern is unset so existing tests stay valid; when the pattern is set
     and does not match the command, allow's reason becomes
     `"command does not match deny pattern"` so audits stay informative.
   - Pattern matches command → return `decision: "deny"`,
     `reason: "matched deny pattern: <pattern source>"` (stable format from
     KTD2), `matched_pattern: <pattern source>`, `audit_id: <recorder id>`,
     `status: "decided"`. Audit with the same `recordSandboxShellDecision`
     helper, passing `decision="deny"`, the reason, and the new
     `aileron.shell.matched_pattern` payload field.
3. **Tests.** Extend `handlers_sandbox_shell_test.go` with:
   - Deny path: pattern set + matching command → 200, body asserts
     `decision="deny"`, `reason` matches the literal regex
     `^matched deny pattern: ` followed by the pattern source,
     `matched_pattern` field equals the pattern source; audit event records
     `aileron.shell.decision="deny"`, the same reason format,
     `aileron.shell.matched_pattern` equals the pattern source, and
     `aileron.shell.latency_ms` non-negative. Payload leak assertion (slice 5's
     `for _, leaked := range []string{"leak-me", "env", "output"}`) is reused
     verbatim and must still pass.
   - Allow path with pattern set: pattern set + non-matching command → 200,
     `decision="allow"`, `reason="command does not match deny pattern"`,
     `matched_pattern` field absent.
   - Allow path with pattern unset: byte-for-byte slice-5 behavior preserved
     (existing test in the file). No new test needed; existing test must stay
     green.
   - **Bad pattern (new — startup refusal):** pattern set to invalid regex
     (e.g., `^(`) at daemon construction → `app.New` (or equivalent) returns a
     non-nil error naming both the env var and the regex error; no daemon is
     constructed, `/v1/sandbox-shell/decide` is never registered. Verified by
     instantiating the daemon in the test with the bad env var present and
     asserting the constructor error.
   - latency_ms still recorded on deny (non-negative).

**Patterns to follow:** `internal/app/handlers_sandbox_shell.go:46-81` for
recorder use; daemon env reads at `internal/app/handlers_status.go:40` and
`internal/app/app.go:317` for `os.Getenv` convention; `audit.Recorder`
interface at `internal/audit/record.go:19-34` (use `RecordSuccess` —
`RecordFailure` is for `*failure.Failure` shapes, which deny is not).

**Test scenarios:**

- *Covers R1 / R5.* Pattern `^rm -rf` + command `rm -rf /` → response
  `decision="deny"`, `reason="matched deny pattern: ^rm -rf"`,
  `matched_pattern="^rm -rf"`; audit event payload has
  `aileron.shell.decision="deny"`, `aileron.shell.reason="matched deny pattern: ^rm -rf"`,
  `aileron.shell.matched_pattern="^rm -rf"`, `aileron.shell.latency_ms`
  non-negative, plus the slice-5 keys; no `env`, `output`, or token strings
  anywhere in the payload JSON.
- *Covers KTD2 stable reason format.* Pattern `git push --force` + command
  `git push --force origin main` → response `reason` matches the literal regex
  `^matched deny pattern: ` followed by the pattern source verbatim. Stability
  of the prefix is the assertion target.
- *Covers R6.* Pattern unset + command `rm -rf /` → response
  `decision="allow"`, same body shape and reason text as slice 5
  (`sandboxShellDecisionReasonAllowOnly`); audit event identical to slice 5;
  `matched_pattern` field absent.
- *Covers R6.* Pattern `^never-matches` + command `ls -la` → response
  `decision="allow"`, `reason="command does not match deny pattern"`,
  `matched_pattern` field absent; audit event records `decision="allow"`.
- *Covers R7 / KTD2 startup-refusal.* Daemon constructor invoked with
  `AILERON_SANDBOX_SHELL_DENY_PATTERN="^("` (invalid regex) → constructor
  returns a non-nil error containing both the env var name and the regex
  error; the test asserts `/v1/sandbox-shell/decide` is never registered. No
  daemon serves the misconfigured pattern.
- *Covers R9.* `go vet`/`go build` pass after `task generate:api`; the
  generated `SandboxShellDecisionResponse.Decision` field's Go type permits
  the literal `"deny"`; the generated struct exposes a `MatchedPattern *string`
  (or equivalent optional-field shape) the handler can populate on deny.

**Verification:** All four daemon-side tests green; existing
`TestDecideSandboxShellCommand_AllowsAndAuditsSanitizedPayload` and
`TestDecideSandboxShellCommand_RequiresCommand` still green; `task generate:api`
clean diff (re-run shows no further changes); `task test` for the `internal/app`
package passes.

---

### U3. Bashrc trap: wire the chosen halt mechanism + CI shellcheck

**Goal:** Replace the slice-5 veto-to-skip trap behavior in
`images/sandbox-base/shell/aileron-bashrc` with the U1-chosen mechanism, gated
on interactive vs non-interactive per KTD3. Wire a `shellcheck -s sh` step
into the GitHub Actions lint job so R12 becomes truly enforced.

**Requirements:** R2, R3, R4, R6, R8, R11, R12.

**Dependencies:** U1 must be resolved.

**Files:**

- `images/sandbox-base/shell/aileron-bashrc` — the final trap implementation
- `internal/sandbox/container/shellrc_test.go` — promote the U1 tests to
  permanent regression tests against the final rcfile (already partially landed
  in U1)
- `internal/sandbox/container/shellrc_alpine_test.go` — same, Alpine-pinned
  variant (lands in U1; U3 keeps it green)
- `images/sandbox-base/bin/aileron-shell-mediator` — **likely unchanged**;
  edit only if U1's mechanism requires distinct deny vs error exit codes (today
  the mediator already prints distinguishing `[Aileron] denied:` vs `[Aileron]
  mediation unavailable:` stderr text, which is sufficient for KTD5).
- `.github/workflows/ci.yml` — add a `Shellcheck` step to the existing lint
  job that runs `shellcheck -s sh` against the three sandbox shell scripts and
  fails the job on any finding.

**Approach:**

1. Apply the U1-chosen mechanism to the trap. Branch on `[[ $- == *i* ]]`
   (interactive) vs default (non-interactive) per KTD3. Preserve:
   - Recursion guard `_AILERON_IN_TRAP` (R11; defeatable in the threat model
     per TM-L2, but still load-bearing for the non-adversarial case).
   - Lazy `shopt -s extdebug` inside the trap function, not at rcfile top
     (load-bearing — see `aileron-bashrc:20-26` and the comment there).
   - Inert short-circuit when `AILERON_SANDBOX_SHELL_MEDIATION` is unset
     (R6, R11).
   - `[Aileron] denied: <reason>` stderr message comes from the mediator
     (R4, KTD5); the trap does not print its own message.
2. Add a `Shellcheck` step to the `lint` job in `.github/workflows/ci.yml`
   that runs:
   ```yaml
   - name: Shellcheck (sandbox shell scripts)
     run: |
       shellcheck -s sh \
         images/sandbox-base/shell/aileron-bashrc \
         images/sandbox-base/bin/aileron-shell-mediator \
         images/sandbox-base/bin/aileron-shell-wrapper
   ```
   (The directive lines above are a recipe sketch, not final YAML — the
   implementer can use the `ludeeus/action-shellcheck@v2` action or an inline
   shell step, whichever matches the team's CI conventions. `shellcheck` is
   pre-installed on `ubuntu-latest` runners, so the inline `run:` form is
   sufficient.)
3. Run `shellcheck -s sh` locally against all three scripts (R12). Fix any
   findings.
4. Run the U1 test suite against the final rcfile — every test must be green,
   both locally and under Alpine.
5. Run the full `internal/sandbox/container` test package and the slice-5
   end-to-end tests; nothing else may regress.

**Patterns to follow:** keep the trap-function shape from slice 5
(`_aileron_mediate` + `trap '_aileron_mediate' DEBUG`). The mediator-script
contract (one POST per top-level command, exit code is the signal) is
unchanged; the rcfile is the only edit point unless U1 explicitly demanded a
mediator change.

**Test scenarios:** see U1. U3 promotes them to permanent regression tests.

**Verification:** U1 tests green against the final rcfile; existing slice-5
tests still green; `shellcheck -s sh images/sandbox-base/shell/aileron-bashrc`
clean; `shellcheck -s sh images/sandbox-base/bin/aileron-shell-mediator`
clean (existing pass preserved).

---

### U4. sandbox-base Docker smoke: deny halts inside the real image, with a real pty

**Goal:** Prove against the *real* `images/sandbox-base` image (Alpine + BusyBox
wget/awk/grep/sed) that `bash -c "denied && next"` does not run `next` AND
exits nonzero, AND that an interactive `bash -ic` under a real pty preserves
the soft-veto + REPL-alive contract (R8). Extends
`.github/workflows/sandbox-base.yml`.

**Requirements:** R2, R3, R4, R7, R8.

**Dependencies:** Conceptually depends on U2 (to understand the deny response
shape so the python mock stub returns the right body), implementation-
independent since U4 uses a python stub rather than calling the real U2
daemon. Hard blocker: U3 (for the rcfile).

**Files:**

- `.github/workflows/sandbox-base.yml` — extend the existing
  `Smoke test shell mediator against a stub daemon` step with deny cases and
  a pty-allocated interactive case

**Approach:**

1. Reuse the existing python `socketserver` stub pattern in the workflow
   (`.github/workflows/sandbox-base.yml:84-122`). Stand up a **second port**
   (e.g. 8098) that always returns a deny body, leaving the existing 8099
   allow stub unchanged. Second port is simplest and keeps allow-side
   regression coverage clean.
2. Add four smoke steps after the existing `routed bash -c with closed port`
   block:
   - `routed bash -c against deny stub → expect command vetoed AND exit nonzero`
     — assert that `echo second` in `bash -c "echo first && echo second"` does
     not appear in stdout AND the `docker run` invocation exits nonzero (use
     `if docker run … ; then echo "expected nonzero"; exit 1; fi`). Grep
     stderr for `[Aileron] denied:`.
   - `routed bash -c against deny stub → expect matched_pattern surfaces on stderr`
     — sanity check that the stable deny-reason format (`matched deny pattern: …`)
     reaches the user-facing stderr via the mediator.
   - `routed bash -c "denied; second" against deny stub → expect "second" does
     not print AND exit nonzero` — covers the semicolon edge case the user
     asked about explicitly.
   - `routed bash -ic under a real pty against deny stub → expect side effect
     suppressed AND bash exit 0 AND [Aileron] denied: on stderr (R8)`. Allocate
     a fake terminal so the interactive branch is actually exercised — use
     `script -qc 'bash -ic "touch /tmp/should-not-exist"' /dev/null` (the
     `script` utility on Ubuntu runners allocates a pty and feeds the inner
     command into it). Assert `/tmp/should-not-exist` was not created AND the
     interactive shell exited cleanly. This is the gate that catches the
     "U1-chosen mechanism passes unit tests but kills the interactive REPL"
     risk; the no-TTY `docker run -ic` shape would silently miss it.
3. Keep the existing allow + closed-port + mediation-off cases unchanged.

**Patterns to follow:** `.github/workflows/sandbox-base.yml:33-198` — the
python stub + `docker run --network host` pattern is the convention; reuse.

**Test scenarios:** the smoke run *is* the test scenario set. The workflow
fails the build if any of the bash-c-with-deny assertions break.

**Verification:** `.github/workflows/sandbox-base.yml` runs green on this PR
(GitHub Actions reports the `Smoke test shell mediator against a stub daemon`
step passing); the deny cases visibly run in the action log; closed-port and
allow cases remain green.

---

### U5. ADR-0021 + sandbox-composition docs update

**Goal:** Document slice 6 in ADR-0021 (amended in place, pre-MVP per memory
rules) and in the user-facing sandbox composition doc, in the same prose
style and length as slices 1-5.

**Requirements:** R8, R10.

**Dependencies:** U1 (the chosen halt mechanism is named in the prose), U2-U4
(shipped behavior is described accurately).

**Execution note:** documentation-only; no tests.

**Files:**

- `docs/src/content/docs/adr/0021-v4-shell-layer-mediation.md`
- `docs/src/content/docs/development/sandbox-composition.md`

**Approach:**

1. **ADR-0021.** Append one paragraph after the slice-5 paragraph (line 41 in
   today's file), in the same prose voice. Cover: deny is now active; the
   chosen halt mechanism (named, with one-clause rationale, taken from the
   `Resolved by U1` section once U1 settles); the interactive vs non-interactive
   split (KTD3); the env-driven deny trigger (KTD2) as "minimal seam this
   slice; real policy is the next slice"; the stable deny-reason format and
   the new `matched_pattern` audit field (KTD4); the **threat model and known
   limits** (TM-L1 through TM-L4) and the explicit posture that slice 6 is
   detection plus reliable veto when the agent cooperates, not untrusted-code
   containment; what is still deferred (approval-pending, container-observed
   round-trip latency, bypass and subshell-inheritance tests, the mitigation
   tracks named in the threat model section).
2. **sandbox-composition.md.** Update the "What This Does Not Do Yet"
   paragraph at the bottom of the page to reflect that deny is now part of the
   active behavior under `AILERON_SANDBOX_SHELL_MEDIATION=1`, and to add a
   short paragraph naming the threat model and the env-strip / guard-tamper /
   interactive-spawn / `$BASH_COMMAND` blind-spot limits in plain language.
   Approval-pending remains the next deferred step. Use plain language; do
   not introduce em-dashes (per docs voice memory).
3. Hyperlink ADR-NNNN references properly (memory rule).
4. Do not call earlier slices "predecessors" or claim this slice "supersedes"
   them — they were intentionally incremental (memory rule on ADR relationship
   language).

**Patterns to follow:** the slice-1-through-slice-5 paragraphs in ADR-0021
itself.

**Test scenarios:** `Test expectation: none -- documentation-only.`

**Verification:** A reviewer can read the ADR and the dev-docs page and
understand exactly what slice 6 added without consulting the diff.

---

## Risks & Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| The U1-chosen halt mechanism passes unit tests but kills the interactive REPL in real Alpine. | Medium | High | U1 is Alpine-pinned (runs candidate selection inside `docker run aileron-sandbox-base:smoke`). U4 smoke runs `bash -c` and `bash -ic` (the interactive case under a real pty via `script -qc`) against the real image; if interactive softness breaks under BusyBox-only PATH or under tini PID 1, U4 catches it before merge. |
| Recursion guard breaks when the chosen mechanism is `exit` from inside the trap — `exit` may unwind through trap state in surprising ways. | Medium | High | U1 explicitly retests `TestBashrcNonInteractiveHitsDaemonOncePerCommand` against every candidate. The chosen candidate must keep "exactly one daemon call per top-level command" green inside Alpine. |
| The `exit`-from-trap candidate (C1) leaks the denied command text to stderr via bash's own exit diagnostics under `set -eu` or xtrace. | Medium | Medium | U1 adds an explicit stderr-cleanliness test that asserts stderr contains `[Aileron] denied:` AND does NOT contain `$BASH_COMMAND` text or arguments (see KTD1). If C1 leaks, U1 falsifies it on this test and falls back to C2 or C3. |
| Adding `enum: [allow, deny, pending_approval]` plus the optional `matched_pattern` field to the spec breaks downstream Go callers because slice-5 code wrote `"allow"` as a bare string. | Low | Medium | The generated type from oapi-codegen for an enum is typically a string alias with constants; existing string-literal call sites still compile. U2 explicitly runs `task generate:api` and `go build ./...` before committing. If the generated shape forces a rename, U2 sequence is: regenerate → fix call sites → keep diff small. |
| The deny-pattern env approach starts a daemon that silently allows everything when the pattern is misconfigured. | N/A | N/A | **Resolved by amended KTD2.** The daemon now refuses to start when `AILERON_SANDBOX_SHELL_DENY_PATTERN` is set but does not compile, returning a fatal error from the constructor. Covered by the new U2 startup-refusal test. R7 fail-closed posture extended to policy-config errors. |
| TM-L1 / TM-L2 / TM-L3 / TM-L4 (env strip, guard tamper, interactive-shell spawn, `$BASH_COMMAND` blind spots) — see `Threat model and limits` section. | N/A | N/A | Acknowledged, not closed, in slice 6. The plan ships honestly via the Threat model section and ADR-0021 amendment (U5). Closing the holes is a separate architecture track (per-exec env injection by a supervisor, off-env recursion guard, wrapper-layer non-interactive enforcement, shallow command unwrapping or a real policy DSL). The slice's posture is detection plus reliable veto for a cooperative agent, not untrusted-code containment. |
| `bash -c "denied; second"` semicolon behavior surprises a future reader expecting `;` to be sequential and *not* halt. | Low | Low | U1 records the observed behavior explicitly; U5 documents it in ADR-0021. U4 smoke also exercises this case. |
| Slice-5 latency_ms field gets dropped on the deny path because the new code path forgets the timing wrap. | Low | Medium | U2 test asserts `aileron.shell.latency_ms` is present and non-negative on the deny audit event. |
| Slice-6 hypothesis (C1) is overturned and none of C2/C3 passes either — plan re-opens mid-slice. | Low | High | U1 is structured as a falsification gate; `Resolved by U1` documents the falsification evidence and the slice owner pauses U3 to redesign before continuing. The four candidates already span the bash control-flow surface; if none halts non-interactive `bash -c` with a nonzero exit, the architecture itself needs reconsideration (which is in-scope to surface, not in-scope to solve, in this slice). |

---

## Open Questions / Deferred Implementation Notes

These are real questions whose answers do not affect the plan boundary but will
be settled during implementation, not by the planner:

- **Whether U1's `||` recovery branch should run after a deny.** Depends on
  the chosen mechanism. U1 records the observed behavior in `Resolved by U1`;
  the team can later redirect if `||` recovery is needed for non-interactive
  scripts — that redirect lives in the approval-pending slice, not here.
- **Whether `AILERON_SANDBOX_SHELL_DENY_PATTERN` belongs in a daemon-scoped
  config struct rather than a process env read.** This slice uses inline env
  read at construction per the established daemon convention; a structured
  config seam is a real refactor for the policy-engine slice that follows.
  Reading happens at startup so a sync.Once-guarded helper plus an injectable
  test seam covers U2's needs without committing to a struct yet.
- **Whether the wrapper-layer non-interactive enforcement (TM-L3 mitigation
  track) folds into the policy-engine slice or its own micro-slice.** The
  change to `aileron-shell-wrapper` is small (~5 lines); the open question is
  sequencing, not feasibility.

These are deferred-but-known:

- Real policy engine / policy DSL (next #801 slice).
- Approval-pending: `decision: "pending_approval"`, `approval_id`, approval
  queue, `aileron wait <id>` (slice after policy engine).
- Container-observed round-trip latency telemetry.
- Bypass attempts (`eval`, `command`, `exec`, function wrappers, base64+eval,
  subshell inheritance) — see TM-L4 in the Threat model section. These are
  architectural blind spots of regex-against-`$BASH_COMMAND`, not just
  unwritten tests. Closing them needs either shallow command unwrapping in
  the mediator or a different match surface entirely, both of which belong
  to the policy-engine slice or its own slice. Test coverage lands when the
  architecture supports it.
- Hard-boundary architecture work (per-exec env injection by a supervisor,
  off-env recursion guard, seccomp/LSM layer) — the mitigation tracks named
  in the Threat model section. Out of scope for the #801 epic at the slice
  level; needs its own ADR.

---

## Resolved by U1

- **Chosen halt mechanism (non-interactive):** **C1 confirmed.** The DEBUG trap
  calls `exit "$rc"` on deny when `[[ $- != *i* ]]`, with `$rc` being the
  mediator's own nonzero exit code (typically 1). This halts the bash process
  immediately, propagates the nonzero exit to the parent, and the rest of any
  `&&` / `;` chain never runs. Verified inside `aileron-sandbox-base:smoke`
  (Alpine 3.23, bash 5.2) via the new `shellrc_alpine_test.go` harness.
- **Rejected candidates and why:**
  - **C2 (trap `return 2` under extdebug):** rejected — `bash -c "denied"` and
    `bash -c "denied && next"` both exit 0 inside Alpine bash 5.2. The
    "simulate a return" semantic suppresses the about-to-run command (slice-5
    behavior) but does not propagate to the shell's exit status, so R2/R3
    fail. Same result as today's `return 1`.
  - **C3 (`set -e` + `return 1` from the trap):** halts the chain and exits
    nonzero (same observable outcome as C1 on the deny path), but `set -e`
    persists in the shell after the trap returns — any subsequent failure
    anywhere in user scripts would now hard-exit unexpectedly. The behavior
    change reaches far beyond deny mediation, which is out of scope for the
    slice and a risk for follow-on work. C1 keeps the shell's `set -e` state
    exactly where the user left it.
- **Interactive behavior:** confirmed — under `[[ $- == *i* ]]` the trap
  returns 1, extdebug suppresses the about-to-run command, the
  `[Aileron] denied:` line reaches stderr, and the REPL stays alive (the bash
  process exit code is whatever the prompt produces; the shell is not killed).
  Exercised by `TestBashrcInteractiveDenyKeepsReplAlive` (host bash) and
  `TestBashrcAlpine_InteractiveDenyKeepsReplAlive` (Alpine `bash -ic`). The
  stricter real-pty assertion lives in U4 via `script -qc`.
- **`||` recovery branch behavior:** recorded — `denied || recover` does NOT
  run the recovery branch under C1. The first deny calls `exit`, the bash
  process is gone, and `||` is never evaluated. This matches user intent for
  the "deny actually blocks" criterion. A future slice that needs `||`
  recovery semantics for non-interactive scripts would have to swap to a
  return-based mechanism with daemon-driven retry signaling, which belongs
  with the approval-pending work, not here.
- **`;` semicolon chain behavior:** recorded — `denied; next` does NOT run
  `next` under C1, for the same reason as `||`: the shell is gone. Matches
  intent.
- **Stderr cleanliness:** confirmed — under C1, bash does NOT emit any exit
  diagnostic that includes `$BASH_COMMAND` or its arguments, even under
  `set -x` or `set -eu`. The only stderr text on deny is the mediator's
  `[Aileron] denied: <reason>` line. Asserted by
  `TestBashrcNonInteractiveDenyDoesNotLeakCommandToStderr` and the Alpine
  variant. Manually probed with `bash -c "set -x; denied-cmd
  --arg=SECRET-12345"` and `bash -c "set -eu; denied-cmd --arg=SECRET-12345"`
  inside the Alpine image; both produced only the `[Aileron] denied:` line on
  stderr.

---

## Sources & Research

- `internal/launch/launcher.go` (slice 5 mediation env block at lines 344-354)
- `internal/app/handlers_sandbox_shell.go` (allow-only handler today)
- `internal/app/handlers_sandbox_shell_test.go` (sanitized audit payload
  assertions, leak guards)
- `internal/api/openapi.yaml:2769-2796, 6437-6459` (path + schema)
- `internal/api/gen/server.gen.go` (do not hand-edit; regenerated by `task
  generate:api` per repo `CLAUDE.md`)
- `internal/sandbox/container/shellmediator_test.go` (mediator test harness;
  `TestInterceptDenyDecisionFailsClosed` already asserts nonzero exit +
  `[Aileron] denied:` on the mediator wire)
- `internal/sandbox/container/shellrc_test.go:219-229` (slice-5 comment
  explicitly hands slice 6 the chain-halt + exit-code work)
- `internal/model/model.go:311` (`EventTypeSandboxShellDecided` constant
  shared across allow + deny per repo convention)
- `internal/audit/record.go:19-34` (`Recorder` interface)
- `internal/app/handlers.go:126, 141` (`writeJSON`, `writeError`)
- `internal/app/handlers_status.go:40`, `internal/app/app.go:317, 543-546`
  (existing inline `os.Getenv("AILERON_*")` daemon convention)
- `images/sandbox-base/Containerfile` (Alpine + bash/wget/awk/grep/sed apk add)
- `images/sandbox-base/bin/aileron-shell-mediator` (allow=0, nonzero=fail
  closed contract)
- `images/sandbox-base/bin/aileron-shell-wrapper`
  (`/usr/local/bin/{bash,sh}` baked ahead of real shells on PATH)
- `images/sandbox-base/shell/aileron-bashrc` (today's `return 1` veto;
  recursion guard; lazy extdebug)
- `.github/workflows/sandbox-base.yml:84-198` (existing image smoke harness
  with python stub daemon on 127.0.0.1:8099)
- `Taskfile.yml:21-30` (`generate:api` recipe)
- ADR-0021 ([`docs/src/content/docs/adr/0021-v4-shell-layer-mediation.md`](/adr/0021-v4-shell-layer-mediation/))
- ADR-0017 ([`docs/src/content/docs/adr/0017-sandbox-composition.md`](/adr/0017-sandbox-composition/))
- ADR-0015 ([`docs/src/content/docs/adr/0015-launch-audit-scope.md`](/adr/0015-launch-audit-scope/))
- Tracking: [#801](https://github.com/ALRubinger/aileron/issues/801) (parent
  [#747](https://github.com/ALRubinger/aileron/issues/747)); predecessor PR
  [#949](https://github.com/ALRubinger/aileron/pull/949)
