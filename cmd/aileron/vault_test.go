package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

// `aileron vault init` is the deliberate first-run flow per ADR-0011 /
// #492 item 5d. The contract these tests pin:
//
//   - Missing vault, interactive prompt:
//     prompt for passphrase + confirmation; on match, write the vault
//     file, return 0, and print the path.
//   - Missing vault, --passphrase-file <path>:
//     read once, no confirmation needed, write the vault.
//   - Missing vault, AILERON_VAULT_PASSPHRASE env:
//     same as --passphrase-file but the source is the env var.
//   - Existing vault: refuse with a non-zero exit and a hint that
//     deleting the file destroys all stored secrets.
//   - Empty passphrase: refuse with exit 1.
//   - Mismatched confirmation in interactive mode: refuse with exit 1.
//
// Tests use the swappable `promptPassphrase` package var to script the
// prompt sequence, and t.Setenv("HOME", ...) to redirect
// launch.DefaultVaultPath() at a temp dir so the real ~/.aileron is
// never touched.

func scriptPrompt(answers ...string) func() {
	prev := promptPassphrase
	idx := 0
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		if idx >= len(answers) {
			return "", io.EOF
		}
		ans := answers[idx]
		idx++
		return ans, nil
	}
	return func() { promptPassphrase = prev }
}

func vaultInitTempHome(t *testing.T) string {
	t.Helper()
	dir := setTestHome(t)
	t.Setenv(envVaultPassphrase, "")
	return filepath.Join(dir, ".aileron", "secrets.json")
}

func TestRunVaultInit_InteractiveCreatesVault(t *testing.T) {
	vaultPath := vaultInitTempHome(t)
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	restore := scriptPrompt("correct horse", "correct horse")
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"vault", "init"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Vault created at") {
		t.Errorf("stdout = %q; want 'Vault created at ...'", stdout.String())
	}

	state, err := vault.CheckState(vaultPath)
	if err != nil {
		t.Fatalf("CheckState: %v", err)
	}
	if state != vault.StateReady {
		t.Errorf("state = %v, want StateReady (vault file should exist)", state)
	}
}

func TestRunVaultInit_PassphraseFileSkipsConfirmation(t *testing.T) {
	vaultPath := vaultInitTempHome(t)
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}

	pf := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(pf, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// No prompt should fire — script none and assert the prompter is
	// never reached.
	restore := scriptPrompt()
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"vault", "init", "--passphrase-file", pf}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}

	state, _ := vault.CheckState(vaultPath)
	if state != vault.StateReady {
		t.Errorf("state = %v, want StateReady", state)
	}
}

func TestRunVaultInit_EnvVarSkipsConfirmation(t *testing.T) {
	vaultPath := vaultInitTempHome(t)
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envVaultPassphrase, "from-env")

	restore := scriptPrompt() // must not fire
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"vault", "init"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}

	state, _ := vault.CheckState(vaultPath)
	if state != vault.StateReady {
		t.Errorf("state = %v, want StateReady", state)
	}
}

func TestRunVaultInit_RefusesIfVaultExists(t *testing.T) {
	vaultPath := vaultInitTempHome(t)
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Init(vaultPath, "first"); err != nil {
		t.Fatalf("seed vault: %v", err)
	}

	restore := scriptPrompt() // must not fire
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"vault", "init"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr = %q; want 'already exists'", stderr.String())
	}
}

func TestRunVaultInit_EmptyPassphraseRefused(t *testing.T) {
	_ = vaultInitTempHome(t)
	restore := scriptPrompt("") // empty
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"vault", "init"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "passphrase cannot be empty") {
		t.Errorf("stderr = %q; want 'passphrase cannot be empty'", stderr.String())
	}
}

func TestRunVaultInit_MismatchedConfirmationRefused(t *testing.T) {
	_ = vaultInitTempHome(t)
	restore := scriptPrompt("first", "second") // mismatch
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"vault", "init"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "do not match") {
		t.Errorf("stderr = %q; want 'do not match'", stderr.String())
	}
}

func TestRunVaultInit_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"vault", "bogus"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown vault command") {
		t.Errorf("stderr = %q; want 'unknown vault command'", stderr.String())
	}
}

