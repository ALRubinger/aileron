package launch

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// overlayPrefix is the traditional byte value for Ctrl-] in raw
	// terminal mode. Modern terminals using the kitty keyboard protocol
	// encode this as CSI 93;5u instead, which the router also recognises.
	overlayPrefix = 0x1D
	// overlayCodepoint is the Unicode codepoint for ']' (used in kitty
	// keyboard protocol CSI u sequences: ESC [ 93 ; <mod> u).
	overlayCodepoint = 93
	// doubleTapTimeout is the window for a second Ctrl-] to pass through
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
// either the pty (normal mode) or the overlay (overlay mode). Ctrl-]
// toggles overlay mode. Double Ctrl-] within 500ms sends a literal
// Ctrl-] to the pty.
//
// The router recognises Ctrl-] both as the traditional byte 0x1D and as
// the kitty keyboard protocol encoding ESC [ 93 ; <mod> u (where mod
// indicates Ctrl is held, i.e. mod & 4 != 0).
type KeyRouter struct {
	stdin   io.Reader
	ptmx    io.Writer
	overlay OverlayController

	overlayActive atomic.Bool

	mu           sync.Mutex
	pendingPrefix bool
	prefixTimer   *time.Timer

	// inputSink, when set, receives all stdin bytes instead of routing
	// them to the pty or overlay. Used by the approval server.
	inputSink chan byte

	// CSI u state machine for kitty keyboard protocol detection.
	// States: 0=normal, 1=got ESC, 2=got ESC+[, 3=accumulating codepoint
	// digits, 4=got semicolon, 5=accumulating modifier digits.
	csiState    int
	csiCP       int // accumulated codepoint
	csiMod      int // accumulated modifier
	csiBuf      []byte // buffered bytes for replay on mismatch
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

// StealInput redirects all stdin bytes to the returned channel.
// Call ReleaseInput to restore normal routing.
func (kr *KeyRouter) StealInput() <-chan byte {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	ch := make(chan byte, 16)
	kr.inputSink = ch
	return ch
}

// ReleaseInput restores normal key routing after StealInput.
func (kr *KeyRouter) ReleaseInput() {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	if kr.inputSink != nil {
		close(kr.inputSink)
		kr.inputSink = nil
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

		// If input is being stolen (approval prompt), send there.
		kr.mu.Lock()
		sink := kr.inputSink
		kr.mu.Unlock()
		if sink != nil {
			sink <- b
			continue
		}

		if kr.overlayActive.Load() {
			kr.overlay.HandleKey(b)
			continue
		}

		// Feed byte through the CSI u state machine. If it detects the
		// overlay prefix sequence, it calls handlePrefix and returns true.
		// If the sequence is incomplete, the byte is buffered and we
		// continue reading. If the sequence doesn't match, buffered bytes
		// are replayed to the normal routing path.
		if kr.feedCSIu(b) {
			continue
		}

		kr.routeByte(b)
	}
}

// routeByte handles a single byte through the normal (non-CSI-u) path:
// overlay prefix detection, pending-prefix handling, or passthrough to pty.
func (kr *KeyRouter) routeByte(b byte) {
	if b == overlayPrefix {
		kr.handlePrefix()
		return
	}

	kr.mu.Lock()
	if kr.pendingPrefix {
		// Non-prefix byte after a pending Ctrl-] — activate overlay
		// and forward this byte to the overlay.
		kr.pendingPrefix = false
		if kr.prefixTimer != nil {
			kr.prefixTimer.Stop()
		}
		kr.mu.Unlock()
		kr.activateOverlay()
		kr.overlay.HandleKey(b)
		return
	}
	kr.mu.Unlock()

	kr.ptmx.Write([]byte{b})
}

// feedCSIu advances the kitty keyboard protocol state machine. Returns
// true if the byte was consumed (buffered or matched). When a complete
// CSI u sequence matching the overlay prefix is detected, handlePrefix
// is called. On mismatch, all buffered bytes are replayed through
// routeByte.
func (kr *KeyRouter) feedCSIu(b byte) bool {
	switch kr.csiState {
	case 0: // normal
		if b == 0x1B {
			kr.csiState = 1
			kr.csiBuf = append(kr.csiBuf[:0], b)
			return true
		}
		return false

	case 1: // got ESC
		kr.csiBuf = append(kr.csiBuf, b)
		if b == '[' {
			kr.csiState = 2
			return true
		}
		// Not a CSI sequence — replay buffered bytes.
		kr.replayCSIBuf()
		return true

	case 2: // got ESC + [
		kr.csiBuf = append(kr.csiBuf, b)
		if b >= '0' && b <= '9' {
			kr.csiCP = int(b - '0')
			kr.csiState = 3
			return true
		}
		// Not a numeric codepoint — replay.
		kr.replayCSIBuf()
		return true

	case 3: // accumulating codepoint digits
		kr.csiBuf = append(kr.csiBuf, b)
		if b >= '0' && b <= '9' {
			kr.csiCP = kr.csiCP*10 + int(b-'0')
			return true
		}
		if b == ';' {
			kr.csiState = 4
			return true
		}
		if b == 'u' {
			// CSI <cp> u with no modifier — not Ctrl, replay.
			kr.replayCSIBuf()
			return true
		}
		// Not a CSI u sequence — replay.
		kr.replayCSIBuf()
		return true

	case 4: // got semicolon, expecting modifier digits
		kr.csiBuf = append(kr.csiBuf, b)
		if b >= '0' && b <= '9' {
			kr.csiMod = int(b - '0')
			kr.csiState = 5
			return true
		}
		// Not a digit after semicolon — replay.
		kr.replayCSIBuf()
		return true

	case 5: // accumulating modifier digits
		kr.csiBuf = append(kr.csiBuf, b)
		if b >= '0' && b <= '9' {
			kr.csiMod = kr.csiMod*10 + int(b-'0')
			return true
		}
		if b == 'u' {
			// Complete CSI <codepoint> ; <modifier> u sequence.
			// Modifier uses xterm encoding: value = 1 + bitmask.
			// Ctrl = bit 2 (value 4), so mod & 4 != 0 means Ctrl held.
			isCtrl := (kr.csiMod-1)&0x04 != 0
			cp := kr.csiCP
			if cp == overlayCodepoint && isCtrl {
				kr.resetCSI()
				kr.handlePrefix()
				return true
			}
			// Not our prefix — send the raw sequence to the pty.
			saved := make([]byte, len(kr.csiBuf))
			copy(saved, kr.csiBuf)
			kr.resetCSI()
			kr.ptmx.Write(saved)
			return true
		}
		// Unexpected terminator — replay.
		kr.replayCSIBuf()
		return true
	}

	return false
}

// replayCSIBuf replays all buffered CSI bytes through routeByte and resets state.
func (kr *KeyRouter) replayCSIBuf() {
	saved := make([]byte, len(kr.csiBuf))
	copy(saved, kr.csiBuf)
	kr.resetCSI()
	for _, rb := range saved {
		kr.routeByte(rb)
	}
}

func (kr *KeyRouter) resetCSI() {
	kr.csiState = 0
	kr.csiCP = 0
	kr.csiMod = 0
	kr.csiBuf = kr.csiBuf[:0]
}

func (kr *KeyRouter) handlePrefix() {
	kr.mu.Lock()
	defer kr.mu.Unlock()

	if kr.pendingPrefix {
		// Double Ctrl-] — send literal Ctrl-] to pty.
		kr.pendingPrefix = false
		if kr.prefixTimer != nil {
			kr.prefixTimer.Stop()
		}
		kr.ptmx.Write([]byte{overlayPrefix})
		return
	}

	// First Ctrl-] — start the timer.
	kr.pendingPrefix = true
	kr.prefixTimer = time.AfterFunc(doubleTapTimeout, func() {
		kr.mu.Lock()
		pending := kr.pendingPrefix
		kr.pendingPrefix = false
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
