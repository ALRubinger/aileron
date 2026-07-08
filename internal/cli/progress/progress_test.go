package progress

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// hasControlChars reports whether s contains a carriage return, an ANSI escape
// introducer, or any other C0 control character besides newline and tab. The
// non-TTY contract forbids all of these.
func hasControlChars(s string) bool {
	if strings.ContainsRune(s, '\r') || strings.Contains(s, "\x1b[") {
		return true
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' {
			return true
		}
	}
	return false
}

func TestNonTTYIndeterminateHasNoControlChars(t *testing.T) {
	var buf bytes.Buffer
	ind := New(&buf, WithForceTTY(false))

	ind.Start("uploading")
	ind.Done("uploading")

	out := buf.String()
	if hasControlChars(out) {
		t.Fatalf("non-TTY output contains control chars: %q", out)
	}
	if !strings.Contains(out, "uploading...") {
		t.Errorf("expected plain start line, got %q", out)
	}
	if !strings.Contains(out, "✓ uploading") {
		t.Errorf("expected plain done line, got %q", out)
	}
}

func TestTTYAdvancesFrames(t *testing.T) {
	var buf bytes.Buffer
	tick := make(chan time.Time)
	ind := New(&buf, WithForceTTY(true), WithTicker(tick))

	ind.Start("working")
	// Drive several ticks; each should redraw with a carriage return and,
	// across a full cycle, cycle through distinct spinner glyphs.
	for i := 0; i < len(spinnerFrames); i++ {
		tick <- time.Now()
	}
	ind.Done("working")

	out := buf.String()
	if !strings.Contains(out, "\r") {
		t.Errorf("expected carriage-return redraws on TTY, got %q", out)
	}
	for _, f := range spinnerFrames {
		if !strings.ContainsRune(out, f) {
			t.Errorf("expected spinner frame %q in output %q", f, out)
		}
	}
	if !strings.HasSuffix(out, "✓ working\n") {
		t.Errorf("expected resolved done line at end, got %q", out)
	}
}

func TestStartThenDoneStopsSpinner(t *testing.T) {
	// Done on a running spinner must join the redraw goroutine before it
	// returns, so the goroutine cannot keep consuming ticks (a leak) or draw a
	// spinner frame after the resolved "✓" line. Drive one tick, resolve with
	// Done, then prove nothing is listening on the tick channel.
	var buf syncBuffer
	tick := make(chan time.Time)
	ind := New(&buf, WithForceTTY(true), WithTicker(tick))

	ind.Start("copy")
	tick <- time.Now() // one spinner frame
	ind.Done("copy")

	// resolve() joins the redraw goroutine synchronously, so after Done the
	// goroutine has exited and a non-blocking send must hit the default branch.
	select {
	case tick <- time.Now():
		t.Fatal("redraw goroutine still consuming ticks after Done")
	default:
	}
	if !strings.HasSuffix(buf.String(), "✓ copy\n") {
		t.Errorf("expected resolved done line after Start->Done, got %q", buf.String())
	}
}

func TestStartThenFailStopsSpinner(t *testing.T) {
	// Fail has the same stop-and-join semantics as Done: it must terminate the
	// redraw goroutine before returning so no tick is consumed and no spinner
	// frame is drawn after the resolved "✗" line.
	var buf syncBuffer
	tick := make(chan time.Time)
	ind := New(&buf, WithForceTTY(true), WithTicker(tick))

	ind.Start("deploy")
	tick <- time.Now() // one spinner frame
	ind.Fail("deploy")

	select {
	case tick <- time.Now():
		t.Fatal("redraw goroutine still consuming ticks after Fail")
	default:
	}
	if !strings.HasSuffix(buf.String(), "✗ deploy\n") {
		t.Errorf("expected resolved fail line after Start->Fail, got %q", buf.String())
	}
}

func TestFailResolvesFailedState(t *testing.T) {
	var buf bytes.Buffer
	ind := New(&buf, WithForceTTY(false))
	ind.Start("deploy")
	ind.Fail("deploy")
	out := buf.String()
	if !strings.Contains(out, "✗ deploy") {
		t.Errorf("expected failed line, got %q", out)
	}
}

