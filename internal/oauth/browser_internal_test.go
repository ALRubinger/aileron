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

// TestBrowserArgv_PerPlatformContract pins the exact argv each
// supported platform produces, for every GOOS, regardless of where
// this test runs. The Windows row is load-bearing: it must open the
// browser on the host with no terminal interaction (the
// no-container-TTY guarantee the host-side acquirer relies on), and
// the empty title argument is what stops `start` from treating a
// quoted URL as a window title. CI runs on Linux, so without this
// table the Windows and macOS argv shapes would never be asserted.
//
// True end-to-end Windows verification (a real browser window opening
// and the code paste landing on the host terminal) is not automatable
// here; it is the manual v4-acceptance step.
func TestBrowserArgv_PerPlatformContract(t *testing.T) {
	const url = "https://example.test/oauth/authorize"
	tests := []struct {
		goos    string
		want    []string
		wantErr bool
	}{
		{goos: "darwin", want: []string{"open", url}},
		{goos: "linux", want: []string{"xdg-open", url}},
		{goos: "windows", want: []string{"cmd", "/c", "start", "", url}},
		{goos: "plan9", wantErr: true},
		{goos: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			argv, err := browserArgv(tc.goos, url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("browserArgv(%q) = %v, nil; want error", tc.goos, argv)
				}
				if argv != nil {
					t.Errorf("browserArgv(%q) returned argv %v alongside error", tc.goos, argv)
				}
				return
			}
			if err != nil {
				t.Fatalf("browserArgv(%q): unexpected error %v", tc.goos, err)
			}
			if !equalArgv(argv, tc.want) {
				t.Errorf("browserArgv(%q) = %v; want %v", tc.goos, argv, tc.want)
			}
		})
	}
}

func equalArgv(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
