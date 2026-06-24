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
  claude.sh            the claude scenario body (#1477)
  scenario/            shell-free Go scenario driver (#1624), its own module,
                       run by hand with `go run ./test/system/scenario codex`;
                       NOT in GO_TEST_PACKAGES, so the live path never runs in CI
    main.go            agent-name dispatch (codex today; claude in #1625)
    scenario.go        the live orchestration (background launch, reap,
                       discovery poll, probes, sentinel read, R10 audit)
    docker.go          thin docker ps/inspect/exec shellouts returning raw facts
  lib/
    assert.sh          generic assert/log helpers (sourced)
    probes.sh          agent-agnostic R8 wiring-invariant probes (sourced)
    runner.go          AgentConfig + CodexConfig + BuildExecArgs/BuildPrompt
    sentinel.go        per-run sentinel (R9) generation
    audit.go           pure R10 audit event-record count + assertion
    scenario.go        pure R8.1/8.3/8.4/8.5 decision predicates (docker-free)
    *_test.go          Go contract suite for the shell library and the pure Go
                       scenario logic plus the launch-target taskenv wiring
                       regression; needs only the Go toolchain, no Docker and no
                       shell (CI-safe, Windows)
```

The Go driver is the shell-free equivalent of `codex.sh`. Both still exist
side by side: the Taskfile target `test:system:launch:codex` runs `codex.sh`
today, and the Go path is added and documented here. `codex.sh` (and
`claude.sh`/`probes.sh`/`assert.sh`) stay on disk and functional; the Taskfile
rewire and the shell retirement happen in #1626. The pure decision logic the Go
driver depends on lives in `lib/` (`systestlib`) and is covered by the standard
`go test ./test/system/lib/...` CI job; the `scenario/` module holds only the
impure docker/exec/poll plumbing and has no tests by design (it is the live
path, excluded from CI).

## Static gate (what runs unattended vs. by hand)

| Layer | Command | Needs | Runs in CI / headless? |
| --- | --- | --- | --- |
| Library contract tests | `task test:system:lib` | Go toolchain | yes, runs unattended and on Windows (Go contract suite, no Docker, no shell) |
| Scenario wiring compile | `task --dry test:system:launch:codex` / `:claude` | nothing | yes — compiles the target, does not launch |
| Go scenario driver compile | `go build ./test/system/scenario` | Go toolchain | yes — compiles the driver; its own module, not in GO_TEST_PACKAGES |
| Live codex scenario (shell) | `task test:system:launch:codex` | Docker + `codex login` + tokens | **no — by hand on a real host** |
| Live codex scenario (Go) | `go run ./test/system/scenario codex` | Docker + `codex login` + tokens | **no — by hand on a real host** |
| Live claude scenario | `task test:system:launch:claude` | Docker + Claude login (vault, ADR-0025) + tokens | **no — by hand on a real host** |

The headless path validates the **wiring** (the target compiles, the build
dependency and preconditions resolve, the scenario library passes its Go
contracts) without performing a live `aileron launch`. The live scenario is the only
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

## Run the codex scenario by hand (shell-free Go)

`test/system/scenario` is the shell-free Go equivalent of `codex.sh`. It runs
the same R7a/R8.1-8.6/R9/R10 assertions, delegating every decision to the pure
`lib/` (`systestlib`) package, and holds only the impure docker/exec/poll
plumbing. It needs the same prerequisites as the shell scenario (Docker, a
`codex login`, and optionally a running daemon for R10). It reads its inputs
from the environment, so build an `aileron` first and point the driver at it:

```sh
task build                                  # produces build/aileron
AILERON_BIN="$PWD/build/aileron" \
WORKSPACE="$(mktemp -d)" \
AILERON_STATE_DIR="$HOME/.aileron" \
  go run ./test/system/scenario codex
```

`EXPECTED_IMAGE` overrides the default expected image ref the same way the shell
scenario does. `AILERON_STATE_DIR` is optional: when unset the R10 audit
assertion is skipped with a logged note. The driver imports the scenario library
as a Go package, so the `AILERON_SYSTEST_LIB` variable the shell path needs is
vestigial for the Go path and is not read.

The driver is its own nested Go module outside `./test/system/lib/...`, so the
standard `go test ./test/system/lib/...` coverage and vet CI job never compiles
or executes it and cannot start a real container. Its pure logic is covered by
the `systestlib` unit tests; the driver itself has no tests by design.

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
  1. **Image** — `.Config.Image` references the
     `ghcr.io/alrubinger/aileron-sandbox-codex` repo with either a floating tag
     (`:edge` for a dev build, `:latest` for a release) or an `@sha256:` digest
     pin (the reproducible-release path resolves the floating ref to a digest),
     and `.State.Running == true`. Override with `EXPECTED_IMAGE=…` to assert a
     specific ref exactly.
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

## Run the claude scenario by hand

The claude scenario (`test/system/claude.sh`) is the sibling of the codex one
and reuses the same shared R8 probes. It differs only in the claude-specific
bindings (issue #1477):

- **Image** — `.Config.Image` references the
  `ghcr.io/alrubinger/aileron-sandbox-claude` repo with a floating tag
  (`:edge` dev, `:latest` release) or an `@sha256:` digest pin
  (`EXPECTED_IMAGE=…` to assert a specific ref exactly).
- **MCP** — claude is wired via the `--mcp-config <json>` CLI flag
  (`internal/launch/agents/claude.go` `ConfigureMCP`), **not** a `config.toml`.
  The probe asserts the `--mcp-config` flag and the `"aileron"` server marker on
  the running container's command line (`probe_mcp_cmdline`), plus the shared
  agent-agnostic core (`aileron-mcp` present + executable, daemon-wiring env
  vars set).
- **Credentials** — `/home/agent/.claude/.credentials.json`, mode `0600`, with
  its parent dir bind-mounted writable (claude rotates the token in-session).
- **Batch flag** — claude's non-interactive flag is `-p`, so the scenario
  drives `aileron launch claude -- -p "<prompt>"`.

The container name prefix, daemon-reachability, teardown, R9 sentinel, and R10
audit round-trip are identical to codex and reuse the shared probes unchanged.

Prerequisites (the scenario fail-fasts with the exact remediation if missing):

1. A reachable Docker daemon (`docker info`).
2. A Claude login resolvable by the launcher's AuthSpec. Claude's credentials
   are vault-backed (`agents/claude/oauth`, ADR-0025), so the precondition
   passes when EITHER the host file `~/.claude/.credentials.json` exists OR the
   vault holds the entry (`aileron vault list --scope agent` prints
   `agents/claude/oauth`). Run `aileron launch claude` once and complete the
   in-flow login (or `claude /login`) to populate it.
3. A running Aileron daemon if you want the R10 audit assertion to read real
   records (`AILERON_STATE_DIR` defaults to `~/.aileron`).

Then:

```sh
task test:system:launch:claude
```

> **Cross-OS note.** On Windows there is no Unix PTY, so `aileron launch` uses
> the stdio exec path rather than the container PTY; run the by-hand scenario on
> macOS or Linux for the full container path. This headless authoring host has
> no Claude vault auth, so the scenario here is statically validated only
> (`task --dry`, `shellcheck`); a green live run is verified by hand on a
> claude-authed host.

## Editing the shared library

`lib/assert.sh` and `lib/probes.sh` are **agent-agnostic**. Codex-specific
values (image suffix, config/auth paths, the MCP block marker, the exec
prompt) are passed in from `codex.sh`, never hard-coded in the library, so the
claude scenario (#1477) reuses every function verbatim. After any change, run:

```sh
task test:system:lib                 # contract tests (CI-safe)
shellcheck -x -s sh test/system/lib/*.sh test/system/codex.sh test/system/claude.sh
```
