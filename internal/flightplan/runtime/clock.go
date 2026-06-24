package runtime

import "time"

// Clock supplies the single launch-time read for dynamic inputs (now/today).
// It is injected so the resolution phase is deterministic in tests: the
// runtime reads the clock ONCE at the launch boundary and reuses that value
// for every dynamic input, so two steps reading the same dynamic input see
// one value (#1523 boundary-straddle guarantee).
type Clock interface {
	Now() time.Time
}

// SystemClock reads the wall clock in UTC. Production launches use it.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock returns a fixed time. It is the deterministic test clock and the
// pin point for the determinism property test.
type FixedClock struct{ T time.Time }

// Now returns the fixed time.
func (c FixedClock) Now() time.Time { return c.T }
