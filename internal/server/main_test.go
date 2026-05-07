package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

// TestSelectVault_CloudShapedBypassesLocalCheck is the regression test
// for the integration-suite failure on PR #503: cloud daemons (set
// AILERON_DATABASE_URL) never have a local vault file, so the
// "vault file required" hard error from #492 item 6a must skip them.
// The postgres-backed vault wires up later inside
// [app.NewHandlerWithConfig]; this branch just ensures selectVault
// doesn't refuse to start the daemon before we get there.
func TestSelectVault_CloudShapedBypassesLocalCheck(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "postgres://test")
	dir := t.TempDir()
	cfg, err := selectVault(slog.Default(), filepath.Join(dir, "secrets.json"), false, refusePrompter(t), io.Discard)
	if err != nil {
		t.Fatalf("selectVault: %v", err)
	}
	if cfg.Vault != nil {
		t.Errorf("cfg.Vault = %v, want nil for cloud path", cfg.Vault)
	}
	if cfg.LocalVaultPath != "" {
		t.Errorf("cfg.LocalVaultPath = %q, want empty for cloud path", cfg.LocalVaultPath)
	}
}

// TestSelectVault_NoFileIsHardError covers #492 item 6a: with the dev
// fallback removed, a missing vault file is no longer silently OK.
// selectVault must return an error pointing the operator at
// `aileron vault init` so we never silently lose data.
func TestSelectVault_NoFileIsHardError(t *testing.T) {
	t.Setenv("AILERON_DATABASE_URL", "")
	dir := t.TempDir()
	_, err := selectVault(slog.Default(), filepath.Join(dir, "secrets.json"), true, refusePrompter(t), io.Discard)
	if err == nil {
		t.Fatal("selectVault: expected error for missing vault, got nil")
	}
	if !strings.Contains(err.Error(), "aileron vault init") {
		t.Errorf("error = %q; want a hint pointing at `aileron vault init`", err)
	}
}

// TestSelectVault_NonTTYStartsLocked covers #492 item 6b: the auto-spawn
// case (no controlling tty, vault file present) returns a Config with
// LocalVaultPath set so the daemon wraps its (empty) inner vault in a
// LockableVault. /v1/vault/unlock then has somewhere to swap the
// unlocked vault into. Pre-#492 this returned an empty Config, daemons
// fell through to a memory dev vault, and /v1/vault/unlock was dead
// code in production.
func TestSelectVault_NonTTYStartsLocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	if _, err := vault.Init(path, "passphrase"); err != nil {
		t.Fatalf("vault.Init: %v", err)
	}

	cfg, err := selectVault(slog.Default(), path, false /* isTTY */, refusePrompter(t), io.Discard)
	if err != nil {
		t.Fatalf("selectVault: %v", err)
	}
	if cfg.Vault != nil {
		t.Errorf("cfg.Vault = %v, want nil (no-TTY path should not pre-unlock)", cfg.Vault)
	}
	if cfg.LocalVaultPath != path {
		t.Errorf("cfg.LocalVaultPath = %q, want %q (so app.NewHandlerWithConfig wires LockableVault)", cfg.LocalVaultPath, path)
	}
}

// TestSelectVault_PresentFileAndTTYUnlocks covers the interactive
// startup path (operator-typed `aileron daemon start`): vault file
// present + isTTY true → unlock interactively, set both Vault and
// LocalVaultPath so the LockableVault wraps the pre-unlocked inner.
func TestSelectVault_PresentFileAndTTYUnlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	const passphrase = "correct-horse-battery-staple"
	if _, err := vault.Init(path, passphrase); err != nil {
		t.Fatalf("vault.Init: %v", err)
	}

	cfg, err := selectVault(slog.Default(), path, true, scriptedPrompter(passphrase), io.Discard)
	if err != nil {
		t.Fatalf("selectVault: %v", err)
	}
	if cfg.Vault == nil {
		t.Fatal("cfg.Vault is nil; expected the persistent vault to be unlocked and used")
	}
	if cfg.LocalVaultPath != path {
		t.Errorf("cfg.LocalVaultPath = %q, want %q (LockableVault wires regardless of when unlock happens)", cfg.LocalVaultPath, path)
	}
}

