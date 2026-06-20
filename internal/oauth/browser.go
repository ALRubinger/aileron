package oauth

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Opener opens a URL in the user's default browser. The default
// implementation [SystemBrowser] dispatches to the platform-native
// command (`open` on macOS, `xdg-open` on Linux, `start` via cmd.exe
// on Windows). Tests substitute their own Opener.
type Opener interface {
	Open(url string) error
}

// SystemBrowser is the default Opener. The zero value is ready to use.
type SystemBrowser struct{}

// Open dispatches to the platform's native "open URL" command. Returns
// an error if the platform is unsupported or the spawned process
// fails.
func (SystemBrowser) Open(url string) error {
	cmd, err := browserCommand(url)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("oauth: open browser: %w", err)
	}
	// Don't Wait() — we don't want to block on the browser process.
	// The caller's flow continues once the user completes consent in
	// the opened browser.
	go func() { _ = cmd.Wait() }()
	return nil
}

// browserCommand returns an unexec'd *exec.Cmd that opens url in the
// platform's default browser, delegating the per-GOOS argv decision to
// the pure [browserArgv] helper. The error branch is exercised by
// browserArgv's table test; browserCommand itself always passes the
// running platform's GOOS, so on a supported target the error is
// unreachable here.
func browserCommand(url string) (*exec.Cmd, error) {
	argv, err := browserArgv(runtime.GOOS, url)
	if err != nil {
		return nil, err
	}
	return exec.Command(argv[0], argv[1:]...), nil
}

// browserArgv returns the platform-native argv that opens url in the
// default browser for the given GOOS, or an error for an unsupported
// platform. It is a pure function of (goos, url) so the per-platform
// invocation contract is testable for every supported target
// regardless of where the test process runs. The Windows argv in
// particular must open the browser on the HOST with no terminal
// interaction, which is the no-container-TTY guarantee the host-side
// acquirer relies on.
func browserArgv(goos, url string) ([]string, error) {
	switch goos {
	case "darwin":
		return []string{"open", url}, nil
	case "linux":
		return []string{"xdg-open", url}, nil
	case "windows":
		// `cmd /c start "" <url>` — the empty title argument prevents
		// `start` from interpreting a quoted URL as a window title.
		//
		// cmd.exe parses the command line itself and treats unescaped
		// metacharacters (notably `&`, which separates commands) as
		// shell syntax, so an OAuth authorize URL like
		// `…?code=true&client_id=…&state=…` would be truncated at the
		// first `&` and the browser would receive only the leading
		// parameter. Escape the metacharacters with a caret so the full
		// URL — every query parameter — survives to `start`.
		return []string{"cmd", "/c", "start", "", escapeURLForWindowsCmd(url)}, nil
	default:
		return nil, fmt.Errorf("oauth: no browser-open command for GOOS=%q", goos)
	}
}

// escapeURLForWindowsCmd caret-escapes the cmd.exe metacharacters in s
// so a URL handed to `cmd /c start "" <url>` reaches the browser intact
// rather than being truncated or mangled by cmd's command-line parser.
//
// The load-bearing case is `&` (cmd's command separator): without
// escaping it, every query parameter after the first is dropped. For
// safety the full set of cmd metacharacters is escaped — `^` itself
// (escaped first so the carets we add aren't re-escaped), `&`, `|`,
// `<`, `>`, `(`, `)`, and `%` (which `start` would otherwise treat as
// the start of a `%VAR%` expansion). macOS/Linux openers pass the URL
// as a single argv element with no shell, so this is Windows-only.
func escapeURLForWindowsCmd(s string) string {
	// `^` must be replaced first; otherwise the carets introduced for
	// the other metacharacters would themselves be doubled.
	replacer := strings.NewReplacer(
		"^", "^^",
		"&", "^&",
		"|", "^|",
		"<", "^<",
		">", "^>",
		"(", "^(",
		")", "^)",
		"%", "^%",
	)
	return replacer.Replace(s)
}
