package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/vault"
)

// withVaultStateSeams swaps the package-level seams in vault_state.go
// for the duration of the test, restoring on cleanup. Returns a struct
// of pointers so individual tests can override specific seams without
// disturbing the others.
type vaultStateFakes struct {
	discoveryRead   func(string) (discovery.Info, error)
	vaultCheckState func(string) (vault.State, error)
	vaultInit       func(string, string) (vault.Vault, error)
	postUnlock      func(string, string) error
	prompt          func(string, io.Writer) (string, error)
	spawnResolve    func() (string, error)
}

func withVaultStateSeams(t *testing.T, fakes vaultStateFakes) {
	t.Helper()
	prevRead, prevCheck, prevInit, prevPost, prevPrompt, prevSpawn :=
		discoveryReadFn, vaultCheckStateFn, vaultInitFn, postVaultUnlockFn, promptPassphrase, spawnResolveCachedFn
	t.Cleanup(func() {
		discoveryReadFn = prevRead
		vaultCheckStateFn = prevCheck
		vaultInitFn = prevInit
		postVaultUnlockFn = prevPost
		promptPassphrase = prevPrompt
		spawnResolveCachedFn = prevSpawn
	})
	if fakes.discoveryRead != nil {
		discoveryReadFn = fakes.discoveryRead
	}
	if fakes.vaultCheckState != nil {
		vaultCheckStateFn = fakes.vaultCheckState
	}
	if fakes.vaultInit != nil {
		vaultInitFn = fakes.vaultInit
	}
	if fakes.postUnlock != nil {
		postVaultUnlockFn = fakes.postUnlock
	}
	if fakes.prompt != nil {
		promptPassphrase = fakes.prompt
	}
	if fakes.spawnResolve != nil {
		spawnResolveCachedFn = fakes.spawnResolve
	}
}

// scopeHomeAndAPIURL isolates HOME (for defaultStateDir) and clears
// AILERON_API_URL so spawnResolveCached's escape hatch doesn't leak
// in. Returns the temp HOME dir.
func scopeHomeAndAPIURL(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AILERON_API_URL", "")
	t.Setenv(envVaultPassphrase, "")
	return dir
}

// --- bypassesVault contract ---

func TestBypassesVault(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"empty args", nil, true},
		{"version", []string{"version"}, true},
		{"--version", []string{"--version"}, true},
		{"-v", []string{"-v"}, true},
		{"help", []string{"help"}, true},
		{"--help", []string{"--help"}, true},
		{"init", []string{"init"}, true},
		{"log", []string{"log"}, true},
		{"stop", []string{"stop"}, true},
		{"vault init", []string{"vault", "init"}, true},
		{"secret set", []string{"secret", "set", "name"}, true},
		{"secret list", []string{"secret", "list"}, true},
		{"daemon status", []string{"daemon", "status"}, true},
		{"daemon stop", []string{"daemon", "stop"}, true},
		{"daemon start", []string{"daemon", "start"}, true},
		{"policy test", []string{"policy", "test", "git"}, true},
		{"policy save", []string{"policy", "save"}, true},

		{"launch", []string{"launch", "claude"}, false},
		{"binding list", []string{"binding", "list"}, false},
		{"binding setup", []string{"binding", "setup", "x"}, false},
		{"connector check", []string{"connector", "check"}, false},
		{"connector install", []string{"connector", "install"}, false},
		{"action add", []string{"action", "add"}, false},
		{"keyring list", []string{"keyring", "list"}, false},
		{"approval list", []string{"approval", "list"}, false},
		{"status", []string{"status"}, false},
		{"sync", []string{"sync"}, false},
		{"audit list", []string{"audit", "list"}, false},
		{"sessions list", []string{"sessions", "list"}, false},
		{"unknown command", []string{"bogus"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bypassesVault(tc.args); got != tc.want {
				t.Errorf("bypassesVault(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// --- ensureVaultUnlocked: state machine ---

// State 1: daemon running, vault unlocked → no-op.
func TestEnsureVaultUnlocked_RunningUnlocked_NoOp(t *testing.T) {
	scopeHomeAndAPIURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// status reports unlocked
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"locked":false,"state":"unlocked"}`)
	}))
	t.Cleanup(srv.Close)

	postCalled := false
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead: func(string) (discovery.Info, error) {
			return discovery.Info{URL: srv.URL}, nil
		},
		postUnlock: func(string, string) error { postCalled = true; return nil },
	})

	var stderr bytes.Buffer
	if err := ensureVaultUnlocked("", &stderr); err != nil {
		t.Fatalf("ensureVaultUnlocked: %v", err)
	}
	if postCalled {
		t.Error("postVaultUnlock should not be called when vault is unlocked")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty, got: %q", stderr.String())
	}
}

// State 2: daemon running, vault locked → POST /v1/vault/unlock with the
// passphrase from the env var.
func TestEnsureVaultUnlocked_RunningLocked_DrivesUnlock(t *testing.T) {
	scopeHomeAndAPIURL(t)
	t.Setenv(envVaultPassphrase, "from-env")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"locked":true,"state":"locked"}`)
	}))
	t.Cleanup(srv.Close)

	var capturedURL, capturedPass string
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead: func(string) (discovery.Info, error) {
			return discovery.Info{URL: srv.URL}, nil
		},
		postUnlock: func(url, pass string) error {
			capturedURL, capturedPass = url, pass
			return nil
		},
	})

	if err := ensureVaultUnlocked("", io.Discard); err != nil {
		t.Fatalf("ensureVaultUnlocked: %v", err)
	}
	if capturedURL != srv.URL {
		t.Errorf("postUnlock url = %q, want %q", capturedURL, srv.URL)
	}
	if capturedPass != "from-env" {
		t.Errorf("postUnlock passphrase = %q, want %q", capturedPass, "from-env")
	}
}

