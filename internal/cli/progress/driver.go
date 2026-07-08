package progress

import (
	"bytes"
	"encoding/json"
	"io"
)

// countingWriter is an io.Writer adapter that forwards bytes to an underlying
// writer while reporting the running total written into an Indicator's
// determinate display. It lets a copy or stream path (for example an oras
// layer copy or a container-build output pump) drive progress simply by
// writing through it, without knowing anything about rendering.
//
// The adapter is provided as a reusable seam and is intentionally not wired to
// any caller in this package. A consumer constructs it with the total size it
// expects to copy, tees its data through Write, and the Indicator renders the
// percentage.
type countingWriter struct {
	ind     *Indicator
	dst     io.Writer
	total   int64
	written int64
}

// NewCountingWriter returns an io.Writer that forwards to dst and reports
// progress to ind as a fraction of total. When dst is nil the bytes are
// counted and discarded, which is useful when the caller only needs the
// progress side effect. A total of zero renders 0% and never divides by zero.
func NewCountingWriter(ind *Indicator, dst io.Writer, total int64) io.Writer {
	return &countingWriter{ind: ind, dst: dst, total: total}
}

// Write forwards p to the underlying writer (if any), accumulates the byte
// count, and pushes the new running total into the Indicator. It returns the
// number of bytes handled by the underlying writer so it composes correctly in
// an io.Copy chain; when there is no underlying writer it reports all bytes
// consumed.
func (c *countingWriter) Write(p []byte) (int, error) {
	n := len(p)
	var err error
	if c.dst != nil {
		n, err = c.dst.Write(p)
	}
	c.written += int64(n)
	if c.ind != nil {
		c.ind.Update(c.written, c.total)
	}
	return n, err
}

// Add reports that n additional units of work have completed and refreshes the
// Indicator. It is the callback-style seam for paths that already track their
// own byte or item counts (for example oras PreCopy/PostCopy hooks) and want
// to feed increments rather than tee an io stream. A single countingWriter is
// meant to be driven from one goroutine; the Indicator it feeds is what is
// safe for concurrent use.
func (c *countingWriter) Add(n int64) {
	c.written += n
	if c.ind != nil {
		c.ind.Update(c.written, c.total)
	}
}

// Adder is the callback seam exposed to copy paths that report increments of
// completed work rather than streaming bytes. NewCountingWriter's result
// satisfies it.
type Adder interface {
	Add(n int64)
}

// compile-time assertion that countingWriter satisfies both seams.
var (
	_ io.Writer = (*countingWriter)(nil)
	_ Adder     = (*countingWriter)(nil)
)

// livenessWriter is an io.Writer a caller passes in place of io.Discard for a
// subprocess whose byte stream carries no useful progress payload but whose
// mere activity is proof the step is alive. It swallows every byte, never
// blocks the subprocess, and never errors, so it composes safely as the outer
// argument of an io.MultiWriter (for example alongside a stderr capture buffer)
// without altering the teed bytes or consuming the stream any other reader
// needs. It emits nothing itself: the visible advancing feedback comes from the
// Indicator's ticker-driven Start label, not from this writer, so on the
// non-TTY or quiet path it is inert and injects no control characters.
type livenessWriter struct {
	ind *Indicator
}

// NewLivenessWriter returns an io.Writer that discards its input while keeping
// a reference to ind for liveness bookkeeping. It is a named, non-discarding
// sink callers hand to a build or copy step in place of io.Discard so the
// step's output flows through the progress path. A nil ind is valid and yields
// a writer that simply swallows bytes, so callers need no nil branching.
func NewLivenessWriter(ind *Indicator) io.Writer {
	return livenessWriter{ind: ind}
}

// Write reports every byte as consumed and never errors, so it never blocks or
// fails the subprocess writing through it. It emits nothing to the Indicator's
// destination: the visible liveness animation is owned by the ticker via Start,
// and writing raw subprocess bytes here would corrupt a captured buffer or a
// terminal redraw line. It merely nudges the active liveness so a caller can
// observe forward motion, and is a no-op when the Indicator is nil, quiet, or
// non-TTY.
func (w livenessWriter) Write(p []byte) (int, error) {
	if w.ind != nil {
		w.ind.poke()
	}
	return len(p), nil
}

// compile-time assertion that livenessWriter satisfies io.Writer.
var _ io.Writer = livenessWriter{}

// solveStatus is the minimal subset of buildkit's client.SolveStatus record we
// parse from a BUILDKIT_PROGRESS=rawjson stream. buildx emits one JSON object
// per line on stderr, each carrying the build's evolving vertex (build-stage)
// set. We intentionally do NOT import buildkit's Go types: this local struct
// names only the fields the determinate signal needs and lets json.Unmarshal
// drop everything else, so the parse stays tolerant of buildkit-version schema
// drift and of the human log lines the devcontainer CLI interleaves.
type solveStatus struct {
	// Vertexes is the set of build-stage nodes revealed so far. Each rawjson line
	// re-emits the vertexes it touched; a vertex with a non-nil Completed
	// timestamp has finished. The completed-fraction of the known vertex set is
	// the determinate signal: it is the per-stage total buildkit reliably exposes
	// across versions, unlike the per-status byte totals (often zero).
	Vertexes []struct {
		Digest    string `json:"digest"`
		Completed *any   `json:"completed"`
	} `json:"vertexes"`
}

