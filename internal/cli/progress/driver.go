package progress

import "io"

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