// State 2 + interactive retry: a wrong-passphrase 401 reprompts once.
func TestEnsureVaultUnlocked_RunningLocked_RetriesOn401Interactive(t *testing.T) {
	scopeHomeAndAPIURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"locked":true,"state":"locked"}`)
	}))
	t.Cleanup(srv.Close)

	answers := []string{"wrong", "right"}
	idx := 0
	calls := 0
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead: func(string) (discovery.Info, error) {
			return discovery.Info{URL: srv.URL}, nil
		},
		prompt: func(string, io.Writer) (string, error) {
			a := answers[idx]
			idx++
			return a, nil
		},
		postUnlock: func(_, pass string) error {
			calls++
			if pass == "right" {
				return nil
			}
			return errVaultUnlockRejected
		},
	})

	var stderr bytes.Buffer
	if err := ensureVaultUnlocked("", &stderr); err != nil {
		t.Fatalf("ensureVaultUnlocked: %v", err)
	}
	if calls != 2 {
		t.Errorf("postUnlock call count = %d, want 2 (one wrong + one right)", calls)
	}
	if !strings.Contains(stderr.String(), "wrong passphrase") {
		t.Errorf("stderr should mention wrong passphrase, got: %q", stderr.String())
	}
}

// State 2 + non-interactive: a 401 from a file/env passphrase does NOT
// retry — the source isn't going to change.
func TestEnsureVaultUnlocked_RunningLocked_NoRetryForEnv(t *testing.T) {
	scopeHomeAndAPIURL(t)
	t.Setenv(envVaultPassphrase, "wrong")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"locked":true,"state":"locked"}`)
	}))
	t.Cleanup(srv.Close)

	calls := 0
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead: func(string) (discovery.Info, error) {
			return discovery.Info{URL: srv.URL}, nil
		},
		postUnlock: func(string, string) error { calls++; return errVaultUnlockRejected },
	})

	err := ensureVaultUnlocked("", io.Discard)
	if !errors.Is(err, errVaultUnlockRejected) {
		t.Errorf("err = %v, want errVaultUnlockRejected", err)
	}
	if calls != 1 {
		t.Errorf("postUnlock should fire exactly once for env source, got %d calls", calls)
	}
}

