# `aileron launch` system tests

End-to-end system tests that exercise the real `aileron launch <agent>` path
against a live Docker sandbox. They are **run by hand on a real host** (they
pull a sandbox image, start a container, and drive a real agent CLI that needs
a login and consumes LLM tokens), so they are deliberately **not** wired into
CI as a gating job — see the static gate below.

The harness (target tree, build dependency, fail-fast preconditions, `defer`
cleanup) lives in the root `Taskfile.yml` under the `test:system:*` tree
(added in #1475). This directory holds the per-agent scenario bodies and the
shared scenario library.

```
test/system/
  README.md            this file
  codex.sh             the codex scenario body (#1476)
  lib/
    assert.sh          generic assert/log helpers (sourced)
    probes.sh          agent-agnostic R8 wiring-invariant probes (sourced)
    assert_test.sh     contract tests for assert.sh   (CI-safe, no Docker)
    probes_test.sh     contract tests for probes.sh   (CI-safe, stubbed docker)
```

## Static gate (what runs unattended vs. by hand)

| Layer | Command | Needs | Runs in CI / headless? |
| --- | --- | --- | --- |
| Library contract tests | `task test:system:lib` | nothing | yes — pure POSIX shell, `docker` stubbed |
| Scenario wiring compile | `task --dry test:system:launch:codex` | nothing | yes — compiles the target, does not launch |
| Live codex scenario | `task test:system:launch:codex` | Docker + `codex login` + tokens | **no — by hand on a real host** |

The headless path validates the **wiring** (the target compiles, the build
dependency and preconditions resolve, the shell library passes its contracts)
without performing a live `aileron launch`. The live scenario is the only
thing that needs an authenticated agent, and it is invoked manually.

## Run the codex scenario by hand

Prerequisites (the scenario fail-fasts with the exact remediation if missing):

1. A reachable Docker daemon (`docker info`).
2. A host-side codex login: `codex login` writes `~/.codex/auth.json`.
3. A running Aileron daemon if you want the R10 audit assertion to read real
   records (`AILERON_STATE_DIR` defaults to `~/.aileron`).

Then:

```sh
task test:system:launch:codex
```

The target builds a fresh `aileron` (+ the Linux `aileron-mcp` sibling),
checks the preconditions, runs `test/system/codex.sh`, and on exit the
deferred cleanup removes the sandbox container and the temp workspace even if
an assertion failed.

## What the codex scenario asserts

The scenario drives `aileron launch codex -- exec "<prompt>"` **once**. It
never runs `docker run` directly — the launcher orchestrates the image pull
and container run, and the scenario inspects the result out-of-band by the
container's stable name prefix `aileron-sbx-` (the launcher derives the
session id at runtime, so the exact name is discovered, not predicted).

- **R7a — arg forwarding.** Post-`--` tokens flow `LaunchConfig.Args`
  (`cmd/aileron/main.go` `applyTrailingLaunchFlags`) →
  `internal/launch/launcher.go` `command = append(command, config.Args...)` →
  `internal/sandbox/container/runtime.go` image then `opts.Command`. The agent
  only runs our `exec` prompt if forwarding worked; the R9 sentinel is the
  proof.
- **R8 — wiring invariants** (shared `lib/probes.sh`, designed so the claude
  scenario #1477 can reuse every function verbatim once it lands):
  1. **Image** — `.Config.Image` is
     `ghcr.io/alrubinger/aileron-sandbox-codex:edge` for a dev build
     (`imageTag` → `edge`), `:latest` for a release; `.State.Running == true`.
     Override with `EXPECTED_IMAGE=…` to assert a release tag or a digest pin.
  2. **MCP** — `/usr/local/bin/aileron-mcp` present + executable; the codex MCP
     config at `/home/agent/.codex/config.toml` contains
     `[mcp_servers.aileron]`; the daemon-wiring env vars `AILERON_URL`,
     `AILERON_COMMS_URL`, `AILERON_SESSION_ID`, `AILERON_APPROVAL_URL`,
     `AILERON_TOKEN` are all set in the container.
  3. **Credentials** — `/home/agent/.codex/auth.json` exists, mode `0600`, and
     its parent dir is a bind mount.
  4. **Daemon reachable** — `AILERON_URL` host is `host.docker.internal`
     (loopback rewrite); on Linux Docker the container's `ExtraHosts` carries
     `host.docker.internal:host-gateway`. On macOS/Windows Docker Desktop the
     gateway is built in, so that sub-check is Linux-gated.
  5. **Teardown** — no `aileron-sbx-*` container survives (`docker run --rm`);
     SIGINT/SIGTERM triggers `docker stop` first.
  6. **Exit code** — the `aileron launch` process exits 0.
- **R9 — deterministic result.** The agent writes a per-run sentinel
  `AILERON_SYSTEST_OK_<runid>` into the mounted workspace at
  `/home/agent/workspace/.aileron-systest-sentinel`; the test reads
  `<workspace>/.aileron-systest-sentinel` on the host and asserts byte-exact
  equality. The `<runid>` is fresh each run, so a stale file cannot pass.
- **R10 — audit round-trip (authored; deterministic where the daemon logs).**
  The agent calls the built-in `http_request` MCP tool against the daemon's
  own `${AILERON_URL}/healthz`. That routes through the daemon's `/comms/http`
  handler, which appends a message audit entry to today's audit JSONL
  (`internal/audit/local.go` `MessageEntry`, written by
  `handlers_comms.go logCommsEvent`). The R10 jq assertion is **conditional**:
  it runs only when `AILERON_STATE_DIR` points at a daemon state dir (it
  defaults to `~/.aileron`), and it is skipped with a logged note when that
  dir is unset, so a run without a daemon does not fail on a missing audit
  trail. When enabled, the test jq-asserts today's audit file has event
  records for this run window.

  When the daemon's OpenTelemetry tracing is enabled, the richer span record
  carrying `aileron.action.name` lands separately under
  `<stateDir>/traces/spans-YYYY-MM-DD.jsonl`. To tighten R10 to a specific
  action span by hand, enable tracing and jq that file:

  ```sh
  # the session id is printed in the launch banner on stderr
  jq -c 'select(.Attributes[]? | .Key == "aileron.action.name")' \
    "$HOME/.aileron/traces/spans-$(date +%F).jsonl"
  ```

## Editing the shared library

`lib/assert.sh` and `lib/probes.sh` are **agent-agnostic**. Codex-specific
values (image suffix, config/auth paths, the MCP block marker, the exec
prompt) are passed in from `codex.sh`, never hard-coded in the library, so the
claude scenario (#1477) reuses every function verbatim. After any change, run:

```sh
task test:system:lib                 # contract tests (CI-safe)
shellcheck -x -s sh test/system/lib/*.sh test/system/codex.sh
```
