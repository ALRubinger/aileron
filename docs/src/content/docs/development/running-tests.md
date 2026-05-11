---
title: "Running Tests"
description: "Unit, integration, race, coverage. What CI runs, and how to reproduce a CI failure locally."
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

## Testing philosophy

Per the project's CLAUDE.md, tests are written against the contract of the code (inputs, outputs, side effects, error conditions defined by the function signature or API spec), never against implementation internals. A refactor that preserves the contract should leave the suite green; if it doesn't, the test was coupled to internals.

Two consequences:

- **Happy path is mandatory.** A test that only asserts on failure modes tells you nothing about whether the feature works.
- **Implementation accidents are not contracts.** If a test passes because of how the code happens to be structured (e.g., "this fails because it tries to reach Google"), that's a mirror, not a test.

See the project's root CLAUDE.md for the full statement.

## See also

- [Building from Source](/development/building-from-source/)
- [Submitting Changes](/development/submitting-changes/)
