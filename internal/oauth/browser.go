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
	cmd, err := browserCommand(url)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("oauth: open browser: %w", err)
	}
	// Don't Wait() — we don't want to block on the browser process.
	// The caller's flow continues with the loopback listener.
	go func() { _ = cmd.Wait() }()
	return nil
}

// browserCommand returns an unexec'd *exec.Cmd that opens url in the
// platform's default browser. Split out so tests can verify the
// command shape without spawning a real browser.
func browserCommand(url string) (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url), nil
	case "linux":
		return exec.Command("xdg-open", url), nil
	case "windows":
		// `cmd /c start "" <url>` — the empty title argument prevents
		// `start` from interpreting a quoted URL as a window title.
		return exec.Command("cmd", "/c", "start", "", url), nil
	default:
		return nil, fmt.Errorf("oauth: no browser-open command for GOOS=%q", runtime.GOOS)
	}
}