func TestResolveIdempotent(t *testing.T) {
	var buf bytes.Buffer
	ind := New(&buf, WithForceTTY(true))
	ind.Start("x")
	ind.Done("x")
	before := buf.String()
	// A second Done and a Fail after resolution must be no-ops.
	ind.Done("x")
	ind.Fail("x")
	if buf.String() != before {
		t.Errorf("resolve was not idempotent: %q -> %q", before, buf.String())
	}
}

func TestNoWriteAfterResolve(t *testing.T) {
	// Once resolved, no method may write anything, so the completion line is
	// always the last bytes emitted.
	for _, tty := range []bool{true, false} {
		var buf bytes.Buffer
		ind := New(&buf, WithForceTTY(tty))
		ind.Start("job")
		ind.Done("job")
		resolved := buf.String()
		// Post-resolve calls must be inert.
		ind.Start("job2")
		ind.Fail("job")
		ind.Done("job")
		if buf.String() != resolved {
			t.Errorf("tty=%v: wrote after resolve: %q -> %q", tty, resolved, buf.String())
		}
		mark := "✓ job"
		if !strings.HasSuffix(strings.TrimRight(buf.String(), "\n"), mark) {
			t.Errorf("tty=%v: resolved line is not last, got %q", tty, buf.String())
		}
	}
}

func TestStartTwiceIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	ind := New(&buf, WithForceTTY(false))
	ind.Start("a")
	ind.Start("b") // ignored while active
	out := buf.String()
	if strings.Contains(out, "b...") {
		t.Errorf("second Start should be ignored, got %q", out)
	}
}

func TestQuietSuppressesAllOutput(t *testing.T) {
	for _, tty := range []bool{true, false} {
		var buf bytes.Buffer
		ind := New(&buf, WithForceTTY(tty), WithQuiet(true))
		ind.Start("work")
		ind.Done("work")
		ind.Fail("work")
		if buf.Len() != 0 {
			t.Errorf("quiet (tty=%v) produced output: %q", tty, buf.String())
		}
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer so the -race test can have the
// redraw goroutine and the test goroutine write concurrently without a data
// race in the buffer itself (which is what we are not testing).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestConcurrentPokesAndDone drives concurrent ticks (the spinner goroutine)
// and concurrent liveness pokes (a subprocess writing through a livenessWriter)
// while another goroutine resolves the indicator with Done. The resolve must
// still win the race and leave the "✓" line as the last thing written, with no
// data race on shared render state (run under -race).
func TestConcurrentPokesAndDone(t *testing.T) {
	var buf syncBuffer
	tick := make(chan time.Time)
	ind := New(&buf, WithForceTTY(true), WithTicker(tick))

	ind.Start("copy")

	var wg sync.WaitGroup
	// Concurrent ticks.
	stopTicks := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopTicks:
				return
			case tick <- time.Now():
			}
		}
	}()
	// Concurrent liveness pokes via the writer seam.
	lw := NewLivenessWriter(ind)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_, _ = lw.Write([]byte("chunk"))
		}
	}()

	ind.Done("copy") // stops the redraw goroutine
	close(stopTicks)
	wg.Wait()

	if !strings.HasSuffix(buf.String(), "✓ copy\n") {
		t.Errorf("expected resolved line after concurrent activity, got %q", buf.String())
	}
}

func TestNonTTYAutoDetectViaPipe(t *testing.T) {
	// A pipe's read/write ends are *os.File but not terminals; New must pick
	// the non-TTY path via real detection (no force).
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	ind := New(w)
	if ind.tty {
		t.Errorf("expected pipe write end to be detected as non-TTY")
	}
}

func TestNonFileWriterIsNonTTY(t *testing.T) {
	var buf bytes.Buffer
	ind := New(&buf)
	if ind.tty {
		t.Errorf("expected non-*os.File writer to be non-TTY")
	}
}

func TestForceTTYAutodetectSeam(t *testing.T) {
	// Exercise the isTerminalFn seam by forcing it true against a pipe write
	// end, proving New consults it when the writer is an *os.File.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	orig := isTerminalFn
	isTerminalFn = func(f *os.File) bool { return true }
	defer func() { isTerminalFn = orig }()

	ind := New(w)
	if !ind.tty {
		t.Errorf("expected isTerminalFn seam to force TTY detection")
	}
}
