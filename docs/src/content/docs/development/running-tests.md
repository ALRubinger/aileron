---
title: "Running Tests"
description: "Unit, integration, race, coverage: what CI runs and how to reproduce a CI failure locally, plus the by-hand black-box system suite."
order: 4
---

Aileron leans heavily on the test suite. Every PR runs the full Go test set on Linux and Windows, the docs build, the UI tests, and a Playwright-driven end-to-end suite. This page is the contributor's view of the same surface.

## Run everything

```sh
task test
```

This is the full local equivalent of what CI runs. It exercises the Go suite, the docs tests, the UI tests, and the Playwright integration tests. Expect ~5–10 minutes on a modern machine.

For a faster inner loop, run individual targets:

```sh
task test:go              # Go unit tests across the workspace
task test:go:cover        # Go unit tests with coverage summary
task test:go:ci           # what CI runs: race + coverage + JUnit
task test:docs            # docs site unit tests (rehype plugins, etc.)
task test:ui              # UI unit and component tests
task test:integration     # Playwright + running services
task test:e2e:integration # Playwright E2E against a real stack
```

## Run a single Go package

The Taskfile's `test:go` target wraps `go test` across the workspace. For tight iteration on one package, `go test` directly is faster:

```sh
go test ./internal/sandbox/...
go test ./internal/cstore -run TestForwarder -v
go test ./internal/wrap -coverprofile=/tmp/cov.out
```

The `go.work` workspace handles module resolution; no manual `cd` required.

## Race detector

```sh
go test ./internal/sandbox -race
```

The sandbox package has the densest concurrency surface (per-invocation state, shared executor, audit emission). Run with `-race` whenever you change any of those paths. CI does the same automatically.

## Coverage

The project doesn't pin a hard coverage threshold, but the convention is:

- **>80% on new code** is the working bar.
- **Don't chase metrics** on filesystem-error wrappers, concurrent-install race recovery, or other paths where the test fixture would be brittle and not catch real bugs. Tests should strengthen Aileron, not satisfy a metric.
- **Bug fixes need a regression test** that fails before the fix and passes after.

To see what's covered:

```sh
task test:go:cover
```

Or for a single package:

```sh
go test ./internal/cstore -coverprofile=/tmp/cov.out
go tool cover -func=/tmp/cov.out
go tool cover -html=/tmp/cov.out  # opens an HTML report in the browser
```

## Linting

```sh
task lint        # everything
task lint:go     # go vet across the workspace
task lint:docs   # docs site type-check
task lint:webapp # webapp type-check
```

`golangci-lint` is recommended but not required locally. CI runs `go vet` plus a stricter check.

## Reproducing a CI failure

CI's Go suite runs with `task test:go:ci`. To reproduce locally:

```sh
task test:go:ci
```

This runs with `-race`, full coverage, and JUnit output (under `test-results/`). Most CI failures reproduce on the first run.

If a test passes locally but fails in CI, the usual suspects are:

- **Goroutine leaks or races** — surface under `-race`; the inline `task test:go` skips it.
- **TempDir vs HOME** — tests that touch `~/.aileron/` need `t.Setenv("HOME", t.TempDir())`. The CI runners have no fallback path.
- **Time-of-day or timezone** — tests that compare against `time.Now()` without an injected clock will be flaky on slow CI runners.

## System tests (black-box CLI)

The system-test suite sits above the unit, integration, and sandbox-integration layers. It builds the shipped `aileron` binary and drives the real `aileron launch <agent> -- <agent-flag> "..."` path against a live Docker sandbox, for example `aileron launch codex -- exec "..."` or `aileron launch claude -- -p "..."`, then asserts on the result with shell and `jq`. The lower layers prove that Docker works on the host. The `test:go` unit layer exercises Go functions in isolation. The `task test:integration` layer runs Playwright against running services. The `integration_sandbox` Go tests call `docker run` and the sandbox Go functions directly. The system suite proves that `aileron launch` itself correctly drives Docker on the host. It does not replace any of those layers, and it sits above them.

### Run it

```sh
task test:system               # lib contract tests + harness smoke + the codex and claude scenarios
task test:system:lib           # POSIX-shell contract tests for the shared scenario library (no Docker, CI-safe)
task test:system:smoke         # harness self-test: build fires, Docker precondition gates, defer cleanup runs
task test:system:launch:codex  # the codex scenario in isolation: aileron launch codex -- exec "..."
task test:system:launch:claude # the claude scenario in isolation: aileron launch claude -- -p "..."
```

Each agent scenario builds a fresh `aileron` (plus the Linux `aileron-mcp` sibling), runs the launch once, and on exit a deferred cleanup removes the sandbox container and the temporary workspace even when an assertion failed.

### Host prerequisites

- **A reachable Docker daemon.** On macOS and Windows this means Docker Desktop running; on Linux it means `dockerd`. The suite checks `docker info` before any launch.
- **The target agent's auth already present.** The codex scenario expects `~/.codex/auth.json` (created by `codex login`). The claude scenario expects `~/.claude/.credentials.json` (created by `claude /login`). v1 does not inject any LLM secret; you authenticate once with your own `aileron launch <agent>` login and the suite reuses that file.
- **Optional: a running Aileron daemon** if you want the audit round-trip assertion to read real records (`AILERON_STATE_DIR` defaults to `~/.aileron`).

A missing prerequisite stops the run immediately and prints the exact remediation command, for example `Authenticate first with: claude /login`. The suite never silently skips a scenario when a prerequisite is absent.

A live agent scenario needs a real login and consumes LLM tokens, so it is run by hand. The headless path validates the wiring without launching:

```sh
task --dry test:system:launch:codex   # compiles the target, resolves deps and preconditions, does not launch
```

### Cross-OS

The same suite runs unmodified on Ubuntu, Fedora, macOS, and Windows, with the container path included on all four. Task runs each target's command steps through its embedded `mvdan/sh` interpreter, so the Taskfile-level shell logic is portable without a host Bash. Windows uses the stdio exec path and does not use a Unix PTY. OS and distribution versions are unpinned in v1.

The launch container path is not gated by the spawn-primitive availability probes (`internal/sandbox/sandbox_available_*.go`, [ADR-0014](/adr/0014-spawn-sandbox-technology/)). Those probes guard the separate spawn-primitive OS confinement subsystem. The `aileron launch` container path works on all four OS families regardless of that gate, so the system suite runs everywhere Docker runs.

### Scope boundary

v1 is portable and run by hand. `task test:system` is not wired into CI. The maintainer runs it on real hosts across the four OS families. CI-matrix automation, including self-hosted runners, cloud VMs, and secret injection, is deferred to a later initiative.

The human-driven manual acceptance this suite complements is tracked in [issue #962](https://github.com/ALRubinger/aileron/issues/962). The scenario bodies and the shared probe library are documented in `test/system/README.md` in the repository.

## Testing philosophy

Per the project's CLAUDE.md, tests are written against the contract of the code (inputs, outputs, side effects, error conditions defined by the function signature or API spec), never against implementation internals. A refactor that preserves the contract should leave the suite green; if it doesn't, the test was coupled to internals.

Two consequences:

- **Happy path is mandatory.** A test that only asserts on failure modes tells you nothing about whether the feature works.
- **Implementation accidents are not contracts.** If a test passes because of how the code happens to be structured (e.g., "this fails because it tries to reach Google"), that's a mirror, not a test.

See the project's root CLAUDE.md for the full statement.

## See also

- [Building from Source](/development/building-from-source/)
- [Submitting Changes](/development/submitting-changes/)
