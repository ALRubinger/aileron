// Package progress provides a small, self-contained CLI progress-indicator
// helper for long-running commands. It renders live liveness (indeterminate
// spinner) and percentage (determinate) feedback on an interactive terminal,
// and degrades to plain, control-character-free lines when the destination is
// not a TTY (a pipe, a file, or a captured buffer).
//
// The package intentionally has no command wiring. It exposes a writer-backed
// Indicator plus a driver adapter so copy/stream paths can push updates into
// it, leaving the choice of caller to the consumer.
//
// Concurrency: an Indicator is safe for concurrent use. Start launches a
// redraw goroutine driven by a ticker; Update, Done, and Fail may be called
// from other goroutines. All shared render state is guarded by a mutex, and a
// resolve (Done/Fail) is idempotent and never writes after the line is
// resolved.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// isTerminalFn reports whether f is attached to an interactive terminal. It is
// a package-level seam so tests can force either branch without a real TTY,
// mirroring isTTYFn in cmd/aileron/claude_auth.go.
var isTerminalFn = func(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// spinnerFrames are the successive glyphs drawn on the TTY liveness path.
var spinnerFrames = []rune{'|', '/', '-', '\\'}

// tickInterval is the default cadence of the indeterminate redraw when no
// ticker is injected via WithTicker.
const tickInterval = 120 * time.Millisecond

// throttleInterval bounds how often the non-TTY determinate path emits a plain
// percentage line, so a fast Update loop does not flood a log.
const throttleInterval = 500 * time.Millisecond

// Indicator renders progress feedback to a writer. Construct one with New.
type Indicator struct {
	w         io.Writer
	tty       bool
	ttyForced bool
	quiet     bool

	// ticker is the channel that drives indeterminate redraws. When nil, New
	// installs a real time.Ticker on Start. A test may inject a deterministic
	// channel via WithTicker.
	ticker <-chan time.Time

	mu sync.Mutex
	// active is true between Start and Done/Fail on the indeterminate path.
	active bool
	// finished is set by the first Done/Fail and makes the indicator inert:
	// every subsequent output method (Start, Update, Done, Fail) is a no-op, so
	// nothing can ever be written after the resolved completion line.
	finished bool
	label    string
	frame    int
	// stop signals the redraw goroutine to exit; done is closed by the
	// goroutine once it has returned so Done/Fail can join it.
	stop chan struct{}
	done chan struct{}

	// determinate tracking.
	lastEmit    time.Time
	lastPercent int
}

// Option configures an Indicator at construction time.
type Option func(*Indicator)

// WithQuiet suppresses all output on both the TTY and non-TTY paths when q is
// true. A quiet Indicator still tracks state so callers need no branching.
func WithQuiet(q bool) Option {
	return func(i *Indicator) { i.quiet = q }
}

// WithTicker injects the channel that drives the indeterminate redraw loop,
// replacing the default wall-clock ticker. Tests send on the channel to step
// the spinner deterministically. When set, the Indicator does not create or
// stop a real ticker.
func WithTicker(ch <-chan time.Time) Option {
	return func(i *Indicator) { i.ticker = ch }
}

// WithForceTTY overrides TTY autodetection. Passing true selects the
// terminal render path (carriage-return redraws, spinner); false selects the
// plain path. It exists so tests can exercise both branches against a buffer.
func WithForceTTY(tty bool) Option {
	return func(i *Indicator) {
		i.tty = tty
		i.ttyForced = true
	}
}

// ttyForced records whether WithForceTTY set tty explicitly, so New does not
// override it with autodetection.
//
// It lives on the struct rather than in the closure so autodetection in New
// can consult it after all options have run.
func (i *Indicator) forcedTTY() bool { return i.ttyForced }

// New builds an Indicator writing to w. It detects whether w is an interactive
// terminal at construction time: if w is an *os.File it consults
// term.IsTerminal on the file descriptor, otherwise w is treated as non-TTY.
// WithForceTTY overrides this detection.
func New(w io.Writer, opts ...Option) *Indicator {
	i := &Indicator{w: w}
	for _, opt := range opts {
		opt(i)
	}
	if !i.forcedTTY() {
		if f, ok := w.(*os.File); ok {
			i.tty = isTerminalFn(f)
		}
	}
	return i
}

// Start begins an indeterminate liveness indication under label. On the TTY
// path it launches a redraw goroutine that advances the spinner on each tick.
// On the non-TTY path it emits a single plain start line. Calling Start while
// an indication is already active, or after the indicator has been resolved by
// Done or Fail, is a no-op.
func (i *Indicator) Start(label string) {
	i.mu.Lock()
	if i.quiet || i.active || i.finished {
		i.mu.Unlock()
		return
	}
	i.active = true
	i.label = label
	i.frame = 0

	if !i.tty {
		fmt.Fprintf(i.w, "%s...\n", label)
		i.mu.Unlock()
		return
	}

	// Draw the first frame immediately so callers see feedback before the
	// first tick fires.
	i.drawLocked()

	ticker := i.ticker
	var realTicker *time.Ticker
	if ticker == nil {
		realTicker = time.NewTicker(tickInterval)
		ticker = realTicker.C
	}
	i.stop = make(chan struct{})
	i.done = make(chan struct{})
	stop := i.stop
	done := i.done
	i.mu.Unlock()

	go func() {
		if realTicker != nil {
			defer realTicker.Stop()
		}
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case _, ok := <-ticker:
				if !ok {
					return
				}
				i.mu.Lock()
				if !i.active {
					i.mu.Unlock()
					return
				}
				i.frame++
				i.drawLocked()
				i.mu.Unlock()
			}
		}
	}()
}