// TestSelectVault_CorruptFileFailsStartup ensures the server start
// fails loudly when a vault file exists but is unparseable, rather
// than silently falling back to the in-memory dev vault. Falling
// back would mask a security signal (the file may have been
// tampered with) AND would orphan whatever bindings the user thought
// they had — both worse outcomes than refusing to start.
func TestSelectVault_CorruptFileFailsStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("vault.Init: %v", err)
	}
	tamper(t, path)

	_, err := selectVault(slog.Default(), path, true, refusePrompter(t), io.Discard)
	if err == nil {
		t.Fatal("selectVault: expected error for corrupt vault file, got nil")
	}
}

// seedVault creates a passphrase-protected vault file at path, so the
// daemon's selectVault has something to start with. After #492 item 6a
// the dev fallback is gone — every test that boots the daemon must
// supply a real vault file. The passphrase is irrelevant when isTTY
// is false (the daemon starts vault-locked); when isTTY is true the
// caller is responsible for the prompter handshake.
func seedVault(t *testing.T, path string) {
	t.Helper()
	if _, err := vault.Init(path, "test-passphrase"); err != nil {
		t.Fatalf("seed vault at %s: %v", path, err)
	}
}

// TestRun_NoTTYDaemonHonorsVaultUnlock is the integration test for
// #492 item 6c: a daemon spawned production-shape (no controlling TTY,
// vault file at the canonical path) starts vault-locked, and an HTTP
// POST to /v1/vault/unlock with the right passphrase actually unlocks
// it. Pre-#492 this path was dead code in production: selectVault
// never set LocalVaultPath, so the LockableVault wrapper was never
// installed, and /v1/vault/unlock had no inner vault to swap into.
//
// Asserts the full chain: selectVault → app.NewHandlerWithConfig
// (LockableVault wrap) → HTTP request → vault.Unlock → /v1/vault/status
// flips from locked to unlocked.
func TestRun_NoTTYDaemonHonorsVaultUnlock(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	const passphrase = "the-passphrase"
	if _, err := vault.Init(vaultPath, passphrase); err != nil {
		t.Fatalf("seed vault: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := options{
		BindAddr:  "127.0.0.1:0",
		StateDir:  stateDir,
		VaultPath: vaultPath,
		IsTTY:     false, // production-shape: no controlling TTY
		Stderr:    io.Discard,
	}

	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, log, opts) }()
	info := waitForDaemon(t, stateDir, 3*time.Second)

	// Pre-unlock the vault should report locked. (Empty cfg.Vault +
	// LocalVaultPath set → LockableVault wrap → vaultLocked = true.)
	statusResp, err := http.Get(info.URL + "/v1/vault/status")
	if err != nil {
		t.Fatalf("GET /v1/vault/status: %v", err)
	}
	defer statusResp.Body.Close()
	statusBody, _ := io.ReadAll(statusResp.Body)
	if !strings.Contains(string(statusBody), `"locked":true`) {
		t.Errorf("status before unlock = %s, want locked:true", string(statusBody))
	}

	// Drive the unlock with the right passphrase.
	unlockBody := strings.NewReader(`{"passphrase":"` + passphrase + `"}`)
	unlockResp, err := http.Post(info.URL+"/v1/vault/unlock", "application/json", unlockBody)
	if err != nil {
		t.Fatalf("POST /v1/vault/unlock: %v", err)
	}
	defer unlockResp.Body.Close()
	if unlockResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(unlockResp.Body)
		t.Fatalf("unlock status = %d, want 200; body = %s", unlockResp.StatusCode, string(body))
	}

	// Status after unlock: locked:false.
	statusResp2, err := http.Get(info.URL + "/v1/vault/status")
	if err != nil {
		t.Fatalf("GET /v1/vault/status (post-unlock): %v", err)
	}
	defer statusResp2.Body.Close()
	statusBody2, _ := io.ReadAll(statusResp2.Body)
	if !strings.Contains(string(statusBody2), `"locked":false`) {
		t.Errorf("status after unlock = %s, want locked:false", string(statusBody2))
	}

	cancel()
	<-runErr
}

// TestRun_NoTTYDaemonRejectsWrongPassphrase: the same production-shape
// path with a wrong passphrase returns 401 and leaves the vault locked.
func TestRun_NoTTYDaemonRejectsWrongPassphrase(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(vaultPath, "right"); err != nil {
		t.Fatalf("seed vault: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := options{
		BindAddr:  "127.0.0.1:0",
		StateDir:  stateDir,
		VaultPath: vaultPath,
		IsTTY:     false,
		Stderr:    io.Discard,
	}
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, log, opts) }()
	info := waitForDaemon(t, stateDir, 3*time.Second)

	resp, err := http.Post(info.URL+"/v1/vault/unlock", "application/json",
		strings.NewReader(`{"passphrase":"wrong"}`))
	if err != nil {
		t.Fatalf("POST /v1/vault/unlock: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for wrong passphrase", resp.StatusCode)
	}

	cancel()
	<-runErr
}

