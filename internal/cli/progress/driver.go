package progress

import (
	"io"
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
