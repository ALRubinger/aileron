package main

import (
	"errors"
	"io/fs"
	"testing"
)

// TestOpenControllingTerminal asserts that the controlling-terminal opener
// resolves the correct platform device. The regression this guards is
// feedback #1250: on Windows the passphrase prompt hardcoded the POSIX
// device /dev/tty, which does not exist, so first-run (runVaultInit ->
// defaultPromptPassphrase) crashed with
// "open /dev/tty: The system cannot find the path specified" — a
// path-not-found (fs.ErrNotExist) error.
//
// On a host with a controlling terminal the opener succeeds and returns a
// handle whose Fd() is usable by golang.org/x/term. In CI / non-interactive
// contexts there may be no terminal, so the open can fail — but it must fail
// because the device is unavailable, never because the path does not exist.
// A fs.ErrNotExist here means we asked the OS for the wrong device name,
// which is exactly the bug being fixed.
func TestOpenControllingTerminal(t *testing.T) {
	f, err := openControllingTerminal()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("openControllingTerminal returned path-not-found %v; the platform terminal device name is wrong", err)
		}
		// No controlling terminal in this environment (e.g. CI). The
		// device name was resolved correctly; the open just failed
		// because no terminal is attached. That is acceptable.
		t.Skipf("no controlling terminal available in this environment: %v", err)
	}
	defer f.Close()

	if f.Fd() == 0 && f.Name() == "" {
		t.Fatalf("openControllingTerminal returned an unusable handle")
	}
}
