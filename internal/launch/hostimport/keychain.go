package hostimport

import (
	"bytes"
	"errors"
	"os/exec"
)

// securityRunner runs the macOS `security` tool and returns its stdout.
// It is a package var so tests on every platform can stub the
// shell-out and exercise the output-parsing logic in readKeychain
// without a real Keychain (the GitHub macOS runner cannot grant the
// non-interactive Keychain access these reads require). Production binds
// it to runSecurity, which execs /usr/bin/security.
var securityRunner = runSecurity

// runSecurity execs `/usr/bin/security <args...>` and returns its
// stdout. A non-zero exit (the item is absent: security exits 44,
// "could not be found") is reported as an error the caller maps to
// ErrNotAuthenticated.
func runSecurity(args ...string) ([]byte, error) {
	cmd := exec.Command("/usr/bin/security", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Surface the captured stderr on the ExitError so callers can
		// distinguish "item not found" from other access failures.
		// cmd.Run leaves ExitError.Stderr nil when cmd.Stderr is set, so
		// we attach it here.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitErr.Stderr = stderr.Bytes()
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// keychainItemNotFoundExit is the exit code `security
// find-generic-password` returns when the requested item does not exist
// (errSecItemNotFound surfaces as exit 44). It is the single exit code
// that means "the credential is simply absent" as opposed to a real
// keychain access failure.
const keychainItemNotFoundExit = 44

// isKeychainItemNotFound reports whether a `security` ExitError means
// the item was absent (exit 44) rather than a genuine access failure
// such as a locked keychain or permission denial. It also accepts the
// "could not be found" stderr substring when present, so a future
// security release that changes the exit code still maps a clear
// not-found message to ErrNotAuthenticated.
func isKeychainItemNotFound(exitErr *exec.ExitError) bool {
	if exitErr.ExitCode() == keychainItemNotFoundExit {
		return true
	}
	return bytes.Contains(exitErr.Stderr, []byte("could not be found"))
}

// readKeychain reads a generic-password item's bytes via the injected
// securityRunner. keychainPath, when non-empty, is appended so the read
// targets a throwaway test keychain rather than the login keychain.
//
// The `-w` flag prints the password followed by a single trailing
// newline; that newline is trimmed so the stored credential bytes are
// exact. A non-zero `security` exit (absent item) maps to
// ErrNotAuthenticated, as does an empty password.
//
// readKeychain has no build tag so the parsing/error-mapping logic is
// unit-testable on every platform through the securityRunner seam; the
// real exec (runSecurity) only ever fires on darwin via keychain_darwin.go.
func readKeychain(service, keychainPath string) ([]byte, error) {
	args := []string{"find-generic-password", "-s", service, "-w"}
	if keychainPath != "" {
		args = append(args, keychainPath)
	}
	out, err := securityRunner(args...)
	if err != nil {
		// `security` exits 44 ("could not be found") when the item is
		// absent; that is the only case that means "not authenticated".
		// Other non-zero exits (locked keychain, permission denied,
		// corrupted database) are real failures the operator must see,
		// so they propagate with the security stderr rather than being
		// masked as a missing credential.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && isKeychainItemNotFound(exitErr) {
			return nil, ErrNotAuthenticated
		}
		return nil, err
	}
	trimmed := bytes.TrimSuffix(out, []byte("\n"))
	if len(trimmed) == 0 {
		return nil, ErrNotAuthenticated
	}
	return trimmed, nil
}
