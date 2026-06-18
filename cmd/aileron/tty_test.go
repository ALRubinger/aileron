package main

import (
	"bytes"
	"errors"
	"io/fs"
	"strings"
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

// TestDefaultPromptPassphrase_NoTerminalFailsGracefully exercises the real
// default prompt (not the injectable `promptPassphrase` seam) when no
// controlling terminal is available. The contract is that it surfaces a
// clear "cannot open terminal" error rather than hanging or panicking — the
// same fallback a user hits running non-interactively. We guard against a
// hang by only running when the terminal is genuinely absent; if a tty is
// attached, term.ReadPassword would block, so we skip.
func TestDefaultPromptPassphrase_NoTerminalFailsGracefully(t *testing.T) {
	if f, err := openControllingTerminal(); err == nil {
		f.Close()
		t.Skip("controlling terminal present; skipping to avoid blocking on read")
	}
	var buf bytes.Buffer
	if _, err := defaultPromptPassphrase("passphrase: ", &buf); err == nil {
		t.Fatal("expected error when no controlling terminal is available")
	} else if !strings.Contains(err.Error(), "cannot open terminal") {
		t.Fatalf("err = %v, want 'cannot open terminal'", err)
	}
}
