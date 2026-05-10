// Package sandboxtest provides reusable test helpers for the spawn
// primitive (per ADR-0002, ADR-0014).
//
// Two helpers are exposed:
//
//   - **RecordingExecutor**: a [sandbox.SpawnExecutor] that records
//     every envelope it sees and returns a scripted [sandbox.SpawnResult].
//     Use this when the test wants to verify the gate, the host
//     functions, and the plumbing without forking a real subprocess.
//     Substituted via [sandbox.WithSpawnExecutor].
//
//   - **FakeBinary**: a builder that writes a scripted executable to
//     a tempdir. The fake binary emits configurable stdout, stderr,
//     and exit code, and optionally records its own argv, env, and
//     cwd to a JSON file the test reads back. Use this when the test
//     wants to exercise the platform sandbox layer through the real
//     `os/exec` path.
//
// Connector authors: this package is the canonical mock for
// spawn-primitive integration tests in your own connector repo.
// Import it as `sandboxtest`, drop a [RecordingExecutor] into the
// runtime via [sandbox.WithSpawnExecutor], and assert on the captured
// envelopes the way you would on a [httptest.Server]'s recorded
// requests.
//
// This package is a test harness, not production code. Coverage for
// it is therefore reported separately from the runtime's coverage
// figure.
package sandboxtest
