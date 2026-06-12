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
		return nil, err
	}
	return stdout.Bytes(), nil
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
		// security exits non-zero when the item is absent. Treat any
		// non-zero exit as "no usable credential" so the caller surfaces
		// the recovery path rather than a raw exec error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
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