func TestRunVaultInit_NoSubcommandPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"vault"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "aileron vault init") {
		t.Errorf("stderr = %q; want usage", stderr.String())
	}
}

func TestRunVaultInit_PassphraseFileTrimsTrailingNewline(t *testing.T) {
	// CRLF and trailing LF must be stripped so `echo "p" > pf` works
	// without the passphrase silently containing a trailing newline.
	vaultPath := vaultInitTempHome(t)
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}

	pf := filepath.Join(t.TempDir(), "pf")
	if err := os.WriteFile(pf, []byte("trim-me\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"vault", "init", "--passphrase-file", pf}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}

	// Verify the trimmed passphrase actually unlocks — i.e. CheckState
	// returns StateReady AND vault.Unlock with "trim-me" (no CRLF) works.
	if _, err := vault.Unlock(vaultPath, "trim-me"); err != nil {
		t.Errorf("Unlock with trimmed passphrase: %v (passphrase was not trimmed correctly)", err)
	}
}

// TestRunVaultInit_BannerPrintsBeforeFirstPrompt regression-pins
// #528 finding 1 (the `vault init` flow shares the same first-run
// banner with the daemon-auto-spawn path; both must print the
// irretrievable-passphrase warning BEFORE the user types a passphrase,
// not between the two prompts).
//
// The scripted prompt writes its prompt string to the supplied writer
// (mirroring defaultPromptPassphrase) so the captured stderr buffer
// reflects natural ordering.
func TestRunVaultInit_BannerPrintsBeforeFirstPrompt(t *testing.T) {
	vaultPath := vaultInitTempHome(t)
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}

	answers := []string{"correct horse", "correct horse"}
	idx := 0
	prevPrompt := promptPassphrase
	t.Cleanup(func() { promptPassphrase = prevPrompt })
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		if w != nil && prompt != "" {
			_, _ = w.Write([]byte(prompt))
		}
		a := answers[idx]
		idx++
		return a, nil
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"vault", "init"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stderr.String()
	bannerAt := strings.Index(out, "Creating a new Aileron vault")
	promptAt := strings.Index(out, "Vault passphrase:")
	if bannerAt < 0 {
		t.Fatalf("banner missing from stderr: %q", out)
	}
	if promptAt < 0 {
		t.Fatalf("first prompt missing from stderr: %q", out)
	}
	if bannerAt > promptAt {
		t.Errorf("banner must appear BEFORE first 'Vault passphrase:' prompt; banner@%d, prompt@%d\nstderr=%q",
			bannerAt, promptAt, out)
	}
}

func TestRunVaultInit_AppearsInHelp(t *testing.T) {
	var stdout bytes.Buffer
	usage(&stdout, newTestRegistry())
	if !strings.Contains(stdout.String(), "aileron vault init") {
		t.Errorf("usage missing 'aileron vault init':\n%s", stdout.String())
	}
}

// TestRunSecretSet_HonorsEnvPassphrase: `aileron secret set` reads the
// vault passphrase from AILERON_VAULT_PASSPHRASE without prompting.
// Item 5c of #492 — the CI / scripts escape hatch must work end-to-end
// at every prompt point, not just `vault init`.
//
// The secret value still comes from the prompt, since it is a distinct
// piece of input from the vault passphrase.
func TestRunSecretSet_HonorsEnvPassphrase(t *testing.T) {
	vaultPath := vaultInitTempHome(t)
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envVaultPassphrase, "from-env-pass")

	// Only the secret VALUE prompt should fire; the vault-passphrase
	// prompt should be bypassed by the env var. Single scripted answer.
	restore := scriptPrompt("the-secret-value")
	defer restore()

	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "set", "linear_token"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `Stored secret "secret/linear_token"`) {
		t.Errorf("stdout = %q; want 'Stored secret ...'", stdout.String())
	}

	// Round-trip: open with the env-supplied passphrase and read back
	// the stored secret. Validates both that the vault was created
	// with the expected passphrase and that the value landed correctly.
	v, err := vault.Unlock(vaultPath, "from-env-pass")
	if err != nil {
		t.Fatalf("Unlock with env passphrase: %v", err)
	}
	got, err := v.Get(t.Context(), "secret/linear_token")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Value) != "the-secret-value" {
		t.Errorf("stored value = %q, want %q", string(got.Value), "the-secret-value")
	}
}