// buildkitProgressWriter is an io.Writer a caller passes as a build
// subprocess's stderr (and stdout) sink in place of a livenessWriter to turn a
// BUILDKIT_PROGRESS=rawjson stream into a determinate percentage. It line-buffers
// its input, parses each complete line as a rawjson solveStatus, and drives
// ind.Update(completedVertexes, knownVertexes) as build stages complete. The
// known-vertex total grows as the stream reveals stages, and Indicator.Update
// renders current/total against the moving total fine.
//
// It degrades cleanly and never regresses: a line that is not parseable rawjson,
// or a parsed line that has not yet revealed any vertex, falls back to a
// liveness poke so the step still animates exactly as a livenessWriter would.
// The determinate bar supersedes the spinner the first time Update fires with a
// non-zero total (existing Indicator.Update semantics), so when
// BUILDKIT_PROGRESS=rawjson is unhonored (older buildx, a non-buildkit daemon,
// or a plain docker build), the indeterminate liveness path is preserved intact.
//
// Like livenessWriter it emits nothing to the Indicator's destination itself:
// all rendering goes through Indicator.Update/poke, which already honor the
// TTY/quiet contract, so the non-TTY path stays plain and --quiet suppresses. It
// consumes every byte, forwards nothing to any co-writer, never blocks, and never
// errors, so it composes safely as the OUTER argument of an
// io.MultiWriter(w, &captureBuf) tee without altering the teed bytes.
type buildkitProgressWriter struct {
	ind *Indicator
	// buf retains a partial trailing line across Write calls so a rawjson object
	// split across chunk boundaries is only parsed once fully received.
	buf bytes.Buffer
	// seen dedupes vertex digests across the stream (each line re-emits the
	// vertexes it touched), so a vertex is counted toward the total exactly once.
	seen map[string]bool
	// completed is the running count of distinct vertexes observed with a
	// completion timestamp.
	completed int64
	// completedDigests records which vertex digests have already been counted as
	// completed, so a completion re-emitted on a later line is counted once. It
	// is lazily allocated via completedSet.
	completedDigests map[string]bool
}

// NewBuildkitProgressWriter returns an io.Writer that parses a
// BUILDKIT_PROGRESS=rawjson stream into determinate progress on ind, degrading
// to liveness pokes on any non-rawjson or not-yet-determinate input. A nil ind
// is valid and yields a pure byte-sink (matching NewLivenessWriter's
// nil-tolerance), so callers need no nil branching.
func NewBuildkitProgressWriter(ind *Indicator) io.Writer {
	return &buildkitProgressWriter{ind: ind, seen: make(map[string]bool)}
}

// Write accumulates p, splits it into complete lines on '\n', and processes each
// line, retaining any partial trailing line for the next call. It reports every
// byte as consumed and never errors, so it never blocks or fails the subprocess
// writing through it, and forwards nothing to any other writer so a co-writer in
// an io.MultiWriter sees the stream verbatim.
func (w *buildkitProgressWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			break
		}
		// Copy the line out before consuming it: bytes.Buffer may reuse the backing
		// array on the next Write, so the slice must not be retained past the Next.
		line := append([]byte(nil), data[:nl]...)
		w.buf.Next(nl + 1)
		w.processLine(line)
	}
	return len(p), nil
}

// processLine parses one complete line as rawjson and advances the determinate
// count, or pokes liveness when the line yields no new determinate information.
// A nil Indicator makes both paths no-ops, so the writer is a pure byte-sink.
func (w *buildkitProgressWriter) processLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	var status solveStatus
	if err := json.Unmarshal(trimmed, &status); err != nil || len(status.Vertexes) == 0 {
		// Not a rawjson status line (the devcontainer CLI interleaves its own human
		// log lines), or a status line that revealed no vertex: fall back to
		// liveness so the step still animates, exactly like a livenessWriter.
		if w.ind != nil {
			w.ind.poke()
		}
		return
	}
	newInfo := false
	for _, v := range status.Vertexes {
		if v.Digest == "" {
			continue
		}
		if !w.seen[v.Digest] {
			w.seen[v.Digest] = true
			newInfo = true
		}
		if v.Completed != nil && !w.completedSeen(v.Digest) {
			w.markCompleted(v.Digest)
			newInfo = true
		}
	}
	if !newInfo {
		// The line re-stated only already-known vertexes with no new completion;
		// keep the step alive without a redundant determinate redraw.
		if w.ind != nil {
			w.ind.poke()
		}
		return
	}
	if w.ind != nil {
		w.ind.Update(w.completed, int64(len(w.seen)))
	}
}

// completedSeen and markCompleted track which vertex digests have already been
// counted as completed, so a vertex re-emitted with a completion timestamp on a
// later line is counted exactly once. They lean on a dedicated set rather than
// overloading seen, because seen also holds still-running vertexes.
func (w *buildkitProgressWriter) completedSeen(digest string) bool {
	return w.completedSet()[digest]
}

func (w *buildkitProgressWriter) markCompleted(digest string) {
	w.completedSet()[digest] = true
	w.completed++
}

// completedSet lazily allocates the completed-digest set. It is separate from
// seen so an already-seen (running) vertex that later reports completion still
// increments the completed count exactly once.
func (w *buildkitProgressWriter) completedSet() map[string]bool {
	if w.completedDigests == nil {
		w.completedDigests = make(map[string]bool)
	}
	return w.completedDigests
}

// compile-time assertion that buildkitProgressWriter satisfies io.Writer.
var _ io.Writer = (*buildkitProgressWriter)(nil)
