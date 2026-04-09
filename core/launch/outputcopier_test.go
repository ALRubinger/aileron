package launch_test

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/launch"
)

// simpleOverlay is a minimal overlay for output copier tests.
type simpleOverlay struct {
	mu     sync.Mutex
	active bool
}

func (o *simpleOverlay) Show()          { o.mu.Lock(); o.active = true; o.mu.Unlock() }
func (o *simpleOverlay) Hide()          { o.mu.Lock(); o.active = false; o.mu.Unlock() }
func (o *simpleOverlay) HandleKey(byte) {}
func (o *simpleOverlay) IsActive() bool { o.mu.Lock(); defer o.mu.Unlock(); return o.active }
func (o *simpleOverlay) setActive(v bool) { o.mu.Lock(); o.active = v; o.mu.Unlock() }

// safeBuf is a thread-safe bytes.Buffer for test assertions.
type safeBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (b *safeBuf) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

func TestOutputCopier_NormalMode(t *testing.T) {
	srcR, srcW := io.Pipe()
	dst := &safeBuf{}
	overlay := &simpleOverlay{}

	oc := launch.NewOutputCopier(srcR, dst, overlay)
	go oc.Run()

	srcW.Write([]byte("agent output"))
	srcW.Close()
	time.Sleep(50 * time.Millisecond)

	if dst.String() != "agent output" {
		t.Errorf("expected 'agent output', got %q", dst.String())
	}
}

func TestOutputCopier_OverlayMode_Buffers(t *testing.T) {
	srcR, srcW := io.Pipe()
	dst := &safeBuf{}
	overlay := &simpleOverlay{}
	overlay.setActive(true)

	oc := launch.NewOutputCopier(srcR, dst, overlay)
	go oc.Run()

	srcW.Write([]byte("buffered"))
	time.Sleep(50 * time.Millisecond)
	srcW.Close()
	time.Sleep(50 * time.Millisecond)

	if dst.Len() != 0 {
		t.Errorf("expected no output to dst while overlay active, got %q", dst.String())
	}
}

func TestOutputCopier_Flush(t *testing.T) {
	srcR, srcW := io.Pipe()
	dst := &safeBuf{}
	overlay := &simpleOverlay{}
	overlay.setActive(true)

	oc := launch.NewOutputCopier(srcR, dst, overlay)
	go oc.Run()

	srcW.Write([]byte("buffered content"))
	time.Sleep(50 * time.Millisecond)

	// Flush should write buffered content to dst.
	oc.Flush()

	if !strings.Contains(dst.String(), "buffered content") {
		t.Errorf("expected 'buffered content' after flush, got %q", dst.String())
	}
}

func TestOutputCopier_FlushEmpty(t *testing.T) {
	dst := &safeBuf{}
	overlay := &simpleOverlay{}
	srcR, _ := io.Pipe()

	oc := launch.NewOutputCopier(srcR, dst, overlay)

	// Flush with nothing buffered — should not panic or write anything.
	oc.Flush()

	if dst.Len() != 0 {
		t.Error("expected no output from empty flush")
	}
}

func TestOutputCopier_BufferBounded(t *testing.T) {
	srcR, srcW := io.Pipe()
	dst := &safeBuf{}
	overlay := &simpleOverlay{}
	overlay.setActive(true)

	oc := launch.NewOutputCopier(srcR, dst, overlay)
	go oc.Run()

	// Write more than 1MB.
	chunk := strings.Repeat("x", 512*1024) // 512KB
	srcW.Write([]byte(chunk))
	srcW.Write([]byte(chunk))
	srcW.Write([]byte(chunk)) // 1.5MB total
	time.Sleep(100 * time.Millisecond)
	srcW.Close()
	time.Sleep(50 * time.Millisecond)

	oc.Flush()

	// Flushed content should be bounded to ~1MB.
	if dst.Len() > 1<<20+4096 {
		t.Errorf("buffer should be bounded to ~1MB, got %d bytes", dst.Len())
	}
}

func TestOutputCopier_TransitionToOverlay(t *testing.T) {
	srcR, srcW := io.Pipe()
	dst := &safeBuf{}
	overlay := &simpleOverlay{}

	oc := launch.NewOutputCopier(srcR, dst, overlay)
	go oc.Run()

	// Normal output first.
	srcW.Write([]byte("visible"))
	time.Sleep(50 * time.Millisecond)

	// Activate overlay.
	overlay.setActive(true)
	srcW.Write([]byte("hidden"))
	time.Sleep(50 * time.Millisecond)

	if !strings.Contains(dst.String(), "visible") {
		t.Error("expected 'visible' in output")
	}
	if strings.Contains(dst.String(), "hidden") {
		t.Error("'hidden' should not appear while overlay active")
	}

	// Flush the buffered content.
	oc.Flush()
	if !strings.Contains(dst.String(), "hidden") {
		t.Error("expected 'hidden' after flush")
	}

	srcW.Close()
}
