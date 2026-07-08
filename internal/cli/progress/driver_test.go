package progress

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestCountingWriterForwardsBytesAndReportsProgress(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))

	var sink bytes.Buffer
	cw := NewCountingWriter(ind, &sink, 100)

	// Write 100 bytes in two halves; the sink must receive them verbatim and
	// the indicator must render progress ending at 100%.
	if _, err := cw.Write(bytes.Repeat([]byte("a"), 50)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := cw.Write(bytes.Repeat([]byte("b"), 50)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if sink.Len() != 100 {
		t.Errorf("expected 100 forwarded bytes, got %d", sink.Len())
	}
	if !strings.Contains(display.String(), "100%") {
		t.Errorf("expected 100%% reported, got %q", display.String())
	}
}

func TestCountingWriterReturnsUnderlyingCount(t *testing.T) {
	ind := New(io.Discard)
	var sink bytes.Buffer
	cw := NewCountingWriter(ind, &sink, 10)
	n, err := cw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 5 {
		t.Errorf("expected Write to return 5, got %d", n)
	}
}

func TestCountingWriterNilDstCountsAndDiscards(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	cw := NewCountingWriter(ind, nil, 4)
	n, err := cw.Write([]byte("data"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 4 {
		t.Errorf("expected all bytes consumed, got %d", n)
	}
	if !strings.Contains(display.String(), "100%") {
		t.Errorf("expected 100%% with nil dst, got %q", display.String())
	}
}

func TestCountingWriterAddIncrements(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	adder := NewCountingWriter(ind, nil, 100).(Adder)
	adder.Add(25)
	adder.Add(75)
	if !strings.Contains(display.String(), "100%") {
		t.Errorf("expected Add to reach 100%%, got %q", display.String())
	}
}

func TestCountingWriterComposesWithIOCopy(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	var sink bytes.Buffer
	src := strings.NewReader(strings.Repeat("x", 200))
	cw := NewCountingWriter(ind, &sink, 200)

	n, err := io.Copy(cw, src)
	if err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	if n != 200 {
		t.Errorf("expected io.Copy to move 200 bytes, got %d", n)
	}
	if sink.Len() != 200 {
		t.Errorf("expected sink to receive 200 bytes, got %d", sink.Len())
	}
	if !strings.Contains(display.String(), "100%") {
		t.Errorf("expected 100%% after copy, got %q", display.String())
	}
}

func TestLivenessWriterConsumesAllBytesNoError(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")
	w := NewLivenessWriter(ind)

	payload := bytes.Repeat([]byte("z"), 512)
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("liveness write: %v", err)
	}
	if n != len(payload) {
		t.Errorf("expected Write to report %d bytes, got %d", len(payload), n)
	}
}

// TestLivenessWriterEmitsNothingNonTTY proves the sink injects no control
// characters (or any bytes) into the indicator's destination on the non-TTY
// path: the visible feedback is owned by Start's ticker, not by writes through
// the sink. The buffer therefore holds only Start's plain label line.
func TestLivenessWriterEmitsNothingNonTTY(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")
	before := display.String()

	w := NewLivenessWriter(ind)
	if _, err := w.Write([]byte("noisy subprocess output\r\x1b[Kwith control chars")); err != nil {
		t.Fatalf("liveness write: %v", err)
	}

	if got := display.String(); got != before {
		t.Errorf("liveness write must emit nothing; buffer changed from %q to %q", before, got)
	}
	if strings.ContainsAny(display.String(), "\r\x1b") {
		t.Errorf("non-TTY output must contain no ESC or CR, got %q", display.String())
	}
}

// TestLivenessWriterQuietEmitsNothing proves a quiet indicator's liveness sink
// still consumes bytes without error and writes nothing.
func TestLivenessWriterQuietEmitsNothing(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false), WithQuiet(true))
	ind.Start("building")
	w := NewLivenessWriter(ind)
	n, err := w.Write([]byte("payload"))
	if err != nil || n != len("payload") {
		t.Fatalf("quiet liveness write = %d, %v", n, err)
	}
	if display.Len() != 0 {
		t.Errorf("quiet indicator must emit nothing, got %q", display.String())
	}
}

// TestLivenessWriterNilIndicator proves the sink is safe with no backing
// indicator: it swallows bytes and reports them consumed.
func TestLivenessWriterNilIndicator(t *testing.T) {
	w := NewLivenessWriter(nil)
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("nil-indicator liveness write = %d, %v", n, err)
	}
}

// TestLivenessWriterComposesInMultiWriterUnchanged proves the sink does not
// alter the bytes a co-writer in an io.MultiWriter receives, so a stderr
// capture buffer teed alongside it sees the stream verbatim. This is the
// must-not-break requirement for the freeze daemon-unreachable detection.
func TestLivenessWriterComposesInMultiWriterUnchanged(t *testing.T) {
	var display, capture bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")

	tee := io.MultiWriter(NewLivenessWriter(ind), &capture)
	payload := []byte("cannot connect to the docker daemon")
	if _, err := tee.Write(payload); err != nil {
		t.Fatalf("tee write: %v", err)
	}
	if capture.String() != string(payload) {
		t.Errorf("capture buffer must receive the stream verbatim, got %q", capture.String())
	}
}
