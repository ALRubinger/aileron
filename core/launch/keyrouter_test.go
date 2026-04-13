package launch_test

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/launch"
)

// mockOverlay records calls for testing.
type mockOverlay struct {
	mu       sync.Mutex
	shown    bool
	hidden   bool
	keys     []byte
	isActive bool
}

func (m *mockOverlay) Show() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shown = true
	m.isActive = true
}

func (m *mockOverlay) Hide() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hidden = true
	m.isActive = false
}

func (m *mockOverlay) HandleKey(b byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys = append(m.keys, b)
}

func (m *mockOverlay) IsActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isActive
}

func (m *mockOverlay) wasShown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shown
}

func (m *mockOverlay) getKeys() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]byte, len(m.keys))
	copy(out, m.keys)
	return out
}

func TestKeyRouter_NormalBytesPassThrough(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	stdinW.Write([]byte("hello"))
	stdinW.Close()

	// Give the router time to process.
	time.Sleep(50 * time.Millisecond)

	if ptmxBuf.String() != "hello" {
		t.Errorf("expected 'hello' passed to pty, got %q", ptmxBuf.String())
	}
	if overlay.wasShown() {
		t.Error("overlay should not have been shown")
	}
}

func TestKeyRouter_CtrlBracket_ActivatesOverlay(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// Send Ctrl-] then wait for the timeout to fire.
	stdinW.Write([]byte{0x1D})
	time.Sleep(600 * time.Millisecond)
	stdinW.Close()

	if !overlay.wasShown() {
		t.Error("overlay should have been shown after Ctrl-] timeout")
	}
	if ptmxBuf.Len() != 0 {
		t.Errorf("Ctrl-] should not have passed to pty, got %q", ptmxBuf.String())
	}
}

func TestKeyRouter_DoubleCtrlBracket_PassesThrough(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// Double Ctrl-] within the timeout window.
	stdinW.Write([]byte{0x1D})
	time.Sleep(50 * time.Millisecond)
	stdinW.Write([]byte{0x1D})
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	if overlay.wasShown() {
		t.Error("overlay should not have been shown for double Ctrl-]")
	}
	if ptmxBuf.String() != "\x1D" {
		t.Errorf("expected literal Ctrl-] in pty, got %q", ptmxBuf.String())
	}
}

func TestKeyRouter_CtrlBracketThenKey_ActivatesAndForwards(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// Ctrl-] followed by a regular key before timeout — activates overlay
	// and forwards the key to the overlay.
	stdinW.Write([]byte{0x1D})
	time.Sleep(50 * time.Millisecond)
	stdinW.Write([]byte{'x'})
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	if !overlay.wasShown() {
		t.Error("overlay should have been shown")
	}
	keys := overlay.getKeys()
	if len(keys) != 1 || keys[0] != 'x' {
		t.Errorf("expected key 'x' forwarded to overlay, got %v", keys)
	}
	if ptmxBuf.Len() != 0 {
		t.Errorf("no bytes should reach pty, got %q", ptmxBuf.String())
	}
}

func TestKeyRouter_OverlayMode_KeysGoToOverlay(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// Activate overlay via Ctrl-] + timeout.
	stdinW.Write([]byte{0x1D})
	time.Sleep(600 * time.Millisecond)

	// Now send keys — they should go to the overlay, not the pty.
	stdinW.Write([]byte("abc"))
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	keys := overlay.getKeys()
	if string(keys) != "abc" {
		t.Errorf("expected 'abc' to overlay, got %q", string(keys))
	}
	if ptmxBuf.Len() != 0 {
		t.Error("no bytes should reach pty while overlay is active")
	}
}

func TestKeyRouter_DeactivateOverlay(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// Activate overlay.
	stdinW.Write([]byte{0x1D})
	time.Sleep(600 * time.Millisecond)

	// Deactivate.
	kr.DeactivateOverlay()
	time.Sleep(50 * time.Millisecond)

	// Keys should now go to pty.
	stdinW.Write([]byte("after"))
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	if ptmxBuf.String() != "after" {
		t.Errorf("expected 'after' in pty after deactivate, got %q", ptmxBuf.String())
	}
}

