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

// rawjsonLine builds one BUILDKIT_PROGRESS=rawjson status line describing a
// single vertex, optionally with a completion timestamp, mirroring the shape
// buildx emits (one JSON object per line on stderr).
func rawjsonLine(digest string, completed bool) string {
	if completed {
		return `{"vertexes":[{"digest":"` + digest + `","name":"stage","completed":"2026-07-08T00:00:00Z"}]}` + "\n"
	}
	return `{"vertexes":[{"digest":"` + digest + `","name":"stage"}]}` + "\n"
}

// TestBuildkitProgressWriterDrivesDeterminatePercentage proves a rawjson stream
// whose vertexes all complete drives the Indicator to 100%.
func TestBuildkitProgressWriterDrivesDeterminatePercentage(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")
	w := NewBuildkitProgressWriter(ind)

	// Reveal three vertexes, then complete all three. The final Update must be
	// 3/3 = 100%.
	for _, d := range []string{"sha256:v1", "sha256:v2", "sha256:v3"} {
		if _, err := w.Write([]byte(rawjsonLine(d, false))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for _, d := range []string{"sha256:v1", "sha256:v2", "sha256:v3"} {
		if _, err := w.Write([]byte(rawjsonLine(d, true))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if !strings.Contains(display.String(), "100%") {
		t.Errorf("expected 100%% after all vertexes complete, got %q", display.String())
	}
}

// TestBuildkitProgressWriterIgnoresNonJSONLines proves interleaved human log
// lines from the devcontainer CLI do not corrupt the vertex count: the parsed
// rawjson still settles at 100%.
func TestBuildkitProgressWriterIgnoresNonJSONLines(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")
	w := NewBuildkitProgressWriter(ind)

	stream := "" +
		"[+] Building devcontainer...\n" +
		rawjsonLine("sha256:v1", false) +
		"resolving provenance for metadata file\n" +
		rawjsonLine("sha256:v1", true) +
		"not-json {oops\n"
	if _, err := w.Write([]byte(stream)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(display.String(), "100%") {
		t.Errorf("expected 100%% with one vertex completed despite noise, got %q", display.String())
	}
}

// TestBuildkitProgressWriterReassemblesSplitLines proves a rawjson object split
// across multiple Write calls (partial-line reassembly) still parses.
func TestBuildkitProgressWriterReassemblesSplitLines(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")
	w := NewBuildkitProgressWriter(ind)

	line := rawjsonLine("sha256:v1", true)
	// Write the line one byte at a time; nothing renders until the trailing
	// newline arrives, then it settles at 1/1 = 100%.
	for i := 0; i < len(line); i++ {
		if _, err := w.Write([]byte{line[i]}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if !strings.Contains(display.String(), "100%") {
		t.Errorf("expected split-line reassembly to reach 100%%, got %q", display.String())
	}
}

// TestBuildkitProgressWriterAllNoiseEmitsNothingDeterminate proves an all-noise
// stream (no parseable vertexes) emits no determinate output but consumes every
// byte with no error (poke-only liveness), leaving the buffer at just Start's
// label line.
func TestBuildkitProgressWriterAllNoiseEmitsNothingDeterminate(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")
	before := display.String()
	w := NewBuildkitProgressWriter(ind)

	payload := []byte("plain docker build output line 1\nno buildkit rawjson here\n")
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(payload) {
		t.Errorf("expected all %d bytes consumed, got %d", len(payload), n)
	}
	if got := display.String(); got != before {
		t.Errorf("all-noise stream must emit no determinate output; buffer changed from %q to %q", before, got)
	}
	if strings.Contains(display.String(), "%") {
		t.Errorf("all-noise stream must not render a percentage, got %q", display.String())
	}
}

// TestBuildkitProgressWriterNilIndicator proves the writer is safe with no
// backing indicator: it swallows bytes (including a full rawjson line) and
// reports them consumed with no panic.
func TestBuildkitProgressWriterNilIndicator(t *testing.T) {
	w := NewBuildkitProgressWriter(nil)
	payload := []byte(rawjsonLine("sha256:v1", true) + "noise\n")
	n, err := w.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("nil-indicator write = %d, %v", n, err)
	}
}

// TestBuildkitProgressWriterComposesInMultiWriterUnchanged proves the writer
// does not alter the bytes a co-writer in an io.MultiWriter receives, so a
// stderr capture buffer teed alongside it (the freeze daemon-unreachable
// detection) sees the stream verbatim. Mirrors
// TestLivenessWriterComposesInMultiWriterUnchanged.
func TestBuildkitProgressWriterComposesInMultiWriterUnchanged(t *testing.T) {
	var display, capture bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")

	tee := io.MultiWriter(NewBuildkitProgressWriter(ind), &capture)
	payload := []byte(rawjsonLine("sha256:v1", true) + "cannot connect to the docker daemon\n")
	if _, err := tee.Write(payload); err != nil {
		t.Fatalf("tee write: %v", err)
	}
	if capture.String() != string(payload) {
		t.Errorf("capture buffer must receive the stream verbatim, got %q", capture.String())
	}
}

// TestBuildkitProgressWriterQuietEmitsNothing proves a quiet indicator's build
// progress writer consumes a full rawjson stream without error and writes
// nothing.
func TestBuildkitProgressWriterQuietEmitsNothing(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false), WithQuiet(true))
	ind.Start("building")
	w := NewBuildkitProgressWriter(ind)

	payload := []byte(rawjsonLine("sha256:v1", false) + rawjsonLine("sha256:v1", true))
	n, err := w.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("quiet build progress write = %d, %v", n, err)
	}
	if display.Len() != 0 {
		t.Errorf("quiet indicator must emit nothing, got %q", display.String())
	}
}

// TestBuildkitProgressWriterEmptyDigestVertexIgnored proves a vertex with no
// digest does not count toward the total: a stream of one empty-digest vertex
// then one real completed vertex settles at 1/1 = 100%, not 1/2.
func TestBuildkitProgressWriterEmptyDigestVertexIgnored(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")
	w := NewBuildkitProgressWriter(ind)

	// A rawjson line whose vertex carries an empty digest reveals no countable
	// stage; it must be skipped rather than inflating the known-vertex total.
	if _, err := w.Write([]byte(`{"vertexes":[{"digest":"","name":"noise"}]}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.Write([]byte(rawjsonLine("sha256:v1", true))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(display.String(), "100%") {
		t.Errorf("empty-digest vertex must not inflate the total; want 100%%, got %q", display.String())
	}
}

// TestBuildkitProgressWriterDuplicateLinesDoNotDoubleCount proves a vertex
// re-emitted as completed on later lines is counted once: after two vertexes
// complete (each restated multiple times), the fraction is exactly 2/2 = 100%,
// never overshooting.
func TestBuildkitProgressWriterDuplicateLinesDoNotDoubleCount(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")
	w := NewBuildkitProgressWriter(ind)

	stream := "" +
		rawjsonLine("sha256:v1", false) +
		rawjsonLine("sha256:v2", false) +
		rawjsonLine("sha256:v1", true) +
		// Restate v1 as completed again (buildkit re-emits touched vertexes): this
		// re-stated line carries no new info and must not double-count or redraw a
		// determinate step; it only pokes liveness.
		rawjsonLine("sha256:v1", true) +
		rawjsonLine("sha256:v2", true)
	if _, err := w.Write([]byte(stream)); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := display.String()
	if !strings.Contains(out, "100%") {
		t.Errorf("want the two-vertex build to settle at 100%%, got %q", out)
	}
	// The non-TTY path emits one line per changed percentage; a double-count would
	// have driven the fraction past 2/2 and cannot, so 100% is the terminal line.
	if strings.Contains(out, "150%") || strings.Contains(out, "200%") {
		t.Errorf("duplicate completion lines must not overshoot 100%%, got %q", out)
	}
}

// TestBuildkitProgressWriterBlankLinesIgnored proves blank/whitespace-only lines
// in the stream are consumed with no error and no effect on the count.
func TestBuildkitProgressWriterBlankLinesIgnored(t *testing.T) {
	var display bytes.Buffer
	ind := New(&display, WithForceTTY(false))
	ind.Start("building")
	w := NewBuildkitProgressWriter(ind)

	stream := "\n   \n" + rawjsonLine("sha256:v1", true) + "\n"
	n, err := w.Write([]byte(stream))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(stream) {
		t.Errorf("expected all %d bytes consumed, got %d", len(stream), n)
	}
	if !strings.Contains(display.String(), "100%") {
		t.Errorf("blank lines must not disturb the count; want 100%%, got %q", display.String())
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
