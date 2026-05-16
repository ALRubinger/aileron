package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// HelpTimeout caps how long RunHelp waits for `<binary> --help`
// to produce output before killing the subprocess. Five seconds
// matches the existing unsandboxed runner the manual `aileron
// action wrap` flow uses (internal/wrap.HelpRunnerExec), so the
// introspection deadline is the same whether the caller asks for
// the sandboxed or unsandboxed variant.
const HelpTimeout = 5 * time.Second

// RunHelpResult is the captured outcome of a single sandboxed
// `<binary> --help` invocation. Both Stdout and Stderr may be
// populated regardless of ExitCode; many CLIs write usage to
// stderr while still exiting non-zero (notably `curl`), and
// callers parse whichever stream contains the help text.
type RunHelpResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// RunHelp invokes `<programPath> --help` under the platform
// spawn sandbox the runtime uses for declared connector actions.
// Confinement matches what would apply to a hub-published
// connector calling spawn at runtime:
//
//   - fs_read scoped to `cwd` only (the directory the user
//     invoked the CLI from). No `$HOME`, no user-config dirs.
//   - fs_write denied entirely.
//   - network egress denied entirely (no [capabilities.network]
//     declared, so the sandbox engages without a proxy port).
//   - HelpTimeout (5s) deadline; the subprocess is killed if it
//     hasn't exited by then.
//   - stdout / stderr capped to [cstore.DefaultMaxStdoutBytes]
//     and [cstore.DefaultMaxStderrBytes]; output beyond the cap
//     is dropped with a structured truncation marker per ADR-0014.
//
// `subcommandPath` is the verb chain prepended before `--help`.
// Nil/empty yields `<programPath> --help` (the root-help call);
// `["issues", "create"]` yields `<programPath> issues create
// --help` so the introspector can walk Cobra-style nested verb
// trees. Each call is its own sandboxed invocation — callers
// running many paths in sequence pay the per-call sandbox-startup
// cost N times.
//
// `programPath` must be absolute. RunHelp returns the
// [ErrSpawnUnavailable] wrapper from [SandboxAvailable] when the
// host can't honor the confinement the docstring promises —
// callers should treat that as a hard install-time failure
// rather than fall back to unsandboxed exec.
//
// Intended caller: the `aileron cli add` introspector (issue
// #749), which parses the captured stdout via internal/wrap's
// `--help` parser. Generalizable to any other one-shot sandboxed
// invocation that doesn't need a fully-formed connector manifest
// on disk first.
func RunHelp(ctx context.Context, programPath, cwd string, subcommandPath ...string) (RunHelpResult, error) {
	if err := SandboxAvailable(); err != nil {
		return RunHelpResult{}, err
	}
	if !filepath.IsAbs(programPath) {
		return RunHelpResult{}, fmt.Errorf("run_help: program path %q must be absolute", programPath)
	}
	if cwd == "" {
		return RunHelpResult{}, fmt.Errorf("run_help: cwd is required")
	}
	if !filepath.IsAbs(cwd) {
		return RunHelpResult{}, fmt.Errorf("run_help: cwd %q must be absolute", cwd)
	}

	argv := make([]string, 0, 2+len(subcommandPath))
	argv = append(argv, filepath.Base(programPath))
	argv = append(argv, subcommandPath...)
	argv = append(argv, "--help")
	envelope := SpawnEnvelope{
		Program: programPath,
		Argv:    argv,
		Cwd:     cwd,
	}
	// fs_read scope: the user's cwd (so the introspector can
	// see config the user is pointing at), plus the platform's
	// system-essentials (libc, ld.so, /bin/sh, etc.) that
	// Landlock would otherwise deny on Linux — shebang-style
	// CLIs and any libc-linked binary fail to start without
	// these. Non-Linux platforms get nil from systemReadScopes
	// since their sandbox profiles include system paths in
	// their permissive baselines.
	fsRead := append([]string{cwd}, systemReadScopes()...)
	limits := SpawnLimits{
		MaxStdoutBytes: cstore.DefaultMaxStdoutBytes,
		MaxStderrBytes: cstore.DefaultMaxStderrBytes,
		FSRead:         fsRead,
	}

	ctx, cancel := context.WithTimeout(ctx, HelpTimeout)
	defer cancel()

	res, err := defaultSpawnExecutor{}.Spawn(ctx, envelope, limits)
	out := RunHelpResult{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}
	return out, err
}
