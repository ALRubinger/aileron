//go:build linux

package spawnhelper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Run is the entry point the daemon's main() dispatches to when
// it sees [HelperArgvMarker] as its first argv. The function
// parses the request, applies Landlock filesystem restrictions,
// optionally starts the [RunSpawnShim] network bridge, then
// either:
//
//   - `syscall.Exec`s into the wrapped program (no network shim
//     needed; helper becomes the CLI), or
//   - forks the wrapped CLI as a child and waits while the shim
//     goroutine serves connections from inside the namespace
//     (network shim needed; helper stays alive to host the
//     shim, exits with the CLI's status when it finishes).
//
// Never returns on success in the no-shim path; replaces the
// process via syscall.Exec. In the shim path returns after the
// wrapped CLI exits with the CLI's exit code. On any failure
// (malformed request, Landlock unavailable, shim startup error,
// exec failure) writes a structured error message to stderr
// and exits with code 1 so the runtime audit row records a
// sandbox-setup failure rather than a silently unconfined
// spawn.
func Run(encodedReq string) {
	req, err := DecodeRequest(encodedReq)
	if err != nil {
		failHelper("decode request: %v", err)
	}

	if err := applyLandlock(req.FSRead, req.FSWrite); err != nil {
		failHelper("apply Landlock: %v", err)
	}

	// Default env is os.Environ() when the request didn't carry
	// one; the runtime always provides Env in production but the
	// fallback keeps the helper testable in isolation.
	env := req.Env
	if env == nil {
		env = os.Environ()
	}

	if req.ProxyUDSPath == "" {
		// No network shim: syscall.Exec replaces this process
		// with the wrapped CLI.
		if req.Cwd != "" {
			if err := os.Chdir(req.Cwd); err != nil {
				failHelper("chdir %s: %v", req.Cwd, err)
			}
		}
		if err := syscall.Exec(req.Program, req.Argv, env); err != nil {
			failHelper("exec %s: %v", req.Program, err)
		}
		return // unreachable
	}

	// Network shim: start RunSpawnShim on a namespace-local
	// loopback port, then fork the wrapped CLI as a child of
	// this helper process. The shim and the CLI become siblings
	// inside the namespace; when the CLI exits we cancel the
	// shim and propagate the CLI's exit code.
	runWithShim(req, env)
}

// runWithShim implements the network-mediated spawn path. The
// helper retains its PID-1 role inside the namespace, hosts the
// shim goroutine, forks the wrapped CLI as a child, and exits
// with the child's status once it terminates.
//
// Lifecycle:
//
//  1. Listen on `127.0.0.1:0` inside the namespace (the
//     CLONE_NEWNET sets up an isolated loopback the kernel
//     auto-brings-up).
//  2. Shim goroutine bridges accepted connections to the host
//     UDS via filesystem path. (CLONE_NEWNS does not isolate
//     filesystem access by default; the daemon's UDS is reachable.)
//  3. Inject `HTTPS_PROXY` / `HTTP_PROXY` into the wrapped CLI's
//     env pointing at the in-namespace loopback port.
//  4. Start the wrapped CLI as a child via `exec.Cmd`.
//  5. Wait for the child to exit; cancel the shim context.
//  6. Exit with the child's exit code (preserves non-zero
//     statuses up to the runtime).
func runWithShim(req Request, env []string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan string, 1)
	shimErr := make(chan error, 1)
	go func() {
		shimErr <- RunSpawnShim(ctx, "127.0.0.1:0", req.ProxyUDSPath, ready)
	}()

	var shimAddr string
	select {
	case shimAddr = <-ready:
	case err := <-shimErr:
		// Shim failed before publishing its ready address; abort.
		failHelper("network shim startup: %v", err)
	}

	// Inject the proxy env into the wrapped CLI's env. Replace
	// any pre-existing HTTPS_PROXY/HTTP_PROXY values the runtime
	// may have set on the cmd before deciding the helper was
	// needed (defensive; production processSpawn doesn't set
	// these on Linux when UDS path is in the limits).
	env = removeProxyEnv(env)
	env = append(env,
		"HTTPS_PROXY=http://"+shimAddr,
		"HTTP_PROXY=http://"+shimAddr,
	)

	cmd := exec.Command(req.Program)
	cmd.Args = req.Argv
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			cancel()
			<-shimErr // drain
			os.Exit(exitErr.ExitCode())
		}
		failHelper("run wrapped CLI: %v", err)
	}
	cancel()
	<-shimErr
}

// removeProxyEnv strips any HTTPS_PROXY / HTTP_PROXY keys from
// the env slice. Returns a new slice. Used to clear pre-existing
// proxy hints before the helper sets its own.
func removeProxyEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if hasPrefixCI(kv, "HTTPS_PROXY=") || hasPrefixCI(kv, "HTTP_PROXY=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// hasPrefixCI reports whether s starts with prefix, case-insensitively
// on ASCII letters. The proxy env vars are conventionally upper-case
// but some CLIs honor lowercase variants; treat both alike.
func hasPrefixCI(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		a, b := s[i], prefix[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// failHelper writes a `spawn-helper: <msg>` line to stderr and
// exits the process with code 1. Centralizes the failure shape
// so audit logs and tests can match on it.
func failHelper(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "spawn-helper: "+format+"\n", args...)
	os.Exit(1)
}
