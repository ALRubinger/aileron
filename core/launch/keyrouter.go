package launch

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// ctrlA is the byte value for Ctrl-A in raw terminal mode.
	ctrlA = 0x01
	// doubleTapTimeout is the window for a second Ctrl-A to pass through
	// as a literal byte (like tmux prefix handling).
	doubleTapTimeout = 500 * time.Millisecond
)

// OverlayController is the interface the key router uses to activate
// and deactivate the overlay. This avoids a circular dependency between
// the key router and the overlay.
type OverlayController interface {
	Show()
	Hide()
	HandleKey(b byte)
	IsActive() bool
}

// KeyRouter reads from stdin byte-by-byte and routes keystrokes to
// either the pty (normal mode) or the overlay (overlay mode). Ctrl-A
// toggles overlay mode. Double Ctrl-A within 500ms sends a literal
// Ctrl-A to the pty.
type KeyRouter struct {
	stdin   io.Reader
	ptmx    io.Writer
	overlay OverlayController

	overlayActive atomic.Bool

	mu           sync.Mutex
	pendingCtrlA bool
	ctrlATimer   *time.Timer
}

// NewKeyRouter creates a key router that reads from stdin and writes
// to the pty master. The overlay controller handles show/hide/keypress.
func NewKeyRouter(stdin io.Reader, ptmx io.Writer, overlay OverlayController) *KeyRouter {
	return &KeyRouter{
		stdin:   stdin,
		ptmx:    ptmx,
		overlay: overlay,
	}
}

// Run reads from stdin and routes each byte. This blocks until stdin
// returns an error or EOF. Call in a goroutine.
func (kr *KeyRouter) Run() {
	buf := make([]byte, 1)
	for {
		n, err := kr.stdin.Read(buf)
		if n == 0 || err != nil {
			return
		}
		b := buf[0]

		if kr.overlayActive.Load() {
			kr.overlay.HandleKey(b)
			continue
		}

		if b == ctrlA {
			kr.handleCtrlA()
			continue
		}

		kr.mu.Lock()
		if kr.pendingCtrlA {
			// Non-Ctrl-A byte after a pending Ctrl-A — activate overlay
			// and forward this byte to the overlay.
			kr.pendingCtrlA = false
			if kr.ctrlATimer != nil {
				kr.ctrlATimer.Stop()
			}
			kr.mu.Unlock()
			kr.activateOverlay()
			kr.overlay.HandleKey(b)
			continue
		}
		kr.mu.Unlock()

		kr.ptmx.Write(buf[:n])
	}
}

func (kr *KeyRouter) handleCtrlA() {
	kr.mu.Lock()
	defer kr.mu.Unlock()

	if kr.pendingCtrlA {
		// Double Ctrl-A — send literal Ctrl-A to pty.
		kr.pendingCtrlA = false
		if kr.ctrlATimer != nil {
			kr.ctrlATimer.Stop()
		}
		kr.ptmx.Write([]byte{ctrlA})
		return
	}

	// First Ctrl-A — start the timer.
	kr.pendingCtrlA = true
	kr.ctrlATimer = time.AfterFunc(doubleTapTimeout, func() {
		kr.mu.Lock()
		pending := kr.pendingCtrlA
		kr.pendingCtrlA = false
		kr.mu.Unlock()

		if pending {
			kr.activateOverlay()
		}
	})
}

func (kr *KeyRouter) activateOverlay() {
	kr.overlayActive.Store(true)
	kr.overlay.Show()
}

// DeactivateOverlay is called by the overlay when the user presses
// Escape. It switches the key router back to pty mode.
func (kr *KeyRouter) DeactivateOverlay() {
	kr.overlayActive.Store(false)
	kr.overlay.Hide()
}
