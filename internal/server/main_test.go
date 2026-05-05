package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

// TestSelectVault_NoFileFallsBackToMemory verifies that the standalone
// server uses the in-memory dev vault when no vault file exists at
// the path. Other modes are gated on this state — if the file is
// missing, the server should not attempt to prompt the user.
func TestSelectVault_NoFileFallsBackToMemory(t *testing.T) {
	dir := t.TempDir()
	cfg, err := selectVault(slog.Default(), filepath.Join(dir, "secrets.json"), true, refusePrompter(t), io.Discard)
	if err != nil {
		t.Fatalf("selectVault: %v", err)
	}
	if cfg.Vault != nil {
		t.Errorf("cfg.Vault = %v, want nil (in-memory fallback)", cfg.Vault)
	}
}

// TestSelectVault_NonTTYFallsBackToMemoryEvenWithFile asserts that the
// server does not attempt to unlock a persistent vault in headless
// contexts (Docker, CI, systemd) — there is nowhere to prompt the
// passphrase. Without this guard the server would block on read from
// /dev/tty during startup.
func TestSelectVault_NonTTYFallsBackToMemoryEvenWithFile(t *testing.T) {
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
		t.Errorf("cfg.Vault = %v, want nil (non-TTY should never unlock)", cfg.Vault)
	}
}

// TestSelectVault_PresentFileAndTTYUnlocks is the regression test for
// the bug this commit fixes: prior to this change the standalone
// server always took the in-memory path, even when a persistent vault
// file existed. That silently dropped every binding created via
// `aileron binding setup` the moment the server process exited,
// surfacing later as a `binding_required` error from the launch
// gateway. After this change the server unlocks the persistent vault
// when conditions allow.
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
