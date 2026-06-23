package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
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

func TestShouldProtectLocalDaemon(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{addr: "127.0.0.1:8721", want: true},
		{addr: "[::1]:8721", want: true},
		{addr: "localhost:8721", want: true},
		{addr: "0.0.0.0:8080", want: false},
		{addr: "[::]:8080", want: false},
		{addr: "10.0.0.5:8080", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			got := shouldProtectLocalDaemon(&net.TCPAddr{IP: parseTestIP(t, tc.addr), Port: parseTestPort(t, tc.addr)})
			if strings.HasPrefix(tc.addr, "localhost:") {
				got = shouldProtectLocalDaemon(stringAddr(tc.addr))
			}
			if got != tc.want {
				t.Fatalf("shouldProtectLocalDaemon(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

type stringAddr string

func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return string(a) }

func parseTestIP(t *testing.T, addr string) net.IP {
	t.Helper()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	return net.ParseIP(host)
}

func parseTestPort(t *testing.T, addr string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	if port == "8721" {
		return 8721
	}
	return 8080
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
	statusResp := doDaemonRequest(t, http.MethodGet, info.URL+"/v1/vault/status", info.Token, "")
	defer statusResp.Body.Close()
	statusBody, _ := io.ReadAll(statusResp.Body)
	if !strings.Contains(string(statusBody), `"locked":true`) {
		t.Errorf("status before unlock = %s, want locked:true", string(statusBody))
	}

	// Drive the unlock with the right passphrase.
	unlockResp := doDaemonRequest(t, http.MethodPost, info.URL+"/v1/vault/unlock", info.Token, `{"passphrase":"`+passphrase+`"}`)
	defer unlockResp.Body.Close()
	if unlockResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(unlockResp.Body)
		t.Fatalf("unlock status = %d, want 200; body = %s", unlockResp.StatusCode, string(body))
	}

	// Status after unlock: locked:false.
	statusResp2 := doDaemonRequest(t, http.MethodGet, info.URL+"/v1/vault/status", info.Token, "")
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

	resp := doDaemonRequest(t, http.MethodPost, info.URL+"/v1/vault/unlock", info.Token, `{"passphrase":"wrong"}`)
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
	resp := doDaemonRequest(t, http.MethodGet, info.URL+"/v1/vault/status", info.Token, "")
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

// TestRun_WebappURL_DefaultsToBoundURL pins the contract that the
// daemon's own bound URL becomes the default WebappURL surfaced in
// agent-facing approval messages. Without this default, an
// approval-gated action's review URL is rendered as "Visit  to
// approve" — empty between "Visit" and "to" — because
// `buildPendingApprovalResponse` interpolates `s.webappURL` directly.
// The daemon serves the webapp itself (see internal/app/webapp_embed.go),
// so its own URL is the right out-of-the-box answer for the common
// local case.
//
// `AILERON_WEBAPP_URL` (covered by [TestRun_WebappURL_EnvOverrides])
// remains an operator override for reverse-proxied deployments.
func TestRun_WebappURL_DefaultsToBoundURL(t *testing.T) {
	t.Setenv("AILERON_WEBAPP_URL", "")
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

	got := fetchStatusGatewayURL(t, info.URL, info.Token)
	if got != info.URL {
		t.Errorf("gateway_url = %q, want %q (the daemon's own bound URL)", got, info.URL)
	}

	cancel()
	<-runErr
}

// TestRun_WebappURL_EnvOverrides asserts that `AILERON_WEBAPP_URL`,
// when set, takes precedence over the bound-URL default. This is the
// escape hatch for reverse-proxied or `--bind 0.0.0.0:...` deployments
// where the bound URL is not what users see.
func TestRun_WebappURL_EnvOverrides(t *testing.T) {
	const override = "https://aileron.example.com"
	t.Setenv("AILERON_WEBAPP_URL", override)
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

	got := fetchStatusGatewayURL(t, info.URL, info.Token)
	if got != override {
		t.Errorf("gateway_url = %q, want %q (env override wins over bound URL)", got, override)
	}

	cancel()
	<-runErr
}

// fetchStatusGatewayURL hits the daemon's `/v1/status` endpoint and
// returns the `gateway_url` field (which surfaces `s.webappURL` per
// handlers_status.go). Empty string when the field is absent.
func fetchStatusGatewayURL(t *testing.T, daemonURL, token string) string {
	t.Helper()
	resp := doDaemonRequest(t, http.MethodGet, daemonURL+"/v1/status", token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/status status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		GatewayURL string `json:"gateway_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return body.GatewayURL
}

func doDaemonRequest(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
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

// TestRun_LockInodeMismatchTriggersShutdown regression-pins #528
// finding 3b: when daemon.lock is unlinked-and-recreated while a daemon
// is running (e.g. external `rm -rf ~/.aileron` followed by another
// daemon spawning at the same path), the running daemon's flock is
// orphaned on a now-unreferenced inode. Without the inode-watcher the
// daemon would survive indefinitely alongside the new owner of the
// path, breaking the singleton invariant.
//
// The watcher must:
//   - detect the inode change,
//   - exit run() cleanly (no error), and
//   - leave daemon.json/daemon.pid intact (those belong to the daemon
//     that took over; removing them would erase its advertisement).
func TestRun_LockInodeMismatchTriggersShutdown(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	seedVault(t, vaultPath)

	// Shorten the watch interval so the test doesn't wait the
	// production 5s. Restored on cleanup.
	prev := lockInodeWatchInterval
	lockInodeWatchInterval = 50 * time.Millisecond
	t.Cleanup(func() { lockInodeWatchInterval = prev })

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
	if info.URL == "" {
		t.Fatal("daemon never published its URL")
	}

	// Simulate external "rm -rf <stateDir>/daemon.lock + new daemon
	// recreates it." The daemon's flock-fd retains the original inode;
	// our recreated file gets a fresh one.
	lockPath := filepath.Join(stateDir, discovery.LockFile)
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove lock: %v", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("recreate lock: %v", err)
	}
	_ = f.Close()

	// Wait for the watcher to detect the mismatch and run() to return.
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run returned error: %v (want nil — inode-mismatch is a clean shutdown)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after lock-file inode change; the watcher is not detecting the mismatch")
	}

	// daemon.json + daemon.pid must still exist: removing them would
	// erase the surviving daemon's advertisement.
	for _, name := range []string{discovery.InfoFile, discovery.PIDFile} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); err != nil {
			t.Errorf("%s missing after inode-mismatch shutdown (err: %v); discovery files should be left for the daemon that took over", name, err)
		}
	}
}

// TestWatchLockInode_FileDisappeared covers the ENOENT branch: an
// unlinked-but-not-recreated daemon.lock is the same shutdown signal
// as a recreated one. Exercised as a unit test against watchLockInode
// directly so we don't need to drive a full daemon.
func TestWatchLockInode_FileDisappeared(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, discovery.LockFile)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("create lock: %v", err)
	}
	original, err := discovery.LockFileInode(dir)
	if err != nil {
		t.Fatalf("LockFileInode: %v", err)
	}

	prev := lockInodeWatchInterval
	lockInodeWatchInterval = 25 * time.Millisecond
	t.Cleanup(func() { lockInodeWatchInterval = prev })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trigger := make(chan struct{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go watchLockInode(ctx, log, dir, original, trigger)

	// Unlink the lock file; do NOT recreate. The watcher should treat
	// disappearance as a mismatch signal.
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove lock: %v", err)
	}

	select {
	case <-trigger:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not signal on file-disappeared (ENOENT)")
	}
}

// TestWatchLockInode_StopsOnContextCancel pins the watcher's clean-stop
// behavior — the goroutine must exit when its context is canceled,
// otherwise it leaks for the daemon's lifetime.
func TestWatchLockInode_StopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, discovery.LockFile), nil, 0o600); err != nil {
		t.Fatalf("create lock: %v", err)
	}
	original, err := discovery.LockFileInode(dir)
	if err != nil {
		t.Fatalf("LockFileInode: %v", err)
	}

	prev := lockInodeWatchInterval
	lockInodeWatchInterval = 25 * time.Millisecond
	t.Cleanup(func() { lockInodeWatchInterval = prev })

	ctx, cancel := context.WithCancel(context.Background())
	trigger := make(chan struct{})
	done := make(chan struct{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		watchLockInode(ctx, log, dir, original, trigger)
		close(done)
	}()

	// Let the watcher run a couple of ticks, then cancel.
	time.Sleep(75 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not return after context cancel")
	}

	// Trigger must not have fired — context cancel is a clean stop, not
	// a mismatch signal.
	select {
	case <-trigger:
		t.Fatal("trigger fired on context cancel; should only fire on inode change")
	default:
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
	go func() { runErr <- startDaemon(ctx, "127.0.0.1:0", "", log) }()

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
	err := startDaemon(context.Background(), "not-a-valid-address", "", log)
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

// TestIsLoopbackBind covers the bind-address classifier the
// --dev-seed-approvals guard depends on. Each case is one row in the
// guard's truth table; rejecting parse failures is the conservative
// default per the function's contract.
func TestIsLoopbackBind(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:0", true},
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{":8080", true}, // empty host = bind on all loopback interfaces in tests
		{"[::1]:0", true},
		{"0.0.0.0:8080", false},
		{"192.168.1.5:0", false},
		{"example.com:80", false}, // hostname; doesn't resolve to loopback
		{"not-a-valid-address", false},
	}
	for _, c := range cases {
		t.Run(c.addr, func(t *testing.T) {
			if got := isLoopbackBind(c.addr); got != c.want {
				t.Errorf("isLoopbackBind(%q) = %v, want %v", c.addr, got, c.want)
			}
		})
	}
}

// TestRun_SeedApprovals_PopulatesQueue verifies the --dev-seed-approvals
// path end-to-end against a real daemon: the JSON fixture is loaded,
// each entry is registered on the action-approval queue, and the
// queue's pending entries surface via the public list endpoint.
//
// This is the contract Playwright's tier-2 integration spec depends
// on. Without it, the spec would have nothing to list/approve.
func TestRun_SeedApprovals_PopulatesQueue(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	seedVault(t, vaultPath)
	seedFile := filepath.Join(t.TempDir(), "approvals.json")
	body := `[
		{"kind":"action","action_name":"send-email","connector_fqn":"github://x/y","args":{"to":"a@b.com"}},
		{"kind":"comms_send","action_name":"send_message","args":{"service":"slack","channel":"#dev","body":"deploy"}}
	]`
	if err := os.WriteFile(seedFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := options{
		BindAddr:          "127.0.0.1:0",
		StateDir:          stateDir,
		VaultPath:         vaultPath,
		IsTTY:             false,
		Stderr:            io.Discard,
		SeedApprovalsPath: seedFile,
	}
	runErr := make(chan error, 1)
	go func() { runErr <- run(ctx, log, opts) }()
	info := waitForDaemon(t, stateDir, 3*time.Second)
	defer func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(5 * time.Second):
			t.Fatal("run did not return within timeout")
		}
	}()

	resp := doDaemonRequest(t, http.MethodGet, info.URL+"/v1/action-approvals", info.Token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var listResp struct {
		Items []struct {
			ID         string `json:"id"`
			Kind       string `json:"kind"`
			ActionName string `json:"action_name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Items) != 2 {
		t.Fatalf("len(items)=%d want 2 — seed entries should be surfaced via /v1/action-approvals", len(listResp.Items))
	}
	kinds := map[string]bool{}
	for _, it := range listResp.Items {
		kinds[it.Kind] = true
		if it.ID == "" || it.ActionName == "" {
			t.Errorf("entry missing id or action_name: %+v", it)
		}
	}
	if !kinds["action"] || !kinds["comms_send"] {
		t.Errorf("kinds=%v want action and comms_send", kinds)
	}
}

// TestRun_SeedApprovals_RejectsNonLoopbackBind verifies the guard
// short-circuits BEFORE binding when --dev-seed-approvals is paired
// with a public bind. Failing fast keeps a careless invocation from
// briefly exposing the seeded queue to the network.
func TestRun_SeedApprovals_RejectsNonLoopbackBind(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	seedVault(t, vaultPath)
	seedFile := filepath.Join(t.TempDir(), "approvals.json")
	if err := os.WriteFile(seedFile, []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), log, options{
		BindAddr:          "0.0.0.0:0",
		StateDir:          stateDir,
		VaultPath:         vaultPath,
		IsTTY:             false,
		Stderr:            io.Discard,
		SeedApprovalsPath: seedFile,
	})
	if err == nil {
		t.Fatal("run should reject --dev-seed-approvals on a non-loopback bind")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("err=%v want mention of loopback", err)
	}
}

// TestRun_SeedApprovals_PropagatesLoadErrors covers the failure mode
// where the seed file is malformed. The startup error should be
// surfaced rather than swallowed — Playwright's job runner needs to
// fail fast on a broken fixture.
func TestRun_SeedApprovals_PropagatesLoadErrors(t *testing.T) {
	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	seedVault(t, vaultPath)
	seedFile := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(seedFile, []byte(`{not json at all`), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), log, options{
		BindAddr:          "127.0.0.1:0",
		StateDir:          stateDir,
		VaultPath:         vaultPath,
		IsTTY:             false,
		Stderr:            io.Discard,
		SeedApprovalsPath: seedFile,
	})
	if err == nil {
		t.Fatal("run should surface seed-load errors")
	}
	if !strings.Contains(err.Error(), "load seed approvals") {
		t.Errorf("err=%v want 'load seed approvals' prefix", err)
	}
}

// canBindLoopbackAlias reports whether the host can bind a secondary
// loopback address (127.0.0.2). Linux treats the whole 127.0.0.0/8 as
// loopback; macOS only configures 127.0.0.1 by default. The bridge-bind
// regression test needs a second bindable IP to stand in for the Docker
// bridge-gateway IP, so it skips where one isn't available.
func canBindLoopbackAlias(t *testing.T, addr string) bool {
	t.Helper()
	l, err := net.Listen("tcp", addr+":0")
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// TestRun_BridgeListener_ReachableAndTokenProtected is the regression
// test for issue #1471: on Linux+Docker the daemon must additionally
// bind the Docker bridge-gateway IP at the SAME ephemeral port as the
// loopback listener, serving the same token-protected handler, so a
// sandbox container reaching the daemon via host.docker.internal can
// actually connect.
//
// It stubs the OS + Docker seams so no real Docker daemon is needed and
// pins the bridge IP to a secondary loopback alias (127.0.0.2). It then
// asserts BOTH halves of the operator's answer:
//   - reachability: an authenticated request on the bridge IP:port
//     succeeds (the SYN no longer gets a kernel RST), and
//   - auth: an unauthenticated request on that same wider listener is
//     rejected (the bridge listener is NOT shipped UNAUTHENTICATED).
func TestRun_BridgeListener_ReachableAndTokenProtected(t *testing.T) {
	const bridgeIP = "127.0.0.2"
	if !canBindLoopbackAlias(t, bridgeIP) {
		t.Skipf("cannot bind %s on this host (no loopback alias); bridge bind is exercised on Linux/CI", bridgeIP)
	}

	// Force the Linux+Docker code path and pin the derived bridge IP to
	// a bindable loopback alias, independent of any real Docker network.
	withStubbedHostGOOS(t, "linux")
	prevDockerOnPath := dockerOnPath
	dockerOnPath = func() bool { return true }
	t.Cleanup(func() { dockerOnPath = prevDockerOnPath })
	withStubbedDerivation(t,
		func(context.Context) ([]byte, error) {
			return []byte(`[{"IPAM":{"Config":[{"Gateway":"` + bridgeIP + `"}]}}]`), nil
		},
		func(context.Context) (string, error) { return bridgeIP, nil },
	)

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

	// A token must have been minted: the wider bridge listener must be
	// authenticated, so the discovery token is non-empty.
	if info.Token == "" {
		t.Fatal("daemon token is empty; the bridge listener would ship UNAUTHENTICATED")
	}

	// The advertised URL stays the loopback URL: the launcher rewrites
	// loopback→host.docker.internal at containerURLForRuntime, so
	// advertisement is unchanged by the bridge bind.
	if !strings.HasPrefix(info.URL, "http://127.0.0.1:") {
		t.Errorf("advertised URL = %q, want loopback (bridge bind must not change advertisement)", info.URL)
	}

	// Derive the bridge endpoint at the SAME port the loopback listener
	// advertised — that is the port host.docker.internal:<port> resolves
	// to inside the container.
	_, port, err := net.SplitHostPort(strings.TrimPrefix(info.URL, "http://"))
	if err != nil {
		t.Fatalf("parse advertised URL %q: %v", info.URL, err)
	}
	bridgeURL := "http://" + net.JoinHostPort(bridgeIP, port)

	// Reachability: an authenticated request on the bridge IP succeeds.
	authed := doDaemonRequest(t, http.MethodGet, bridgeURL+"/v1/vault/status", info.Token, "")
	defer authed.Body.Close()
	if authed.StatusCode == http.StatusUnauthorized {
		t.Fatalf("authed request on bridge listener got 401; the daemon token should be accepted there")
	}
	if authed.StatusCode >= 500 {
		t.Fatalf("authed request on bridge listener got %d; expected a served response", authed.StatusCode)
	}

	// Auth: an unauthenticated request on the SAME wider listener is
	// rejected — the bridge listener is token-protected.
	unauth := doDaemonRequest(t, http.MethodGet, bridgeURL+"/v1/vault/status", "", "")
	defer unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth request on bridge listener = %d, want 401; the wider listener must stay token-protected", unauth.StatusCode)
	}

	cancel()
	<-runErr
}

// TestRun_BridgeDerivationFailureIsHardError pins issue #1471's
// fail-loud contract: when the bridge-gateway IP cannot be derived on a
// Linux+Docker host, run() returns an error and binds nothing wider,
// rather than silently falling back to 0.0.0.0 or loopback-only. No
// discovery files are left behind.
func TestRun_BridgeDerivationFailureIsHardError(t *testing.T) {
	withStubbedHostGOOS(t, "linux")
	prevDockerOnPath := dockerOnPath
	dockerOnPath = func() bool { return true }
	t.Cleanup(func() { dockerOnPath = prevDockerOnPath })
	withStubbedDerivation(t,
		func(context.Context) ([]byte, error) { return nil, errors.New("docker down") },
		func(context.Context) (string, error) { return "", errors.New("no docker0") },
	)

	stateDir := t.TempDir()
	vaultPath := filepath.Join(t.TempDir(), "secrets.json")
	seedVault(t, vaultPath)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), log, options{
		BindAddr:  "127.0.0.1:0",
		StateDir:  stateDir,
		VaultPath: vaultPath,
		IsTTY:     false,
		Stderr:    io.Discard,
	})
	if err == nil {
		t.Fatal("run should hard-error when the bridge-gateway IP cannot be derived")
	}
	if !strings.Contains(err.Error(), "bridge-gateway") {
		t.Errorf("err = %v; want a bridge-gateway derivation error", err)
	}
	for _, name := range []string{discovery.InfoFile, discovery.PIDFile} {
		if _, statErr := os.Stat(filepath.Join(stateDir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s exists after derivation failure; nothing should have been published", name)
		}
	}
}

// TestShouldProtectLocalDaemon_BridgePairMintsToken pins that when a
// bridge listener is bound alongside the loopback primary, the daemon
// still mints and requires a token. Exercised via run() in
// TestRun_BridgeListener_ReachableAndTokenProtected; this case documents
// the unit-level contract that loopback primaries are always protected,
// which the bridge pair relies on.
func TestShouldProtectLocalDaemon_BridgePairMintsToken(t *testing.T) {
	// The loopback primary is always protected; the bridge listener
	// shares that protection because both serve one token-gated handler.
	if !shouldProtectLocalDaemon(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}) {
		t.Fatal("loopback primary must be protected; the bridge pair depends on it")
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
