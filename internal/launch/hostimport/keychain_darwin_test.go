//go:build darwin

package hostimport

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newTestKeychain creates a throwaway, password-protected keychain
// under t.TempDir(), unlocks it, and registers cleanup. The real
// "Claude Code-credentials" / "Codex Auth" items in the user's login
// keychain are never touched: every read in this file targets this
// throwaway keychain via Options.KeychainPath.
func newTestKeychain(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("/usr/bin/security"); err != nil {
		t.Skipf("/usr/bin/security unavailable: %v", err)
	}
	path := filepath.Join(t.TempDir(), "hostimport-test.keychain-db")
	const pw = "hostimport-test"
	if out, err := exec.Command("/usr/bin/security", "create-keychain", "-p", pw, path).CombinedOutput(); err != nil {
		t.Skipf("create-keychain: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("/usr/bin/security", "delete-keychain", path).Run()
	})
	if out, err := exec.Command("/usr/bin/security", "unlock-keychain", "-p", pw, path).CombinedOutput(); err != nil {
		t.Fatalf("unlock-keychain: %v: %s", err, out)
	}
	return path
}

// addGenericPassword stores fixture bytes under a service in the test
// keychain and makes the read non-interactive via set-key-partition-list
// so cmd.Run() does not block on a Keychain access dialog in CI.
func addGenericPassword(t *testing.T, keychain, service string, value []byte) {
	t.Helper()
	const pw = "hostimport-test"
	if out, err := exec.Command("/usr/bin/security", "add-generic-password",
		"-a", "hostimport", "-s", service, "-w", string(value), keychain).CombinedOutput(); err != nil {
		t.Fatalf("add-generic-password: %v: %s", err, out)
	}
	// Allow the security tool itself to read the item without prompting.
	if out, err := exec.Command("/usr/bin/security", "set-key-partition-list",
		"-S", "apple-tool:,apple:", "-s", "-k", pw, keychain).CombinedOutput(); err != nil {
		t.Skipf("set-key-partition-list (non-interactive read unavailable): %v: %s", err, out)
	}
}

// TestExtract_KeychainReturnsBytes confirms Claude's macOS path reads
// the generic-password item via `security -w` and returns the stored
// bytes with the -w trailing newline trimmed.
func TestExtract_KeychainReturnsBytes(t *testing.T) {
	keychain := newTestKeychain(t)
	const service = "hostimport-test-claude"
	want := []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`)
	addGenericPassword(t, keychain, service, want)

	got, err := Extract(AgentClaude, Options{
		KeychainService: service,
		KeychainPath:    keychain,
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Extract = %q, want %q (no trailing newline)", got, want)
	}
}

// TestExtract_ClaudeDefaultServiceName exercises the default-service
// branch: when Options.KeychainService is empty, Claude reads the
// DefaultClaudeKeychainService name. The read still targets the
// throwaway keychain (KeychainPath set), so the real login-keychain
// item is never touched — we seed the throwaway keychain under the
// default service name.
func TestExtract_ClaudeDefaultServiceName(t *testing.T) {
	keychain := newTestKeychain(t)
	want := []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`)
	addGenericPassword(t, keychain, DefaultClaudeKeychainService, want)

	got, err := Extract(AgentClaude, Options{KeychainPath: keychain})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Extract = %q, want %q", got, want)
	}
}

// TestExtract_CodexDefaultServiceName exercises the Codex
// default-service branch the same way, with an empty HOME so the file
// is absent and the keyring fallback fires.
func TestExtract_CodexDefaultServiceName(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.codex/auth.json
	keychain := newTestKeychain(t)
	want := []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r"}}`)
	addGenericPassword(t, keychain, DefaultCodexKeychainService, want)

	got, err := Extract(AgentCodex, Options{KeychainPath: keychain})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Extract = %q, want %q", got, want)
	}
}

// TestExtract_KeychainEmptyPassword maps a stored-but-empty keychain
// item to ErrNotAuthenticated, covering the empty-output guard in
// readKeychain.
func TestExtract_KeychainEmptyPassword(t *testing.T) {
	keychain := newTestKeychain(t)
	const service = "hostimport-test-empty"
	addGenericPassword(t, keychain, service, []byte(""))

	_, err := Extract(AgentClaude, Options{
		KeychainService: service,
		KeychainPath:    keychain,
	})
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("Extract(empty password): err = %v, want ErrNotAuthenticated", err)
	}
}

// TestExtract_KeychainMissingItem maps an absent keychain item to
// ErrNotAuthenticated.
func TestExtract_KeychainMissingItem(t *testing.T) {
	keychain := newTestKeychain(t)
	_, err := Extract(AgentClaude, Options{
		KeychainService: "hostimport-test-absent",
		KeychainPath:    keychain,
	})
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("Extract(missing item): err = %v, want ErrNotAuthenticated", err)
	}
}

// TestExtract_CodexPrefersFileOverKeychain confirms the Codex macOS
// precedence: when ~/.codex/auth.json exists, the file wins over the
// keyring. We redirect HOME to a temp dir, seed the file, and assert
// the file bytes come back even though no keychain item is registered.
func TestExtract_CodexPrefersFileOverKeychain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r"}}`)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), want, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Extract(AgentCodex, Options{
		KeychainService: "hostimport-test-absent",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Extract = %q, want file bytes %q", got, want)
	}
}

// TestExtract_CodexFallsBackToKeychain confirms that when no
// ~/.codex/auth.json exists, the Codex macOS path falls back to the
// keyring service.
func TestExtract_CodexFallsBackToKeychain(t *testing.T) {
	home := t.TempDir() // empty home: no ~/.codex/auth.json
	t.Setenv("HOME", home)
	keychain := newTestKeychain(t)
	const service = "hostimport-test-codex"
	want := []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r"}}`)
	addGenericPassword(t, keychain, service, want)

	got, err := Extract(AgentCodex, Options{
		KeychainService: service,
		KeychainPath:    keychain,
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Extract = %q, want %q", got, want)
	}
}
