//go:build windows

package spawn

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestDetachProcAttrs_NoConsoleWindow is a regression test for #1383:
// the daemon was fork-exec'd with DETACHED_PROCESS, which let Windows
// allocate a lingering console window for the console-subsystem
// aileron-server binary. The fix uses CREATE_NO_WINDOW instead, which
// is documented to run a console application with no console window,
// while still composing with CREATE_NEW_PROCESS_GROUP for signal-group
// isolation.
func TestDetachProcAttrs_NoConsoleWindow(t *testing.T) {
	flags := detachProcAttrs().CreationFlags

	if flags&windows.CREATE_NO_WINDOW == 0 {
		t.Errorf("CreationFlags missing CREATE_NO_WINDOW: got %#x", flags)
	}
	if flags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Errorf("CreationFlags missing CREATE_NEW_PROCESS_GROUP: got %#x", flags)
	}
	if flags&windows.DETACHED_PROCESS != 0 {
		t.Errorf("CreationFlags still sets DETACHED_PROCESS (#1383 regression): got %#x", flags)
	}
}
