// Package clock abstracts wall-clock time for testability. Production
// code uses [System], which delegates to the standard `time` package;
// tests use [Fake], which advances deterministically.
//
// The interface is intentionally narrow — only the operations the
// retry primitive needs (current time and a timer channel). Adding
// methods is a deliberate API decision, not an oversight.
package clock

import (
	"sync"
	"time"
)

// Clock is the minimal time abstraction used by retry/backoff and
// any other code that needs sleep-with-cancellation semantics.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// After returns a channel that fires once after d has elapsed.
	// Equivalent to time.After but routed through the clock so
	// fakes can advance deterministically.
	After(d time.Duration) <-chan time.Time
}

// System is the production [Clock] backed by the real wall clock.
type System struct{}

// Now returns time.Now().
func (System) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (System) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Fake is a deterministic [Clock] for tests. The current time advances
// only when [Fake.Advance] is called; pending timers fire when their
// scheduled time is reached.
//
// Fake is safe for concurrent use: callers may call Advance on one
// goroutine while another waits on a channel returned by After.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

// NewFake returns a [Fake] starting at the given time. Use the zero
// value `time.Time{}` to start from the epoch.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start}
}

// Now returns the fake's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After returns a channel that fires when [Fake.Advance] has moved
// the clock past `now+d`. The channel is buffered (size 1) so the
// fire side never blocks.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{
		at: f.now.Add(d),
		ch: make(chan time.Time, 1),
	}
	if !f.now.Before(t.at) {
		// Zero-duration timer: fire immediately.
		t.ch <- f.now
	} else {
		f.timers = append(f.timers, t)
	}
	return t.ch
}

// Advance moves the fake forward by d. Any pending timers whose
// scheduled time falls within [now, now+d] fire on their channels.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	remaining := f.timers[:0]
	for _, t := range f.timers {
		if !f.now.Before(t.at) {
			t.ch <- f.now
			continue
		}
		remaining = append(remaining, t)
	}
	f.timers = remaining
}

// Pending reports the number of timers waiting to fire. Useful for
// tests that want to assert something was scheduled.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}
