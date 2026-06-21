package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/daemon/spawn"
)

// runDaemon dispatches "start", "stop", "status", and a usage path
// for unknown subcommands. These tests cover the dispatch + stop +
// status paths; "start" is skipped because it would fork-exec the
// real daemon binary and is exercised end-to-end via the umbrella
// issue's acceptance suite.

func TestRunDaemon_NoSubcommand_PrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDaemon(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit for missing subcommand")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr should mention usage; got %q", stderr.String())
	}
}

func TestRunDaemon_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDaemon([]string{"frobnicate"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit for unknown subcommand")
	}
	if !strings.Contains(stderr.String(), "unknown daemon command") {
		t.Errorf("stderr should mention unknown command; got %q", stderr.String())
	}
}

// --- daemon stop ---

func TestRunDaemonStop_NoDaemon_IsIdempotent(t *testing.T) {
	setTestHome(t)
	var stdout, stderr bytes.Buffer
	code := runDaemonStop(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("stdout should report 'not running'; got %q", stdout.String())
	}
}

func TestRunDaemonStop_StaleDaemonJSON_CleansUp(t *testing.T) {
	home := setTestHome(t)
	stateDir := filepath.Join(home, ".aileron")

	// Write daemon.json with a PID that's almost certainly not alive.
	// PID 1 (init) exists on Unix, so use a fabricated high one.
	if err := discovery.Write(stateDir, discovery.Info{
		URL:       "http://127.0.0.1:1",
		PID:       9999999,
		Version:   "stale",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runDaemonStop(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stale daemon.json removed") {
		t.Errorf("stdout should mention stale cleanup; got %q", stdout.String())
	}
	if _, err := discovery.Read(stateDir); err == nil {
		t.Error("daemon.json should be removed after stale cleanup")
	}
}

// --- daemon status ---

func TestRunDaemonStatus_NoDaemon(t *testing.T) {
	setTestHome(t)
	var stdout, stderr bytes.Buffer
	code := runDaemonStatus(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("stdout should report 'not running'; got %q", stdout.String())
	}
}

func TestRunDaemonStatus_RunningDaemon_RendersInfo(t *testing.T) {
	home := setTestHome(t)
	stateDir := filepath.Join(home, ".aileron")

	// Bind a real loopback port so daemon.json carries a reachable URL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	url := "http://" + ln.Addr().String()

	if err := discovery.Write(stateDir, discovery.Info{
		URL:       url,
		PID:       os.Getpid(),
		Version:   "0.0.7-test",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runDaemonStatus(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"running", url, "0.0.7-test"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, out)
		}
	}
}

// --- probeLocalVaultLocked ---

func TestProbeLocalVaultLocked_LockedTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vault/status" {
			t.Errorf("got path %q, want /v1/vault/status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"locked": true, "state": "locked"})
	}))
	defer srv.Close()

	locked, ok := probeLocalVaultLocked(srv.URL, "")
	if !ok {
		t.Fatal("ok should be true on a 200 response")
	}
	if !locked {
		t.Error("locked should be true")
	}
}

func TestProbeLocalVaultLocked_UnlockedFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"locked": false, "state": "unlocked"})
	}))
	defer srv.Close()

	locked, ok := probeLocalVaultLocked(srv.URL, "")
	if !ok {
		t.Fatal("ok should be true on a 200 response")
	}
	if locked {
		t.Error("locked should be false")
	}
}

func TestProbeLocalVaultLocked_NoServer(t *testing.T) {
	// Port 1 is privileged + nothing listens — connect refused / timeout.
	_, ok := probeLocalVaultLocked("http://127.0.0.1:1", "")
	if ok {
		t.Fatal("ok should be false when the daemon isn't reachable")
	}
}

func TestProbeLocalVaultLocked_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, ok := probeLocalVaultLocked(srv.URL, "")
	if ok {
		t.Fatal("ok should be false on non-200")
	}
}

