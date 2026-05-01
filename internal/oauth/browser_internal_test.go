package oauth

import (
	"runtime"
	"strings"
	"testing"
)

// TestBrowserCommand_BuildsPlatformInvocation exercises the
// command-build path without actually starting a process. We can't
// portably guarantee the platform's `open`/`xdg-open`/`start` is
// installed in the test environment, and even when it is, spawning
// it would launch a real browser window during `go test`. So we
// inspect the returned *exec.Cmd shape instead.
func TestBrowserCommand_BuildsPlatformInvocation(t *testing.T) {
	const url = "https://example.test/oauth/authorize"
	cmd, err := browserCommand(url)
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		if err != nil {
			t.Fatalf("browserCommand on %s: %v", runtime.GOOS, err)
		}
		if cmd == nil {
			t.Fatal("cmd is nil")
		}
		// The URL must appear in the args verbatim, regardless of
		// platform.
		joined := strings.Join(cmd.Args, " ")
		if !strings.Contains(joined, url) {
			t.Errorf("cmd.Args = %v; want URL %q in arglist", cmd.Args, url)
		}
		// Platform-specific spot-check.
		switch runtime.GOOS {
		case "darwin":
			if cmd.Args[0] != "open" {
				t.Errorf("darwin command = %q, want open", cmd.Args[0])
			}
		case "linux":
			if cmd.Args[0] != "xdg-open" {
				t.Errorf("linux command = %q, want xdg-open", cmd.Args[0])
			}
		case "windows":
			if cmd.Args[0] != "cmd" {
				t.Errorf("windows command = %q, want cmd", cmd.Args[0])
			}
		}
	default:
		// Unsupported GOOS should report an error.
		if err == nil {
			t.Errorf("browserCommand on %s should return an error", runtime.GOOS)
		}
	}
}
