package launch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

// EnsureVault contract:
//   - Missing file: prompt + confirm, then create. Mismatched
//     confirmations retry the inner prompt up to 3 times.
//   - Existing file: prompt with retry on wrong passphrase up to
//     maxRetries; tampered vault exits immediately.
//   - Returned vault is usable (Put/Get round-trip with KEK held in
//     memory).
//   - Wrong-passphrase loop emits user-facing retry messages on the
//     supplied io.Writer.

// scripted returns a PassphrasePrompter that hands out the next
// answer from `answers`, failing if exhausted.
func scripted(answers ...string) PassphrasePrompter {
	idx := 0
	return func(_ string, _ io.Writer) (string, error) {
		if idx >= len(answers) {
			return "", fmt.Errorf("script exhausted at index %d", idx)
		}
		a := answers[idx]
		idx++
		return a, nil
	}
}

func TestEnsureVault_FirstRun_CreatesVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	out := &bytes.Buffer{}
	v, err := EnsureVault(path, scripted("correct horse", "correct horse"), out, 3)
	if err != nil {
		t.Fatalf("EnsureVault: %v", err)
	}
	if v == nil {
		t.Fatal("returned vault is nil")
	}
	// Round-trip a secret to prove the KEK is in memory.
	if err := v.Put(context.Background(), "k", []byte("v"), vault.Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := v.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Value) != "v" {
		t.Errorf("Get value = %q, want %q", got.Value, "v")
	}
	if !strings.Contains(out.String(), "vault created") {
		t.Errorf("output missing 'vault created' confirmation: %s", out.String())
	}
}

func TestEnsureVault_FirstRun_RetriesOnMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	out := &bytes.Buffer{}
	// First attempt: typo'd confirmation. Second: matched.
	v, err := EnsureVault(path,
		scripted("first", "fisrt", "second", "second"),
		out, 3)
	if err != nil {
		t.Fatalf("EnsureVault: %v", err)
	}
	if v == nil {
		t.Fatal("vault is nil")
	}
	if !strings.Contains(out.String(), "do not match") {
		t.Errorf("output missing mismatch message: %s", out.String())
	}
}

func TestEnsureVault_FirstRun_GivesUpAfter3MismatchAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	_, err := EnsureVault(path,
		scripted("a", "b", "c", "d", "e", "f"),
		&bytes.Buffer{}, 3)
	if err == nil {
		t.Fatal("expected error after 3 mismatched attempts")
	}
	if !strings.Contains(err.Error(), "confirmation failed") {
		t.Errorf("err = %v, want confirmation-failed message", err)
	}
}

func TestEnsureVault_ExistingVault_AcceptsCorrectPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "right"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v, err := EnsureVault(path, scripted("right"), &bytes.Buffer{}, 3)
	if err != nil {
		t.Fatalf("EnsureVault: %v", err)
	}
	if v == nil {
		t.Fatal("vault is nil")
	}
}

func TestEnsureVault_ExistingVault_RetriesOnWrongPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "right"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	out := &bytes.Buffer{}
	v, err := EnsureVault(path, scripted("oops", "wrong", "right"), out, 5)
	if err != nil {
		t.Fatalf("EnsureVault: %v", err)
	}
	if v == nil {
		t.Fatal("vault is nil")
	}
	// Two retry messages should have been emitted.
	if got := strings.Count(out.String(), "wrong passphrase"); got != 2 {
		t.Errorf("retry messages = %d, want 2", got)
	}
}

func TestEnsureVault_ExistingVault_GivesUpAfterMaxRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "right"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, err := EnsureVault(path, scripted("a", "b", "c"), &bytes.Buffer{}, 3)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
}

func TestEnsureVault_TamperedVault_FailsImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Tamper: rewrite the verification blob with garbage encrypted
	// under the same KEK.
	tamperVault(t, path, "p")

	called := 0
	prompter := func(_ string, _ io.Writer) (string, error) {
		called++
		return "p", nil
	}
	_, err := EnsureVault(path, prompter, &bytes.Buffer{}, 5)
	if !errors.Is(err, vault.ErrVaultTampered) {
		t.Errorf("err = %v, want ErrVaultTampered", err)
	}
	if called > 1 {
		t.Errorf("prompter called %d times; tampered vault must not retry", called)
	}
}