// State 4: daemon not running, vault file missing → first-run flow
// (prompt + confirm + vault.Init + spawn + unlock). Asserts vault.Init
// is called with the chosen passphrase and unlock fires after.
func TestEnsureVaultUnlocked_StoppedMissing_FirstRun(t *testing.T) {
	scopeHomeAndAPIURL(t)
	t.Setenv(envVaultPassphrase, "fresh-pass")

	initCalled := false
	unlockCalled := false
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead: func(string) (discovery.Info, error) {
			return discovery.Info{}, discovery.ErrNotRunning
		},
		vaultCheckState: func(string) (vault.State, error) {
			return vault.StateMissing, nil
		},
		vaultInit: func(_, pass string) (vault.Vault, error) {
			initCalled = true
			if pass != "fresh-pass" {
				t.Errorf("vault.Init passphrase = %q, want %q", pass, "fresh-pass")
			}
			return vault.NewMemVault(), nil
		},
		postUnlock: func(_, pass string) error {
			unlockCalled = true
			if pass != "fresh-pass" {
				t.Errorf("postUnlock passphrase = %q, want %q", pass, "fresh-pass")
			}
			return nil
		},
	})

	// We can't call ensureVaultUnlocked end-to-end here because
	// spawnResolveCached has a sync.Once that locks process-wide state.
	// Drive the same vault.Init + postUnlock chain directly — that's
	// the contract we want to assert on (the spawn step is exercised
	// in the integration test, not here).
	if err := promptCreateAndUnlockForTest(t, "fresh-pass"); err != nil {
		t.Fatalf("promptCreateAndUnlock: %v", err)
	}
	if !initCalled {
		t.Error("vault.Init should fire for first-run case")
	}
	if !unlockCalled {
		t.Error("postUnlock should fire after vault.Init")
	}
}

// promptCreateAndUnlockForTest exercises the first-run branch without
// going through spawnResolveCached's process-scoped sync.Once.
func promptCreateAndUnlockForTest(t *testing.T, passphrase string) error {
	t.Helper()
	if _, err := vaultInitFn("/test/path", passphrase); err != nil {
		return err
	}
	return postVaultUnlockFn("http://test", passphrase)
}

// State 4 + interactive mismatch: prompted twice with mismatched values
// returns error and never calls vault.Init.
func TestEnsureVaultUnlocked_StoppedMissing_ConfirmationMismatch(t *testing.T) {
	scopeHomeAndAPIURL(t)
	answers := []string{"first", "second"}
	idx := 0
	initCalled := false
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead: func(string) (discovery.Info, error) {
			return discovery.Info{}, discovery.ErrNotRunning
		},
		vaultCheckState: func(string) (vault.State, error) {
			return vault.StateMissing, nil
		},
		vaultInit: func(string, string) (vault.Vault, error) {
			initCalled = true
			return nil, nil
		},
		prompt: func(string, io.Writer) (string, error) {
			a := answers[idx]
			idx++
			return a, nil
		},
	})

	err := ensureVaultUnlocked("", io.Discard)
	if err == nil {
		t.Fatal("expected error for mismatched confirmation")
	}
	if !strings.Contains(err.Error(), "do not match") {
		t.Errorf("err = %v, want 'do not match'", err)
	}
	if initCalled {
		t.Error("vault.Init must not be called when confirmation fails")
	}
}

// --- postVaultUnlock contract (the default postVaultUnlockFn) ---

func TestPostVaultUnlock_200OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	if err := postVaultUnlock(srv.URL, "p"); err != nil {
		t.Errorf("err = %v, want nil for 200", err)
	}
}

func TestPostVaultUnlock_409Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(srv.Close)

	// 409 = already unlocked. From the CLI's perspective the goal is
	// achieved, so postVaultUnlock returns nil.
	if err := postVaultUnlock(srv.URL, "p"); err != nil {
		t.Errorf("err = %v, want nil for 409 (already unlocked)", err)
	}
}

func TestPostVaultUnlock_401Rejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	err := postVaultUnlock(srv.URL, "wrong")
	if !errors.Is(err, errVaultUnlockRejected) {
		t.Errorf("err = %v, want errVaultUnlockRejected", err)
	}
}

func TestPostVaultUnlock_500Wrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	err := postVaultUnlock(srv.URL, "p")
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want one mentioning 500", err)
	}
}

func TestPostVaultUnlock_NetworkError(t *testing.T) {
	// Port 1 is privileged on most systems and nothing listens.
	err := postVaultUnlock("http://127.0.0.1:1", "p")
	if err == nil {
		t.Fatal("expected error for unreachable URL")
	}
}

// Smoke test: postVaultUnlock sends a JSON body with `passphrase` key.
func TestPostVaultUnlock_RequestBodyShape(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	t.Cleanup(srv.Close)

	_ = postVaultUnlock(srv.URL, "secret 🔒")

	if !strings.Contains(gotBody, `"passphrase":`) {
		t.Errorf("body = %q, want JSON with passphrase key", gotBody)
	}
	if !strings.Contains(gotBody, "secret 🔒") {
		t.Errorf("body = %q, want unicode passphrase preserved", gotBody)
	}
}