// CSI u (kitty keyboard protocol) tests.

func TestKeyRouter_CSIu_CtrlBracket_ActivatesOverlay(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// Send CSI u encoding of Ctrl-]: ESC [ 93 ; 5 u
	// (codepoint 93 = ']', modifier 5 = 1+4 where 4=Ctrl)
	stdinW.Write([]byte("\x1b[93;5u"))
	time.Sleep(600 * time.Millisecond)
	stdinW.Close()

	if !overlay.wasShown() {
		t.Error("overlay should have been shown after CSI u Ctrl-]")
	}
	if ptmxBuf.Len() != 0 {
		t.Errorf("CSI u Ctrl-] should not pass to pty, got %q", ptmxBuf.String())
	}
}

func TestKeyRouter_CSIu_DoubleCtrlBracket_PassesThrough(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// Two CSI u Ctrl-] within the timeout window.
	stdinW.Write([]byte("\x1b[93;5u"))
	time.Sleep(50 * time.Millisecond)
	stdinW.Write([]byte("\x1b[93;5u"))
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	if overlay.wasShown() {
		t.Error("overlay should not have been shown for double CSI u Ctrl-]")
	}
	if ptmxBuf.String() != "\x1D" {
		t.Errorf("expected literal 0x1D in pty, got %q", ptmxBuf.String())
	}
}

func TestKeyRouter_CSIu_NonPrefix_PassesThrough(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// Send CSI u encoding of Ctrl-A: ESC [ 97 ; 5 u — not our prefix.
	stdinW.Write([]byte("\x1b[97;5u"))
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	if overlay.wasShown() {
		t.Error("overlay should not show for Ctrl-A CSI u")
	}
	// The raw sequence should pass through to the pty.
	if ptmxBuf.Len() == 0 {
		t.Error("expected non-prefix CSI u sequence to pass to pty")
	}
}

func TestKeyRouter_CSIu_OtherEscSequence_PassesThrough(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// Arrow up: ESC [ A — not a CSI u sequence.
	stdinW.Write([]byte("\x1b[A"))
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	if overlay.wasShown() {
		t.Error("overlay should not show for arrow key")
	}
	if ptmxBuf.Len() == 0 {
		t.Error("expected arrow key sequence to pass to pty")
	}
}

func TestKeyRouter_CSIu_CtrlShiftBracket_ActivatesOverlay(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// Ctrl+Shift+]: ESC [ 93 ; 6 u (modifier 6 = 1+4+1 = Ctrl+Shift)
	// Should still activate because Ctrl bit is set.
	stdinW.Write([]byte("\x1b[93;6u"))
	time.Sleep(600 * time.Millisecond)
	stdinW.Close()

	if !overlay.wasShown() {
		t.Error("overlay should have been shown for Ctrl+Shift+] CSI u")
	}
}

func TestKeyRouter_CSIu_NoModifier_PassesThrough(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// CSI 93 u — codepoint ']' but no modifier (bare key, no Ctrl).
	stdinW.Write([]byte("\x1b[93u"))
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	if overlay.wasShown() {
		t.Error("overlay should not show for bare ] without Ctrl modifier")
	}
}

func TestKeyRouter_CSIu_EscNonBracket_PassesThrough(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// ESC followed by 'O' (not '[') — e.g. SS3 sequence. Should pass through.
	stdinW.Write([]byte("\x1bOP"))
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	if overlay.wasShown() {
		t.Error("overlay should not show for SS3 sequence")
	}
	if ptmxBuf.Len() == 0 {
		t.Error("expected SS3 sequence to pass to pty")
	}
}

func TestKeyRouter_CSIu_IncompleteSequence_PassesThrough(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	ptmxBuf := &safeBuf{}
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, ptmxBuf, overlay)
	go kr.Run()

	// ESC [ followed by a non-digit — incomplete CSI u, should replay.
	stdinW.Write([]byte("\x1b[X"))
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	if overlay.wasShown() {
		t.Error("overlay should not show for incomplete CSI sequence")
	}
	if ptmxBuf.Len() == 0 {
		t.Error("expected incomplete sequence to pass to pty")
	}
}