func TestRunDaemonStatus_WithVaultProbe(t *testing.T) {
	home := setTestHome(t)
	stateDir := filepath.Join(home, ".aileron")

	// Real httptest server that serves the local vault status endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/vault/status" {
			_ = json.NewEncoder(w).Encode(map[string]any{"locked": true})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := discovery.Write(stateDir, discovery.Info{
		URL:       srv.URL,
		PID:       os.Getpid(),
		Version:   "v",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runDaemonStatus(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Vault:      locked") {
		t.Errorf("expected vault state line; got:\n%s", stdout.String())
	}
}

// --- bindingAPIBaseURL caching ---

func TestBindingAPIBaseURL_RespectsAILERON_API_URL(t *testing.T) {
	t.Setenv("AILERON_API_URL", "http://override.test:9999/v1/")
	got, err := bindingAPIBaseURL()
	if err != nil {
		t.Fatalf("override should not error: %v", err)
	}
	if got != "http://override.test:9999/v1" {
		t.Errorf("got %q, want trimmed override URL", got)
	}
}

// --- runDaemon dispatch ---

func TestRunDaemon_DispatchToStop(t *testing.T) {
	setTestHome(t)
	var stdout, stderr bytes.Buffer
	code := runDaemon([]string{"stop"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("dispatch should reach runDaemonStop; got %q", stdout.String())
	}
}

// --- runDaemonStart ---

// withSpawnResolve substitutes the spawnResolveFn seam for the
// duration of a test, restoring it on cleanup. Lets us exercise the
// daemon-start path without fork-execing a real binary.
func withSpawnResolve(t *testing.T, fn func(context.Context, spawn.Options) (string, error)) {
	t.Helper()
	orig := spawnResolveFn
	spawnResolveFn = fn
	t.Cleanup(func() { spawnResolveFn = orig })
}

func TestRunDaemonStart_HappyPath_PrintsURL(t *testing.T) {
	setTestHome(t)

	withSpawnResolve(t, func(_ context.Context, opts spawn.Options) (string, error) {
		// Sanity: opts.StateDir resolves to ~/.aileron under the test HOME.
		if !strings.HasSuffix(opts.StateDir, ".aileron") {
			t.Errorf("StateDir %q should end with .aileron", opts.StateDir)
		}
		return "http://127.0.0.1:54321", nil
	})

	// daemonBinaryPath looks for a sibling 'aileron-server' binary;
	// when not found it falls back to PATH lookup, which will fail in
	// test. Place a no-op binary on PATH so the resolution succeeds.
	stageServerOnPath(t)

	var stdout, stderr bytes.Buffer
	code := runDaemonStart(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:54321") {
		t.Errorf("stdout missing URL; got %q", stdout.String())
	}
}

func TestRunDaemonStart_SpawnError_NonZeroExit(t *testing.T) {
	setTestHome(t)

	stageServerOnPath(t)

	withSpawnResolve(t, func(context.Context, spawn.Options) (string, error) {
		return "", errors.New("spawn-failed-on-purpose")
	})

	var stdout, stderr bytes.Buffer
	code := runDaemonStart(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "spawn-failed-on-purpose") {
		t.Errorf("stderr should propagate spawn error; got %q", stderr.String())
	}
}

func TestRunDaemonStart_BinaryNotFound_NonZeroExit(t *testing.T) {
	setTestHome(t)
	// Empty PATH and no sibling 'aileron-server' next to the test
	// binary → daemonBinaryPath fails before spawn is even called.
	t.Setenv("PATH", "")

	var stdout, stderr bytes.Buffer
	code := runDaemonStart(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit when daemon binary is missing; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "locate daemon binary") {
		t.Errorf("stderr should mention binary lookup; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "task build:server") {
		t.Errorf("stderr should suggest building; got %q", stderr.String())
	}
}

// --- spawnResolveOnce (the body that spawnResolveCached wraps) ---

func TestSpawnResolveOnce_HappyPath(t *testing.T) {
	setTestHome(t)
	stageServerOnPath(t)

	withSpawnResolve(t, func(_ context.Context, opts spawn.Options) (string, error) {
		return "http://127.0.0.1:54321", nil
	})

	got, err := spawnResolveOnce()
	if err != nil {
		t.Fatalf("spawnResolveOnce: %v", err)
	}
	// /v1 suffix appended; trailing slash on input would be trimmed.
	if got != "http://127.0.0.1:54321/v1" {
		t.Errorf("got %q, want trailing /v1", got)
	}
}

func TestSpawnResolveOnce_TrimsTrailingSlash(t *testing.T) {
	setTestHome(t)
	stageServerOnPath(t)

	withSpawnResolve(t, func(context.Context, spawn.Options) (string, error) {
		return "http://127.0.0.1:54321/", nil
	})

	got, err := spawnResolveOnce()
	if err != nil {
		t.Fatalf("spawnResolveOnce: %v", err)
	}
	if got != "http://127.0.0.1:54321/v1" {
		t.Errorf("got %q, want trailing slash trimmed before /v1 append", got)
	}
}

func TestSpawnResolveOnce_BinaryMissing_Errors(t *testing.T) {
	setTestHome(t)
	t.Setenv("PATH", "")

	_, err := spawnResolveOnce()
	if err == nil {
		t.Fatal("expected error when daemon binary cannot be located")
	}
	if !strings.Contains(err.Error(), "locate daemon binary") {
		t.Errorf("error should mention binary lookup; got %v", err)
	}
}

func TestSpawnResolveOnce_PropagatesSpawnError(t *testing.T) {
	setTestHome(t)
	stageServerOnPath(t)

	wantErr := errors.New("kaboom")
	withSpawnResolve(t, func(context.Context, spawn.Options) (string, error) {
		return "", wantErr
	})

	_, err := spawnResolveOnce()
	if !errors.Is(err, wantErr) {
		t.Fatalf("got %v, want %v", err, wantErr)
	}
}

// --- runDaemonStop happy path with a real subprocess ---

// runDaemonStop's happy path (SIGTERM a real daemon and poll for its
// cleanup) is covered end-to-end by Test 8 of the umbrella issue
// against the real daemon binary. Trying to mimic that here with a
// fake subprocess is fragile across shells/platforms — coverage
// gain isn't worth the test-flakiness risk.

// --- signalDaemonStop direct contract ---

// TestSignalDaemonStop_NotRunningPID pins the cross-platform contract:
// a not-alive PID returns (notRunning=true, err=nil) so callers can
// clean up stale daemon.json without surfacing an error. POSIX maps
// this to syscall.ESRCH from kill(); Windows maps it to FindProcess
// failing or Kill returning os.ErrProcessDone.
//
// Uses PID 9999999 — above macOS's PID_MAX (99999), well below Linux's
// default kernel.pid_max (4194304) being reused, and unlikely to be a
// live process on a Windows runner.
func TestSignalDaemonStop_NotRunningPID(t *testing.T) {
	notRunning, err := signalDaemonStop(9999999)
	if err != nil {
		t.Fatalf("signalDaemonStop(9999999): unexpected err: %v", err)
	}
	if !notRunning {
		t.Fatal("signalDaemonStop(9999999): notRunning = false, want true (PID should not exist)")
	}
}

// --- defaultStateDir / daemonBinaryPath ---

func TestDefaultStateDir_UnderHome(t *testing.T) {
	home := setTestHome(t)
	got, err := defaultStateDir()
	if err != nil {
		t.Fatalf("defaultStateDir: %v", err)
	}
	want := filepath.Join(home, ".aileron")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDaemonBinaryPath_FindsSibling(t *testing.T) {
	// Drop an "aileron-server" binary next to the running test binary's
	// dir. daemonBinaryPath uses os.Executable() to find the "self" path
	// and looks for a sibling — this test verifies that branch.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	sibling := filepath.Join(filepath.Dir(self), "aileron-server")
	if _, err := os.Stat(sibling); err == nil {
		// Already exists from a previous test run; fine, just verify.
		got, err := daemonBinaryPath()
		if err != nil {
			t.Fatalf("daemonBinaryPath: %v", err)
		}
		if got != sibling {
			t.Errorf("got %q, want sibling %q", got, sibling)
		}
		return
	}
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		// On some CI sandboxes the test binary's directory isn't
		// writable; in that case fall back to the PATH branch and
		// skip the sibling assertion.
		t.Skipf("cannot write sibling for test (sandboxed?): %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(sibling) })

	got, err := daemonBinaryPath()
	if err != nil {
		t.Fatalf("daemonBinaryPath: %v", err)
	}
	if got != sibling {
		t.Errorf("got %q, want sibling %q", got, sibling)
	}
}

func TestDaemonBinaryPath_FallsBackToPATH(t *testing.T) {
	// No sibling 'aileron-server' near the test binary (we don't write
	// one), so resolution should fall back to PATH lookup. Place one
	// on PATH — mirrors the Homebrew install layout. The exeSuffix keeps
	// exec.LookPath able to find it on Windows.
	binDir := t.TempDir()
	server := filepath.Join(binDir, "aileron-server"+exeSuffix)
	if err := os.WriteFile(server, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	// The sibling check happens first; if a sibling exists from a
	// prior test, this test silently exercises the sibling branch
	// instead. That's fine — both branches are valid resolutions.
	got, err := daemonBinaryPath()
	if err != nil {
		t.Fatalf("daemonBinaryPath: %v", err)
	}
	if got == "" {
		t.Fatal("got empty path")
	}
}

func TestDaemonBinaryPath_NotFound(t *testing.T) {
	// Empty PATH and no sibling next to the test binary → not found.
	t.Setenv("PATH", "")
	// Best-effort: remove any sibling 'aileron-server' a previous test
	// left behind. If we can't, the assertion below is a no-op (safe).
	if self, err := os.Executable(); err == nil {
		_ = os.Remove(filepath.Join(filepath.Dir(self), "aileron-server"))
	}
	_, err := daemonBinaryPath()
	if err == nil {
		t.Fatal("expected error when no daemon binary is reachable")
	}
}

func TestRunDaemon_DispatchToStatus(t *testing.T) {
	setTestHome(t)
	var stdout, stderr bytes.Buffer
	code := runDaemon([]string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("dispatch should reach runDaemonStatus; got %q", stdout.String())
	}
}
