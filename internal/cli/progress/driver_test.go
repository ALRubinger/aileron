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