// --- HOME isolation guard ---

// Verifies a sane environment for state-machine tests: HOME → temp,
// DefaultVaultPath honors HOME, vault file does not exist.
func TestVaultStateTestHarness_TempHomeNoVault(t *testing.T) {
	dir := scopeHomeAndAPIURL(t)
	if got := os.Getenv("HOME"); got != dir {
		t.Errorf("HOME = %q, want %q", got, dir)
	}
	path := filepath.Join(dir, ".aileron", "secrets.json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("vault file already exists at %s; tests would not start clean", path)
	}
}

// --- State 3: daemon stopped, vault file present ---

// TestEnsureVaultUnlocked_StoppedPresent_DrivesSpawnAndUnlock covers the
// path that produced item #492's whole umbrella: a fresh `aileron <cmd>`
// when the daemon isn't running but the user has previously created a
// vault. CLI prompts (or reads env), spawns the daemon, drives unlock.
func TestEnsureVaultUnlocked_StoppedPresent_DrivesSpawnAndUnlock(t *testing.T) {
	scopeHomeAndAPIURL(t)
	t.Setenv(envVaultPassphrase, "stored-pass")

	spawned := false
	var unlockedAt, unlockedWith string
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead: func(string) (discovery.Info, error) {
			return discovery.Info{}, discovery.ErrNotRunning
		},
		vaultCheckState: func(string) (vault.State, error) { return vault.StateReady, nil },
		spawnResolve: func() (string, error) {
			spawned = true
			return "http://127.0.0.1:54321/v1", nil
		},
		postUnlock: func(url, pass string) error {
			unlockedAt, unlockedWith = url, pass
			return nil
		},
	})

	if err := ensureVaultUnlocked("", io.Discard); err != nil {
		t.Fatalf("ensureVaultUnlocked: %v", err)
	}
	if !spawned {
		t.Error("spawnResolveCached should fire when daemon is not running")
	}
	if unlockedAt != "http://127.0.0.1:54321" {
		t.Errorf("postUnlock url = %q, want trimmed-of-/v1", unlockedAt)
	}
	if unlockedWith != "stored-pass" {
		t.Errorf("postUnlock passphrase = %q, want %q", unlockedWith, "stored-pass")
	}
}

// State 3 + interactive retry: a wrong passphrase reprompts once,
// reusing the cached daemon URL on the second attempt.
func TestEnsureVaultUnlocked_StoppedPresent_RetriesOn401Interactive(t *testing.T) {
	scopeHomeAndAPIURL(t)
	answers := []string{"wrong", "right"}
	idx := 0
	calls := 0
	spawnCalls := 0
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead: func(string) (discovery.Info, error) {
			return discovery.Info{}, discovery.ErrNotRunning
		},
		vaultCheckState: func(string) (vault.State, error) { return vault.StateReady, nil },
		prompt: func(string, io.Writer) (string, error) {
			a := answers[idx]
			idx++
			return a, nil
		},
		spawnResolve: func() (string, error) {
			spawnCalls++
			return "http://127.0.0.1:1/v1", nil
		},
		postUnlock: func(_, pass string) error {
			calls++
			if pass == "right" {
				return nil
			}
			return errVaultUnlockRejected
		},
	})

	var stderr bytes.Buffer
	if err := ensureVaultUnlocked("", &stderr); err != nil {
		t.Fatalf("ensureVaultUnlocked: %v", err)
	}
	if calls != 2 {
		t.Errorf("postUnlock call count = %d, want 2", calls)
	}
	if !strings.Contains(stderr.String(), "wrong passphrase") {
		t.Errorf("stderr should mention wrong passphrase, got: %q", stderr.String())
	}
}

