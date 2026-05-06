package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
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
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
	stateDir := filepath.Join(os.Getenv("HOME"), ".aileron")

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
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
	stateDir := filepath.Join(os.Getenv("HOME"), ".aileron")

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
		if r.URL.Path != "/v1/vault/local/status" {
			t.Errorf("got path %q, want /v1/vault/local/status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"locked": true, "state": "locked"})
	}))
	defer srv.Close()

	locked, ok := probeLocalVaultLocked(srv.URL)
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

	locked, ok := probeLocalVaultLocked(srv.URL)
	if !ok {
		t.Fatal("ok should be true on a 200 response")
	}
	if locked {
		t.Error("locked should be false")
	}
}

func TestProbeLocalVaultLocked_NoServer(t *testing.T) {
	// Port 1 is privileged + nothing listens — connect refused / timeout.
	_, ok := probeLocalVaultLocked("http://127.0.0.1:1")
	if ok {
		t.Fatal("ok should be false when the daemon isn't reachable")
	}
}

func TestProbeLocalVaultLocked_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, ok := probeLocalVaultLocked(srv.URL)
	if ok {
		t.Fatal("ok should be false on non-200")
	}
}

func TestRunDaemonStatus_WithVaultProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stateDir := filepath.Join(os.Getenv("HOME"), ".aileron")

	// Real httptest server that serves the local vault status endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/vault/local/status" {
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
	got := bindingAPIBaseURL()
	if got != "http://override.test:9999/v1" {
		t.Errorf("got %q, want trimmed override URL", got)
	}
}

// --- runDaemon dispatch ---

func TestRunDaemon_DispatchToStop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := runDaemon([]string{"stop"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("dispatch should reach runDaemonStop; got %q", stdout.String())
	}
}

func TestRunDaemon_DispatchToStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := runDaemon([]string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("dispatch should reach runDaemonStatus; got %q", stdout.String())
	}
}
