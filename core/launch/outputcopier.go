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
// DefaultIdleTimeout is how long output must be quiet before
// re-rendering the status bar. Needs to be long enough that normal
// output bursts (like ls listings) don't trigger mid-render redraws.
const DefaultIdleTimeout = 2 * time.Second

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
	onIdle      func()
	idleTimeout time.Duration

	// directWrite receives data that must be written to dst exclusively
	// by the copier goroutine — no concurrent writes.
	directWrite chan []byte

	mu        sync.Mutex
	buf       []byte
	paused    bool
	pausedCh  chan struct{} // closed when copier acknowledges pause
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
// the idle timeout. Used to re-render the status bar after scrolling stops.
func (oc *OutputCopier) SetOnIdle(fn func()) {
	oc.onIdle = fn
}

// SetIdleTimeout overrides the idle timeout duration. For testing.
func (oc *OutputCopier) SetIdleTimeout(d time.Duration) {
	oc.idleTimeout = d
}

// SetPaused programmatically pauses or resumes output. When pausing,
// blocks until the copier's current write cycle completes so there's
// no race with the caller switching terminal modes. When unpausing,
// buffered output is flushed.
func (oc *OutputCopier) SetPaused(p bool) {
	oc.mu.Lock()
	if p && !oc.paused {
		oc.pausedCh = make(chan struct{})
		oc.paused = true
		oc.mu.Unlock()
		// Wait for the copier to acknowledge, with a timeout in case
		// no data is flowing (the Read blocks).
		select {
		case <-oc.pausedCh:
		case <-time.After(100 * time.Millisecond):
		}
		return
	}
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
		src:         src,
		dst:         dst,
		overlay:     overlay,
		directWrite: make(chan []byte, 1),
		idleTimeout: DefaultIdleTimeout,
	}
}

// Run reads from the pty master and writes to the appropriate
// destination. Blocks until the source returns an error or EOF.
// Call in a goroutine.
// WriteExclusive sends data to be written to dst by the copier
// goroutine. This is the only safe way to write to dst from another
// goroutine — it guarantees no concurrent writes. Blocks until the
// copier processes the write.
func (oc *OutputCopier) WriteExclusive(data []byte) {
	cp := make([]byte, len(data))
	copy(cp, data)
	oc.directWrite <- cp
}

type readResult struct {
	data []byte
	err  error
}

func (oc *OutputCopier) Run() {
	// Read from pty in a separate goroutine so we can select between
	// pty data and direct writes without blocking.
	ptyCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := oc.src.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				ptyCh <- readResult{data: cp}
			}
			if err != nil {
				ptyCh <- readResult{err: err}
				return
			}
		}
	}()

	for {
		select {
		case r := <-ptyCh:
			if r.err != nil {
				return
			}
			if oc.shouldPause() {
				oc.bufferOutput(r.data)
				oc.ackPause()
			} else {
				oc.dst.Write(r.data)
				oc.resetIdleTimer()
			}
		case data := <-oc.directWrite:
			oc.dst.Write(data)
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

// ackPause signals SetPaused that the copier is now buffering.
func (oc *OutputCopier) ackPause() {
	oc.mu.Lock()
	if oc.pausedCh != nil {
		close(oc.pausedCh)
		oc.pausedCh = nil
	}
	oc.mu.Unlock()
}

func (oc *OutputCopier) resetIdleTimer() {
	if oc.onIdle == nil {
		return
	}
	oc.mu.Lock()
	if oc.idleTimer != nil {
		oc.idleTimer.Stop()
	}
	oc.idleTimer = time.AfterFunc(oc.idleTimeout, oc.onIdle)
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