// tamperVault replaces the verification blob with one whose plaintext
// is not the canonical sentinel, so vault.Unlock returns ErrVaultTampered.
func tamperVault(t *testing.T, path, passphrase string) {
	t.Helper()
	fv, err := vault.NewFileVault(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	salt, _ := fv.Salt()
	_ = salt
	// Encrypt-then-overwrite happens via direct file editing.
	// The vault.Init test in local_test.go already exercises this
	// pattern; reuse the same approach here via a helper.
	tamperFileVerification(t, path, passphrase)
}

func TestEnsureVault_DefaultPrompterFailsGracefullyOffTTY(t *testing.T) {
	// In test contexts there's no controlling terminal open, so
	// calling the default prompter should fail with a clear error
	// rather than hanging or panicking. We exercise this by calling
	// EnsureVault with prompter=nil and checking that we get the
	// expected "cannot open terminal" error.
	if _, err := EnsureVault(filepath.Join(t.TempDir(), "secrets.json"), nil, &bytes.Buffer{}, 1); err == nil {
		t.Error("expected error from default prompter without a tty")
	}
}

// erroringPrompter returns the supplied error on every call.
func erroringPrompter(err error) PassphrasePrompter {
	return func(_ string, _ io.Writer) (string, error) { return "", err }
}

// errorOnNthPrompter returns ok answers until the n-th call, then errors.
func errorOnNthPrompter(n int, errVal error, answers ...string) PassphrasePrompter {
	idx := 0
	return func(_ string, _ io.Writer) (string, error) {
		idx++
		if idx == n {
			return "", errVal
		}
		if idx-1 < len(answers) {
			return answers[idx-1], nil
		}
		return "", fmt.Errorf("script exhausted at index %d", idx)
	}
}

func TestEnsureVault_FirstRun_PrompterErrorOnFirstCallPropagates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	myErr := errors.New("interrupted")
	_, err := EnsureVault(path, erroringPrompter(myErr), &bytes.Buffer{}, 3)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("err = %v, want wrapped 'interrupted'", err)
	}
}

func TestEnsureVault_FirstRun_PrompterErrorOnConfirmPropagates(t *testing.T) {
	// Fail on the SECOND call (the confirm prompt).
	path := filepath.Join(t.TempDir(), "secrets.json")
	myErr := errors.New("eof on confirm")
	_, err := EnsureVault(path, errorOnNthPrompter(2, myErr, "first", "ignored"), &bytes.Buffer{}, 3)
	if err == nil || !strings.Contains(err.Error(), "eof on confirm") {
		t.Errorf("err = %v, want confirm-prompt error", err)
	}
}

func TestEnsureVault_FirstRun_EmptyPassphraseAborts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	_, err := EnsureVault(path, scripted("", ""), &bytes.Buffer{}, 3)
	if err == nil || !strings.Contains(err.Error(), "cannot be empty") {
		t.Errorf("err = %v, want empty-passphrase abort", err)
	}
}

func TestEnsureVault_ExistingVault_PrompterErrorPropagates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	myErr := errors.New("read fail")
	_, err := EnsureVault(path, erroringPrompter(myErr), &bytes.Buffer{}, 3)
	if err == nil || !strings.Contains(err.Error(), "read fail") {
		t.Errorf("err = %v, want wrapped 'read fail'", err)
	}
}

func TestEnsureVault_ExistingVault_GenericUnlockErrorPropagates(t *testing.T) {
	// An empty passphrase makes vault.Unlock return a non-sentinel
	// error ("passphrase cannot be empty"), which the retry loop
	// surfaces immediately rather than retrying.
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	called := 0
	prompter := func(_ string, _ io.Writer) (string, error) {
		called++
		return "", nil // empty passphrase → generic Unlock error
	}
	_, err := EnsureVault(path, prompter, &bytes.Buffer{}, 5)
	if err == nil {
		t.Error("expected error")
	}
	if called > 1 {
		t.Errorf("prompter called %d times; non-sentinel error must not retry", called)
	}
}

func TestEnsureVault_MaxRetriesLessThanOneClampsToDefault(t *testing.T) {
	// maxRetries = 0 should clamp to 3, so three "wrong" attempts
	// fail rather than zero attempts succeeding artificially.
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "right"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	called := 0
	prompter := func(_ string, _ io.Writer) (string, error) {
		called++
		return "wrong", nil
	}
	_, err := EnsureVault(path, prompter, &bytes.Buffer{}, 0)
	if err == nil {
		t.Error("expected error")
	}
	if called != 3 {
		t.Errorf("prompter called %d times; default maxRetries should be 3", called)
	}
}