// TestRun_PublishesDiscoveryAndCleansUp covers the daemon happy path:
// run binds an ephemeral port, writes daemon.json with the URL/PID,
// the URL is reachable for HTTP, and on context cancellation the
// discovery files are removed and run returns cleanly.
func TestRun_PublishesDiscoveryAndCleansUp(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	seedVault(t, vaultPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := options{
		BindAddr:  "127.0.0.1:0",
		StateDir:  stateDir,
		VaultPath: vaultPath,
		IsTTY:     false,
		Stderr:    io.Discard,
	}

	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, log, opts) }()

	info := waitForDaemon(t, stateDir, 3*time.Second)

	if !strings.HasPrefix(info.URL, "http://127.0.0.1:") {
		t.Errorf("URL = %q, want http://127.0.0.1:NNNN", info.URL)
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", info.PID, os.Getpid())
	}
	if info.Version == "" {
		t.Error("Version is empty; expected build-injected or default 'dev'")
	}

	// URL is reachable: GET an endpoint that exists without auth setup.
	resp, err := http.Get(info.URL + "/v1/vault/status")
	if err != nil {
		t.Fatalf("GET %s: %v", info.URL, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusInternalServerError {
		t.Errorf("status = %d, expected non-5xx from a wired daemon", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within timeout after cancel")
	}

	for _, name := range []string{discovery.InfoFile, discovery.PIDFile} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after shutdown (err: %v)", name, err)
		}
	}
}

// TestRun_RefusesWhenAnotherDaemonHoldsLock asserts the singleton
// invariant: a second daemon refuses to start while the first holds
// daemon.lock. This is the core safety property — without it two
// daemons could race to write daemon.json and clients would see only
// the latest, while the other process keeps running invisibly.
func TestRun_RefusesWhenAnotherDaemonHoldsLock(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	seedVault(t, vaultPath)

	release, err := discovery.Lock(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("manual Lock: %v", err)
	}
	defer release()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := options{
		BindAddr:  "127.0.0.1:0",
		StateDir:  stateDir,
		VaultPath: vaultPath,
		IsTTY:     false,
		Stderr:    io.Discard,
	}

	err = run(context.Background(), log, opts)
	if err == nil {
		t.Fatal("run should fail when daemon.lock is held by another process")
	}
}

// TestRun_ListenFailureCleansUp verifies that when listen fails (e.g.
// invalid bind address), no stale discovery files are left behind. The
// daemon either succeeds and publishes, or fails and publishes nothing.
func TestRun_ListenFailureCleansUp(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	seedVault(t, vaultPath)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := options{
		BindAddr:  "not-a-valid-address",
		StateDir:  stateDir,
		VaultPath: vaultPath,
		IsTTY:     false,
		Stderr:    io.Discard,
	}

	err := run(context.Background(), log, opts)
	if err == nil {
		t.Fatal("run should fail on invalid bind address")
	}
	for _, name := range []string{discovery.InfoFile, discovery.PIDFile} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after listen failure (err: %v)", name, err)
		}
	}
}

// TestRun_BindAddrEmptyMeansEphemeral verifies the local-daemon
// default: empty BindAddr resolves to 127.0.0.1:0 (ephemeral port,
// loopback only). Cloud overrides via --bind explicitly.
func TestRun_BindAddrEmptyMeansEphemeral(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	seedVault(t, vaultPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := options{
		BindAddr:  "",
		StateDir:  stateDir,
		VaultPath: vaultPath,
		IsTTY:     false,
		Stderr:    io.Discard,
	}

	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, log, opts) }()

	info := waitForDaemon(t, stateDir, 3*time.Second)
	if !strings.HasPrefix(info.URL, "http://127.0.0.1:") {
		t.Errorf("URL = %q, want loopback ephemeral", info.URL)
	}

	cancel()
	<-runErr
}

