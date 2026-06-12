package hostimport

import (
	"bytes"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
)

// exitWithCode returns a real *exec.ExitError for the given exit code,
// portably across Unix and Windows, so tests can drive readKeychain's
// exit-code branch without a real `security` binary.
func exitWithCode(t *testing.T, code int) *exec.ExitError {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit "+strconv.Itoa(code))
	} else {
		cmd = exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Skipf("could not produce an *exec.ExitError on %s: %v", runtime.GOOS, err)
	}
	return exitErr
}

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

// TestReadKeychain_ItemNotFoundIsNotAuthenticated maps the absent-item
// exit code (44) to ErrNotAuthenticated.
func TestReadKeychain_ItemNotFoundIsNotAuthenticated(t *testing.T) {
	exitErr := exitWithCode(t, keychainItemNotFoundExit)
	stubSecurity(t, func(args ...string) ([]byte, error) {
		return nil, exitErr
	})
	_, err := readKeychain("svc", "")
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("readKeychain (exit 44): err = %v, want ErrNotAuthenticated", err)
	}
}

// TestReadKeychain_NotFoundStderrIsNotAuthenticated maps a non-44 exit
// whose stderr says the item "could not be found" to ErrNotAuthenticated,
// covering the stderr-substring fallback.
func TestReadKeychain_NotFoundStderrIsNotAuthenticated(t *testing.T) {
	exitErr := exitWithCode(t, 1)
	exitErr.Stderr = []byte("security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.")
	stubSecurity(t, func(args ...string) ([]byte, error) {
		return nil, exitErr
	})
	_, err := readKeychain("svc", "")
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("readKeychain (not-found stderr): err = %v, want ErrNotAuthenticated", err)
	}
}

// TestReadKeychain_OtherExitPropagates confirms a non-44 exit without a
// not-found stderr (e.g. a locked keychain or permission denial) is NOT
// masked as ErrNotAuthenticated; the operator must see the real failure.
func TestReadKeychain_OtherExitPropagates(t *testing.T) {
	exitErr := exitWithCode(t, 51) // errSecInteractionNotAllowed-style failure
	exitErr.Stderr = []byte("security: User interaction is not allowed.")
	stubSecurity(t, func(args ...string) ([]byte, error) {
		return nil, exitErr
	})
	_, err := readKeychain("svc", "")
	if errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("readKeychain masked a non-not-found exit as ErrNotAuthenticated: %v", err)
	}
	if err == nil {
		t.Error("readKeychain: want the real exit error, got nil")
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
