package hostimport

import (
	"bytes"
	"errors"
	"os/exec"
	"testing"
)

// stubSecurity swaps securityRunner for the duration of a test and
// restores it afterward, so the keychain output-parsing logic can be
// exercised on every platform without a real macOS Keychain (the CI
// macOS runner cannot grant the non-interactive Keychain access a real
// read requires).
func stubSecurity(t *testing.T, fn func(args ...string) ([]byte, error)) {
	t.Helper()
	prev := securityRunner
	securityRunner = fn
	t.Cleanup(func() { securityRunner = prev })
}

// TestReadKeychain_TrimsTrailingNewline confirms the single trailing
// newline `security -w` appends is stripped while inner bytes are
// preserved verbatim.
func TestReadKeychain_TrimsTrailingNewline(t *testing.T) {
	payload := []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`)
	var gotArgs []string
	stubSecurity(t, func(args ...string) ([]byte, error) {
		gotArgs = args
		return append(append([]byte{}, payload...), '\n'), nil
	})

	got, err := readKeychain("svc", "")
	if err != nil {
		t.Fatalf("readKeychain: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("readKeychain = %q, want %q (one trailing \\n trimmed)", got, payload)
	}
	want := []string{"find-generic-password", "-s", "svc", "-w"}
	if len(gotArgs) != len(want) {
		t.Fatalf("args = %v, want %v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args = %v, want %v", gotArgs, want)
		}
	}
}

// TestReadKeychain_AppendsKeychainPath confirms a non-empty keychain
// path is appended so reads target a throwaway keychain.
func TestReadKeychain_AppendsKeychainPath(t *testing.T) {
	var gotArgs []string
	stubSecurity(t, func(args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("x\n"), nil
	})
	if _, err := readKeychain("svc", "/tmp/test.keychain"); err != nil {
		t.Fatalf("readKeychain: %v", err)
	}
	if len(gotArgs) == 0 || gotArgs[len(gotArgs)-1] != "/tmp/test.keychain" {
		t.Errorf("args = %v, want keychain path appended last", gotArgs)
	}
}

// TestReadKeychain_NonZeroExitIsNotAuthenticated maps a `security`
// non-zero exit (absent item) to ErrNotAuthenticated.
func TestReadKeychain_NonZeroExitIsNotAuthenticated(t *testing.T) {
	// A real *exec.ExitError from a guaranteed-failing command.
	exitErr := exec.Command("/usr/bin/false").Run()
	if exitErr == nil {
		// /usr/bin/false missing (unusual); synthesize via a command
		// that exits non-zero.
		exitErr = exec.Command("/bin/sh", "-c", "exit 44").Run()
	}
	stubSecurity(t, func(args ...string) ([]byte, error) {
		return nil, exitErr
	})
	_, err := readKeychain("svc", "")
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("readKeychain (exit err): err = %v, want ErrNotAuthenticated", err)
	}
}

// TestReadKeychain_NonExitErrorPropagates confirms a non-exit error
// (e.g. the security binary missing) propagates rather than being
// masked as ErrNotAuthenticated.
func TestReadKeychain_NonExitErrorPropagates(t *testing.T) {
	sentinel := errors.New("exec failed: file not found")
	stubSecurity(t, func(args ...string) ([]byte, error) {
		return nil, sentinel
	})
	_, err := readKeychain("svc", "")
	if !errors.Is(err, sentinel) {
		t.Errorf("readKeychain = %v, want the wrapped exec error", err)
	}
	if errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("readKeychain masked a non-exit error as ErrNotAuthenticated")
	}
}

// TestReadKeychain_EmptyOutputIsNotAuthenticated maps an empty (or
// newline-only) password to ErrNotAuthenticated.
func TestReadKeychain_EmptyOutputIsNotAuthenticated(t *testing.T) {
	stubSecurity(t, func(args ...string) ([]byte, error) {
		return []byte("\n"), nil
	})
	_, err := readKeychain("svc", "")
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("readKeychain (empty): err = %v, want ErrNotAuthenticated", err)
	}
}