// setSecretForTest stores secret/<name> in the temp-home vault via the
// same `aileron secret set` path production uses, so the delete tests
// exercise a genuinely-written entry. The vault passphrase is supplied
// through AILERON_VAULT_PASSPHRASE (already set by the caller) and the
// secret value comes from the scripted prompt.
func setSecretForTest(t *testing.T, name, value string) {
	t.Helper()
	restore := scriptPrompt(value)
	defer restore()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"secret", "set", name}, newTestRegistry(), &stdout, &stderr); code != 0 {
		t.Fatalf("secret set %s: exit = %d; stderr=%s", name, code, stderr.String())
	}
}

// TestRunVaultDelete_SecretRoundTrip is the #1716 regression guard: a
// secret written with `aileron secret set` (stored at secret/<name> in
// the local vault and surfaced by `vault list`) must be deletable with
// `aileron vault delete secret/<name>`. Before the fix vaultDeleteTargetFor
// had no secret/ case, so the command returned the not-CLI-deletable
// rejection — asymmetric with list. This asserts the local-vault delete
// path removes the entry.
func TestRunVaultDelete_SecretRoundTrip(t *testing.T) {
	vaultPath := vaultInitTempHome(t)
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envVaultPassphrase, "from-env-pass")

	setSecretForTest(t, "linear_token", "the-secret-value")

	// Confirm it is present before delete.
	v, err := vault.Unlock(vaultPath, "from-env-pass")
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, err := v.Get(t.Context(), "secret/linear_token"); err != nil {
		t.Fatalf("precondition Get: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "secret/linear_token", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("vault delete: exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Deleted secret/linear_token") {
		t.Errorf("stdout = %q; want 'Deleted secret/linear_token'", stdout.String())
	}

	// The entry must be gone.
	v2, err := vault.Unlock(vaultPath, "from-env-pass")
	if err != nil {
		t.Fatalf("Unlock after delete: %v", err)
	}
	if _, err := v2.Get(t.Context(), "secret/linear_token"); err == nil {
		t.Errorf("secret/linear_token still present after delete")
	}
}

// TestRunVaultDelete_SecretNotFound: deleting a secret/ path that was
// never stored reports the not-found error (exit 1), symmetric with the
// daemon-backed namespaces' 404 handling rather than silently succeeding
// on the idempotent local Delete.
func TestRunVaultDelete_SecretNotFound(t *testing.T) {
	vaultPath := vaultInitTempHome(t)
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envVaultPassphrase, "from-env-pass")

	// Create the vault with a real entry so OpenLocalVault validates the
	// passphrase, then target a different, absent name.
	setSecretForTest(t, "present", "v")

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "secret/absent", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("vault delete absent: exit = %d, want 1; stdout=%s stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no credential entry for secret/absent") {
		t.Errorf("stderr = %q; want 'no credential entry for secret/absent'", stderr.String())
	}
}

// TestRunVaultDelete_SecretRejectsMultiSegment: secret/ is a single-
// segment namespace (matching `secret set`), so a name containing a '/'
// is rejected before the vault is touched.
func TestRunVaultDelete_SecretRejectsMultiSegment(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "secret/foo/bar", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "single segment") {
		t.Errorf("stderr = %q; want 'single segment' rejection", stderr.String())
	}
}

// TestRunVaultDelete_SecretInteractiveCancel: without --yes the secret
// branch honors the confirmation prompt; a non-y answer cancels and the
// entry survives.
func TestRunVaultDelete_SecretInteractiveCancel(t *testing.T) {
	vaultPath := vaultInitTempHome(t)
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envVaultPassphrase, "from-env-pass")
	setSecretForTest(t, "keep", "v")

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "secret/keep"},
		strings.NewReader("n\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cancelled") {
		t.Errorf("stdout = %q; want 'cancelled'", stdout.String())
	}
	v, err := vault.Unlock(vaultPath, "from-env-pass")
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, err := v.Get(t.Context(), "secret/keep"); err != nil {
		t.Errorf("secret/keep should survive a cancelled delete: %v", err)
	}
}
