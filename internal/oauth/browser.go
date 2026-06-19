package oauth

import (
	"fmt"
	"os/exec"
	"runtime"
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
	argv, err := browserArgv(runtime.GOOS, url)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("oauth: open browser: %w", err)
	}
	// Don't Wait() — we don't want to block on the browser process.
	// The caller's flow continues once the user completes consent in
	// the opened browser.
	go func() { _ = cmd.Wait() }()
	return nil
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
		return []string{"cmd", "/c", "start", "", url}, nil
	default:
		return nil, fmt.Errorf("oauth: no browser-open command for GOOS=%q", goos)
	}
}