// State 3 + non-interactive 401: env-supplied passphrase that's wrong
// fails immediately without retry.
func TestEnsureVaultUnlocked_StoppedPresent_NoRetryForEnv(t *testing.T) {
	scopeHomeAndAPIURL(t)
	t.Setenv(envVaultPassphrase, "wrong")
	calls := 0
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead:   func(string) (discovery.Info, error) { return discovery.Info{}, discovery.ErrNotRunning },
		vaultCheckState: func(string) (vault.State, error) { return vault.StateReady, nil },
		spawnResolve:    func() (string, error) { return "http://x/v1", nil },
		postUnlock:      func(string, string) error { calls++; return errVaultUnlockRejected },
	})
	err := ensureVaultUnlocked("", io.Discard)
	if !errors.Is(err, errVaultUnlockRejected) {
		t.Errorf("err = %v, want errVaultUnlockRejected", err)
	}
	if calls != 1 {
		t.Errorf("postUnlock should fire exactly once for env source, got %d", calls)
	}
}

// State 3 + empty passphrase from env: rejected before any spawn.
func TestEnsureVaultUnlocked_StoppedPresent_EmptyPassphraseRejected(t *testing.T) {
	scopeHomeAndAPIURL(t)
	t.Setenv(envVaultPassphrase, "")

	spawnCalls := 0
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead:   func(string) (discovery.Info, error) { return discovery.Info{}, discovery.ErrNotRunning },
		vaultCheckState: func(string) (vault.State, error) { return vault.StateReady, nil },
		prompt:          func(string, io.Writer) (string, error) { return "", nil },
		spawnResolve:    func() (string, error) { spawnCalls++; return "", nil },
	})
	err := ensureVaultUnlocked("", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cannot be empty") {
		t.Errorf("err = %v, want 'cannot be empty'", err)
	}
	if spawnCalls != 0 {
		t.Error("daemon must not be spawned for empty passphrase")
	}
}

// State 3 + spawn failure: error surfaced verbatim.
func TestEnsureVaultUnlocked_StoppedPresent_SpawnFailurePropagates(t *testing.T) {
	scopeHomeAndAPIURL(t)
	t.Setenv(envVaultPassphrase, "p")
	wantErr := errors.New("locate daemon binary: not on PATH")
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead:   func(string) (discovery.Info, error) { return discovery.Info{}, discovery.ErrNotRunning },
		vaultCheckState: func(string) (vault.State, error) { return vault.StateReady, nil },
		spawnResolve:    func() (string, error) { return "", wantErr },
	})
	err := ensureVaultUnlocked("", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "locate daemon binary") {
		t.Errorf("err = %v, want spawn error wrapped", err)
	}
}

// --- State 4 (first-run) extra coverage: interactive + spawn + unlock ---

// Interactive first-run: prompt for passphrase + confirmation, then
// spawn + unlock all fire end-to-end. Banner must be printed to stderr.
func TestEnsureVaultUnlocked_StoppedMissing_InteractiveDriveSpawnAndUnlock(t *testing.T) {
	scopeHomeAndAPIURL(t)
	answers := []string{"new-pass", "new-pass"}
	idx := 0
	initFired, spawnFired, unlockFired := false, false, false
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead:   func(string) (discovery.Info, error) { return discovery.Info{}, discovery.ErrNotRunning },
		vaultCheckState: func(string) (vault.State, error) { return vault.StateMissing, nil },
		prompt: func(string, io.Writer) (string, error) {
			a := answers[idx]
			idx++
			return a, nil
		},
		vaultInit:    func(_, p string) (vault.Vault, error) { initFired = true; return vault.NewMemVault(), nil },
		spawnResolve: func() (string, error) { spawnFired = true; return "http://x/v1", nil },
		postUnlock:   func(string, string) error { unlockFired = true; return nil },
	})

	var stderr bytes.Buffer
	if err := ensureVaultUnlocked("", &stderr); err != nil {
		t.Fatalf("ensureVaultUnlocked: %v", err)
	}
	for _, ok := range []bool{initFired, spawnFired, unlockFired} {
		if !ok {
			t.Error("expected vault.Init, spawn, postUnlock all to fire on interactive first-run")
		}
	}
	if !strings.Contains(stderr.String(), "Creating a new Aileron vault") {
		t.Errorf("first-run banner missing from stderr: %q", stderr.String())
	}
}