// TestRun_VaultErrorPropagates exercises the selectVault → run error
// path: when the vault file is corrupt, run should return the vault
// error and write no discovery files (the lock is acquired and
// released cleanly via defer).
func TestRun_RemoveWarnDoesNotFailShutdown(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; chmod-based access denial is bypassed")
	}
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	seedVault(t, vaultPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := options{
		BindAddr:  "127.0.0.1:0",
		StateDir:  stateDir,
		VaultPath: vaultPath,
		IsTTY:     false,
		Stderr:    io.Discard,
	}

	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, log, opts) }()
	waitForDaemon(t, stateDir, 3*time.Second)

	// Make stateDir read-only so discovery.Remove fails during shutdown.
	// run should log a warning but still return cleanly.
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run should swallow Remove failure as a warning: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within timeout")
	}
}

func TestRun_VaultErrorPropagates(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(vaultPath, "passphrase"); err != nil {
		t.Fatalf("vault.Init: %v", err)
	}
	tamper(t, vaultPath)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := options{
		BindAddr:  "127.0.0.1:0",
		StateDir:  stateDir,
		VaultPath: vaultPath,
		IsTTY:     true,
		Stderr:    io.Discard,
	}

	err := run(context.Background(), log, opts)
	if err == nil {
		t.Fatal("run should fail when vault is corrupt")
	}
	for _, name := range []string{discovery.InfoFile, discovery.PIDFile} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should not exist after vault error", name)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"unknown": slog.LevelInfo,
	}
	for env, want := range cases {
		t.Run(env, func(t *testing.T) {
			if got := parseLogLevel(env); got != want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", env, got, want)
			}
		})
	}
}

func TestNewLogger(t *testing.T) {
	if got := newLogger("debug"); got == nil {
		t.Fatal("newLogger returned nil")
	}
}

func TestStartDaemonHappyPath(t *testing.T) {
	// HOME override redirects defaultStateDir at ~/.aileron into a temp
	// dir so the test does not write to the user's actual home.
	t.Setenv("HOME", t.TempDir())

	// Per #492 item 6a, the daemon refuses to start without a vault file.
	// Seed one at the canonical path so startDaemon's selectVault
	// proceeds past the StateMissing check.
	stateDir := filepath.Join(os.Getenv("HOME"), ".aileron")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	seedVault(t, launch.DefaultVaultPath())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() { runErr <- startDaemon(ctx, "127.0.0.1:0", log) }()

	waitForDaemon(t, stateDir, 3*time.Second)

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("startDaemon: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startDaemon did not return within timeout")
	}
}

func TestStartDaemonReturnsErrorFromRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".aileron"), 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	seedVault(t, launch.DefaultVaultPath())

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := startDaemon(context.Background(), "not-a-valid-address", log)
	if err == nil {
		t.Fatal("startDaemon should propagate the listen error")
	}
}

func TestDefaultStateDir(t *testing.T) {
	dir, err := defaultStateDir()
	if err != nil {
		t.Fatalf("defaultStateDir: %v", err)
	}
	if !strings.HasSuffix(dir, ".aileron") {
		t.Fatalf("dir = %q, want suffix .aileron", dir)
	}
}

// waitForDaemon polls daemon.json with a short interval until it
// appears or the deadline expires. Returns the parsed Info on success;
// fails the test on timeout.
func waitForDaemon(t *testing.T, stateDir string, timeout time.Duration) discovery.Info {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := discovery.Read(stateDir)
		if err == nil {
			return info
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon.json did not appear within %s", timeout)
	return discovery.Info{}
}

// scriptedPrompter returns a PassphrasePrompter that always answers
// with the given passphrase. Used to drive the unlock path in tests
// without touching /dev/tty.
func scriptedPrompter(passphrase string) launch.PassphrasePrompter {
	return func(_ string, _ io.Writer) (string, error) {
		return passphrase, nil
	}
}

// refusePrompter returns a PassphrasePrompter that fails the test if
// it is called. Used in cases where the path-under-test should never
// reach the prompt step — calling the prompter is the test failure.
func refusePrompter(t *testing.T) launch.PassphrasePrompter {
	t.Helper()
	return func(_ string, _ io.Writer) (string, error) {
		t.Errorf("prompter was called; expected to short-circuit before prompting")
		return "", nil
	}
}

// tamper corrupts the vault file at `path` by overwriting its first
// byte. Any subsequent vault.Unlock call returns vault.ErrVaultTampered.
func tamper(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open vault for tampering: %v", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte{0xff}); err != nil {
		t.Fatalf("tamper write: %v", err)
	}
}
