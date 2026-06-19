package oauth

import (
	"runtime"
	"strings"
	"testing"
)

// TestBrowserArgv_RunningPlatform exercises the argv-build path for the
// platform the test process actually runs on, without spawning a
// process. We can't portably guarantee the platform's
// `open`/`xdg-open`/`start` is installed in the test environment, and
// even when it is, spawning it would launch a real browser window
// during `go test`. So we inspect the returned argv instead. The
// exhaustive per-GOOS contract (including the platforms this runner is
// not) is in TestBrowserArgv_PerPlatformContract below.
func TestBrowserArgv_RunningPlatform(t *testing.T) {
	const url = "https://example.test/oauth/authorize"
	argv, err := browserArgv(runtime.GOOS, url)
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		if err != nil {
			t.Fatalf("browserArgv on %s: %v", runtime.GOOS, err)
		}
		if len(argv) == 0 {
			t.Fatal("argv is empty")
		}
		// The URL must appear in the args verbatim, regardless of
		// platform.
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, url) {
			t.Errorf("argv = %v; want URL %q in arglist", argv, url)
		}
		// Platform-specific spot-check.
		switch runtime.GOOS {
		case "darwin":
			if argv[0] != "open" {
				t.Errorf("darwin command = %q, want open", argv[0])
			}
		case "linux":
			if argv[0] != "xdg-open" {
				t.Errorf("linux command = %q, want xdg-open", argv[0])
			}
		case "windows":
			if argv[0] != "cmd" {
				t.Errorf("windows command = %q, want cmd", argv[0])
			}
		}
		// browserCommand wraps the same argv into an unexec'd *exec.Cmd.
		// Assert it builds (without Start) so the wrapper's happy path is
		// covered; the spawn itself stays out of the test to avoid
		// launching a real browser window.
		cmd, cmdErr := browserCommand(url)
		if cmdErr != nil {
			t.Fatalf("browserCommand on %s: %v", runtime.GOOS, cmdErr)
		}
		if cmd == nil || len(cmd.Args) == 0 || cmd.Args[0] != argv[0] {
			t.Errorf("browserCommand args = %v; want first arg %q", cmd.Args, argv[0])
		}
	default:
		// Unsupported GOOS should report an error.
		if err == nil {
			t.Errorf("browserArgv on %s should return an error", runtime.GOOS)
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