// Done resolves the indicator as successful, drawing "✓ <label>" on a fresh
// line. It stops any redraw goroutine and marks the indicator finished, whether
// the preceding feedback was an indeterminate spinner or a determinate bar. A
// second call, a later Fail, a later Update, or a later Start is a no-op, so the
// resolved line is the last thing the indicator ever writes.
func (i *Indicator) Done(label string) {
	i.resolve('✓', label)
}

// Fail resolves the active indication as failed, drawing "✗ <label>". It has
// the same stop-and-idempotency semantics as Done.
func (i *Indicator) Fail(label string) {
	i.resolve('✗', label)
}

// resolve stops any redraw goroutine and writes the terminal completion line.
// The goroutine is joined outside the lock to avoid deadlocking against a tick
// that is waiting on the mutex.
func (i *Indicator) resolve(mark rune, label string) {
	i.mu.Lock()
	if i.quiet || i.finished {
		i.mu.Unlock()
		return
	}
	// Mark finished and inactive first, under the lock, so a tick or a
	// concurrent Update blocked on the mutex observes both and returns without
	// writing anything after the resolved completion line. This resolves the
	// indicator whether or not a spinner was active, so a determinate-only flow
	// (Update... then Done) still emits a completion line and becomes inert.
	i.finished = true
	i.active = false
	// Signal and join the redraw goroutine (TTY path only) before writing the
	// resolved line, so no tick can interleave a redraw after it.
	i.stopSpinnerLocked()

	defer i.mu.Unlock()
	if i.tty {
		// Clear the current line, then draw the resolved state with a newline.
		fmt.Fprintf(i.w, "\r\x1b[K%c %s\n", mark, label)
	} else {
		fmt.Fprintf(i.w, "%c %s\n", mark, label)
	}
}

