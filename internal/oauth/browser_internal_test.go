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

// TestBrowserArgv_WindowsEscapesCmdMetacharacters is the regression
// test for the host-acquire OAuth bug: the Claude authorize URL is
// `base?code=true&client_id=…&code_challenge=…&state=…`, and on Windows
// `cmd /c start "" <url>` truncates it at the first `&` because cmd
// treats `&` as a command separator. The Windows argv must caret-escape
// the metacharacters so every parameter survives; the macOS/Linux argv
// must pass the URL byte-for-byte unchanged (no shell, so no escaping).
func TestBrowserArgv_WindowsEscapesCmdMetacharacters(t *testing.T) {
	const url = "https://claude.ai/oauth/authorize?code=true&client_id=abc123&code_challenge=xYz_chal&state=st-987&scope=read%20write"

	// Windows: the final argv element is the escaped URL, and it must
	// preserve every parameter (the bug dropped everything after the
	// first `&`).
	t.Run("windows", func(t *testing.T) {
		argv, err := browserArgv("windows", url)
		if err != nil {
			t.Fatalf("browserArgv(windows): %v", err)
		}
		want := []string{"cmd", "/c", "start", "",
			"https://claude.ai/oauth/authorize?code=true^&client_id=abc123^&code_challenge=xYz_chal^&state=st-987^&scope=read^%20write"}
		if !equalArgv(argv, want) {
			t.Fatalf("browserArgv(windows) = %v;\nwant %v", argv, want)
		}
		escaped := argv[len(argv)-1]
		// Every parameter must survive (the bug truncated at the first &).
		for _, frag := range []string{"client_id=abc123", "code_challenge=xYz_chal", "state=st-987"} {
			if !strings.Contains(escaped, frag) {
				t.Errorf("escaped URL %q missing %q", escaped, frag)
			}
		}
		// The `&` separators must be caret-escaped, not raw.
		if strings.Contains(escaped, "true&client_id") {
			t.Errorf("escaped URL %q still has an unescaped & before client_id", escaped)
		}
		if strings.Count(escaped, "^&") != 4 {
			t.Errorf("escaped URL %q: want 4 caret-escaped ampersands, got %d", escaped, strings.Count(escaped, "^&"))
		}
	})

	// macOS/Linux: single argv element, URL byte-identical to input.
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			argv, err := browserArgv(goos, url)
			if err != nil {
				t.Fatalf("browserArgv(%s): %v", goos, err)
			}
			if argv[len(argv)-1] != url {
				t.Errorf("browserArgv(%s) URL = %q; want byte-identical %q", goos, argv[len(argv)-1], url)
			}
		})
	}
}

// TestEscapeURLForWindowsCmd pins the per-character escaping contract,
// including the `^`-first ordering (so an existing caret doesn't get the
// carets we add re-escaped) and the no-op case for URLs with no
// metacharacters.
func TestEscapeURLForWindowsCmd(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain URL unchanged", "https://example.test/path", "https://example.test/path"},
		{"ampersand", "a&b", "a^&b"},
		{"all metacharacters", `&|<>()%`, `^&^|^<^>^(^)^%`},
		{"caret escaped first", "a^&b", "a^^^&b"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := escapeURLForWindowsCmd(c.in); got != c.want {
				t.Errorf("escapeURLForWindowsCmd(%q) = %q; want %q", c.in, got, c.want)
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