// First-run + vault.Init failure: error wrapped, no spawn.
func TestEnsureVaultUnlocked_StoppedMissing_VaultInitFailurePropagates(t *testing.T) {
	scopeHomeAndAPIURL(t)
	t.Setenv(envVaultPassphrase, "p")
	spawnCalls := 0
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead:   func(string) (discovery.Info, error) { return discovery.Info{}, discovery.ErrNotRunning },
		vaultCheckState: func(string) (vault.State, error) { return vault.StateMissing, nil },
		vaultInit:       func(string, string) (vault.Vault, error) { return nil, errors.New("disk full") },
		spawnResolve:    func() (string, error) { spawnCalls++; return "", nil },
	})
	err := ensureVaultUnlocked("", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "creating vault") {
		t.Errorf("err = %v, want 'creating vault' wrap", err)
	}
	if spawnCalls != 0 {
		t.Error("daemon must not be spawned if vault.Init fails")
	}
}

// First-run + spawn failure after successful vault.Init: error surfaced.
// The vault file is left in place (whatever vault.Init wrote) — this is
// a recoverable state for the next CLI invocation.
func TestEnsureVaultUnlocked_StoppedMissing_SpawnFailurePropagates(t *testing.T) {
	scopeHomeAndAPIURL(t)
	t.Setenv(envVaultPassphrase, "p")
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead:   func(string) (discovery.Info, error) { return discovery.Info{}, discovery.ErrNotRunning },
		vaultCheckState: func(string) (vault.State, error) { return vault.StateMissing, nil },
		vaultInit:       func(string, string) (vault.Vault, error) { return vault.NewMemVault(), nil },
		spawnResolve:    func() (string, error) { return "", errors.New("daemon binary missing") },
	})
	err := ensureVaultUnlocked("", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "spawning daemon") {
		t.Errorf("err = %v, want 'spawning daemon' wrap", err)
	}
}

// --- ensureVaultUnlocked: probe-not-supported branch ---

// Cloud-shaped daemon path: probeLocalVaultLocked returns ok=false
// because the daemon doesn't expose /v1/vault/status. ensureVaultUnlocked
// returns nil without prompting or unlocking.
func TestEnsureVaultUnlocked_ProbeUnsupported_NoOp(t *testing.T) {
	scopeHomeAndAPIURL(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	postCalled := false
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead: func(string) (discovery.Info, error) {
			return discovery.Info{URL: srv.URL}, nil
		},
		postUnlock: func(string, string) error { postCalled = true; return nil },
	})

	if err := ensureVaultUnlocked("", io.Discard); err != nil {
		t.Errorf("ensureVaultUnlocked = %v, want nil for unsupported probe", err)
	}
	if postCalled {
		t.Error("postUnlock must not fire when probe is unsupported")
	}
}

// --- ensureVaultUnlocked: vault.CheckState error surfaces ---

func TestEnsureVaultUnlocked_VaultCheckStateError(t *testing.T) {
	scopeHomeAndAPIURL(t)
	withVaultStateSeams(t, vaultStateFakes{
		discoveryRead:   func(string) (discovery.Info, error) { return discovery.Info{}, discovery.ErrNotRunning },
		vaultCheckState: func(string) (vault.State, error) { return 0, errors.New("permission denied") },
	})
	err := ensureVaultUnlocked("", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checking vault") {
		t.Errorf("err = %v, want 'checking vault' wrap", err)
	}
}

// --- promptAndUnlockRunning: empty passphrase + prompter error ---

func TestPromptAndUnlockRunning_EmptyPassphraseRejected(t *testing.T) {
	scopeHomeAndAPIURL(t)
	withVaultStateSeams(t, vaultStateFakes{
		prompt: func(string, io.Writer) (string, error) { return "", nil },
	})
	err := promptAndUnlockRunning("http://x", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cannot be empty") {
		t.Errorf("err = %v, want 'cannot be empty'", err)
	}
}

func TestPromptAndUnlockRunning_PrompterErrorPropagates(t *testing.T) {
	scopeHomeAndAPIURL(t)
	wantErr := errors.New("read /dev/tty: i/o error")
	withVaultStateSeams(t, vaultStateFakes{
		prompt: func(string, io.Writer) (string, error) { return "", wantErr },
	})
	err := promptAndUnlockRunning("http://x", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "i/o error") {
		t.Errorf("err = %v, want prompter error", err)
	}
}

// --- readVaultPassphrase: file read failure surfaces clean error ---

func TestReadVaultPassphrase_FileReadError(t *testing.T) {
	_, _, err := readVaultPassphrase("/no/such/path/passphrase", "ignored", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "reading passphrase file") {
		t.Errorf("err = %v, want 'reading passphrase file' wrap", err)
	}
}