// stopSpinnerLocked signals the redraw goroutine to exit and joins it. It must
// be called with i.mu held and returns with i.mu held. It releases the lock
// only transiently to let the goroutine acquire it and return, avoiding a
// deadlock against a tick blocked on the mutex. It is a no-op when no spinner
// is running. Callers must set the state flags they want (active, finished)
// before calling so a concurrently-blocked tick observes them and does not draw
// after the join.
func (i *Indicator) stopSpinnerLocked() {
	stop := i.stop
	done := i.done
	i.stop = nil
	i.done = nil
	if stop == nil {
		return
	}
	i.mu.Unlock()
	close(stop)
	if done != nil {
		<-done
	}
	i.mu.Lock()
}

// poke records subprocess liveness observed through a livenessWriter. It is
// deliberately non-emitting: the visible spinner is advanced by the ticker in
// Start's redraw goroutine, and writing here would either corrupt a captured
// non-TTY buffer or race the ticker for the terminal line. It exists so a
// livenessWriter has a real Indicator hook to call, and is a no-op once the
// indicator is quiet, finished, or has no active indication, so a build's write
// after Done can never emit anything. Safe for concurrent use.
func (i *Indicator) poke() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.quiet || i.finished || !i.active {
		return
	}
	// No render here on purpose. The ticker owns the animation; poke only marks
	// that liveness was observed, leaving the non-TTY and quiet paths silent.
}

// percentOf returns current*100/total as an integer percentage without
// overflowing when the counts are near the int64 ceiling. It assumes total > 0
// and 0 <= current <= total, which Update guarantees, so the result is in
// [0, 100].
func percentOf(current, total int64) int {
	// The direct multiply is exact and cheapest for realistic byte counts; fall
	// back to divide-first only when current*100 would overflow int64.
	const maxBeforeOverflow = (1<<63 - 1) / 100
	if current <= maxBeforeOverflow {
		return int(current * 100 / total)
	}
	return int(current / (total / 100))
}

// drawLocked renders the current spinner frame with a carriage return. The
// caller must hold i.mu and must have verified the TTY path.
func (i *Indicator) drawLocked() {
	frame := spinnerFrames[i.frame%len(spinnerFrames)]
	fmt.Fprintf(i.w, "\r\x1b[K%c %s", frame, i.label)
}

// barWidth is the number of cells in the determinate bar drawn on the TTY.
const barWidth = 20

// Update reports determinate progress: current of total units complete. On the
// TTY path it redraws a percentage bar in place; on the non-TTY path it emits
// throttled plain percentage lines. A total of zero renders 0% without
// dividing by zero. Update does not require a prior Start, and is a no-op once
// the indicator has been resolved by Done or Fail.
func (i *Indicator) Update(current, total int64) {
	i.mu.Lock()
	if i.quiet || i.finished {
		i.mu.Unlock()
		return
	}
	// A determinate Update supersedes an indeterminate spinner: mark it inactive
	// and stop the redraw goroutine (if Start launched one) before rendering the
	// bar so the two do not race for the same line. stopSpinnerLocked releases
	// and reacquires the lock to join the goroutine, so all shared state is read
	// after it returns.
	i.active = false
	i.stopSpinnerLocked()
	defer i.mu.Unlock()
	if i.finished {
		return
	}

	percent := 0
	if total > 0 {
		if current > total {
			current = total
		}
		if current < 0 {
			current = 0
		}
		percent = percentOf(current, total)
	}

	if i.tty {
		filled := percent * barWidth / 100
		if filled > barWidth {
			filled = barWidth
		}
		bar := strings.Repeat("#", filled) + strings.Repeat("-", barWidth-filled)
		fmt.Fprintf(i.w, "\r\x1b[K[%s] %3d%%", bar, percent)
		return
	}

	// Non-TTY: throttle plain lines and only emit on a changed percentage so a
	// tight Update loop does not flood a log.
	now := time.Now()
	first := i.lastEmit.IsZero()
	if !first && percent == i.lastPercent {
		return
	}
	if !first && percent != 100 && now.Sub(i.lastEmit) < throttleInterval {
		return
	}
	fmt.Fprintf(i.w, "%d%%\n", percent)
	i.lastEmit = now
	i.lastPercent = percent
}
