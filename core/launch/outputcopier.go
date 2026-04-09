package launch

import (
	"io"
	"os"
	"sync"
	"time"
)

const maxAgentBuffer = 1 << 20 // 1 MB

// OutputCopier reads from the pty master and writes to stdout. When an
// overlay is active, output is buffered instead of displayed. The
// buffer is flushed when the overlay is dismissed.
// idleTimeout is how long output must be quiet before re-rendering
// the status bar. Matches the behavior of Claude Code's own footer.
const idleTimeout = 150 * time.Millisecond

type OutputCopier struct {
	src io.Reader
	dst io.Writer
	// overlay is set after construction when there's a circular dependency
	// (overlay needs copier, copier needs overlay).
	overlay OverlayController
	// pauseFile is checked on each read iteration. When it exists, output
	// is buffered. This allows aileron-sh (a separate process) to pause
	// pty output while showing an approval prompt on /dev/tty.
	pauseFile string
	// onIdle is called when output has been quiet for idleTimeout.
	// Used to re-render the status bar after agent output settles.
	onIdle func()

	mu        sync.Mutex
	buf       []byte
	paused    bool
	idleTimer *time.Timer
}

// SetOverlay sets the overlay controller. Call before Run.
func (oc *OutputCopier) SetOverlay(o OverlayController) {
	oc.overlay = o
}

// SetPauseFile sets the path to check for pause signaling. Call before Run.
func (oc *OutputCopier) SetPauseFile(path string) {
	oc.pauseFile = path
}

// SetOnIdle sets a callback that fires when output has been quiet for
// idleTimeout. Used to re-render the status bar after scrolling stops.
func (oc *OutputCopier) SetOnIdle(fn func()) {
	oc.onIdle = fn
}

// SetPaused programmatically pauses or resumes output. When paused,
// output is buffered. When unpaused, buffered output is flushed.
func (oc *OutputCopier) SetPaused(p bool) {
	oc.mu.Lock()
	oc.paused = p
	oc.mu.Unlock()
	if !p {
		oc.Flush()
	}
}

// NewOutputCopier creates an output copier that routes pty output to
// either the real terminal or a buffer depending on overlay state.
func NewOutputCopier(src io.Reader, dst io.Writer, overlay OverlayController) *OutputCopier {
	return &OutputCopier{
		src:     src,
		dst:     dst,
		overlay: overlay,
	}
}

// Run reads from the pty master and writes to the appropriate
// destination. Blocks until the source returns an error or EOF.
// Call in a goroutine.
func (oc *OutputCopier) Run() {
	buf := make([]byte, 4096)
	for {
		n, err := oc.src.Read(buf)
		if n > 0 {
			if oc.shouldPause() {
				oc.bufferOutput(buf[:n])
			} else {
				oc.dst.Write(buf[:n])
				oc.resetIdleTimer()
			}
		}
		if err != nil {
			return
		}
	}
}

// shouldPause returns true if output should be buffered — either
// because the overlay is active or the pause file exists.
func (oc *OutputCopier) shouldPause() bool {
	if oc.overlay != nil && oc.overlay.IsActive() {
		return true
	}
	oc.mu.Lock()
	paused := oc.paused
	oc.mu.Unlock()
	if paused {
		return true
	}
	if oc.pauseFile != "" {
		if _, err := os.Stat(oc.pauseFile); err == nil {
			return true
		}
	}
	return false
}

func (oc *OutputCopier) resetIdleTimer() {
	if oc.onIdle == nil {
		return
	}
	oc.mu.Lock()
	if oc.idleTimer != nil {
		oc.idleTimer.Stop()
	}
	oc.idleTimer = time.AfterFunc(idleTimeout, oc.onIdle)
	oc.mu.Unlock()
}

func (oc *OutputCopier) bufferOutput(data []byte) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	oc.buf = append(oc.buf, data...)
	// Bound the buffer to prevent unbounded growth.
	if len(oc.buf) > maxAgentBuffer {
		oc.buf = oc.buf[len(oc.buf)-maxAgentBuffer:]
	}
}

// Flush writes any buffered agent output to the real terminal and
// clears the buffer. Called when the overlay is dismissed.
func (oc *OutputCopier) Flush() {
	oc.mu.Lock()
	data := oc.buf
	oc.buf = nil
	oc.mu.Unlock()

	if len(data) > 0 {
		oc.dst.Write(data)
	}
}
