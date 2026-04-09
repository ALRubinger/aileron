package launch_test

import (
	"bytes"
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
	var ptmxBuf bytes.Buffer
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, &ptmxBuf, overlay)
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

func TestKeyRouter_CtrlA_ActivatesOverlay(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	var ptmxBuf bytes.Buffer
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, &ptmxBuf, overlay)
	go kr.Run()

	// Send Ctrl-A then wait for the timeout to fire.
	stdinW.Write([]byte{0x01})
	time.Sleep(600 * time.Millisecond)
	stdinW.Close()

	if !overlay.wasShown() {
		t.Error("overlay should have been shown after Ctrl-A timeout")
	}
	if ptmxBuf.Len() != 0 {
		t.Errorf("Ctrl-A should not have passed to pty, got %q", ptmxBuf.String())
	}
}

func TestKeyRouter_DoubleCtrlA_PassesThrough(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	var ptmxBuf bytes.Buffer
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, &ptmxBuf, overlay)
	go kr.Run()

	// Double Ctrl-A within the timeout window.
	stdinW.Write([]byte{0x01})
	time.Sleep(50 * time.Millisecond)
	stdinW.Write([]byte{0x01})
	time.Sleep(50 * time.Millisecond)
	stdinW.Close()

	time.Sleep(50 * time.Millisecond)

	if overlay.wasShown() {
		t.Error("overlay should not have been shown for double Ctrl-A")
	}
	if ptmxBuf.String() != "\x01" {
		t.Errorf("expected literal Ctrl-A in pty, got %q", ptmxBuf.String())
	}
}

func TestKeyRouter_CtrlAThenKey_ActivatesAndForwards(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	var ptmxBuf bytes.Buffer
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, &ptmxBuf, overlay)
	go kr.Run()

	// Ctrl-A followed by a regular key before timeout — activates overlay
	// and forwards the key to the overlay.
	stdinW.Write([]byte{0x01})
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
	var ptmxBuf bytes.Buffer
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, &ptmxBuf, overlay)
	go kr.Run()

	// Activate overlay via Ctrl-A + timeout.
	stdinW.Write([]byte{0x01})
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
	var ptmxBuf bytes.Buffer
	overlay := &mockOverlay{}

	kr := launch.NewKeyRouter(stdinR, &ptmxBuf, overlay)
	go kr.Run()

	// Activate overlay.
	stdinW.Write([]byte{0x01})
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
