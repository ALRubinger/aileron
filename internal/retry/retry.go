// Package retry implements ADR-0010's bounded-retry policy: at most
// 3 retries with exponential backoff (1s/2s/4s + jitter) for failures
// the [failure] package marks retriable. Network failures and
// upstream 429/5xx responses are the canonical retriable classes;
// everything else fails fast.
//
// The primitive is generic across return types so it can wrap both
// the executor seam and HTTP upstream calls. Callers supply a
// [Policy], a [clock.Clock], and an Op that returns either a value
// or a *failure.Failure. retry.Do handles the loop, the backoff
// scheduling, and the `details.retried = N` annotation on the final
// failure.
package retry

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/ALRubinger/aileron/internal/clock"
	"github.com/ALRubinger/aileron/internal/failure"
)

// DefaultPolicy returns the v1 ADR-0010 default: 3 retries, 1s base
// delay, 20% jitter. Most callers should pass this; a few might tune
// jitter for deterministic tests.
func DefaultPolicy() Policy {
	return Policy{MaxRetries: 3, BaseDelay: time.Second, Jitter: 0.2}
}

// Policy controls how [Do] retries.
type Policy struct {
	// MaxRetries is the upper bound on additional attempts after
	// the first one. ADR-0010 v1 default: 3.
	MaxRetries int

	// BaseDelay is the delay before the first retry. Subsequent
	// retries double from there.
	BaseDelay time.Duration

	// Jitter is the fractional jitter applied to each delay
	// (0.0 = none, 1.0 = up to 100% extra). Default: 0.2.
	Jitter float64
}

// Op is the operation [Do] executes. attempt starts at 1 for the
// first call and increments per retry; useful for callers that want
// to observe the retry count for logging.
type Op[T any] func(ctx context.Context, attempt int) (T, *failure.Failure)

// Do runs op with the given retry policy. On a retriable failure it
// sleeps `BaseDelay << (attempt-1)` (with jitter), up to MaxRetries
// times, then returns the final failure with `details.retried = N`
// stamped on. Non-retriable failures return immediately. Successes
// return on the first non-failing attempt.
//
// retriable predicate: only NetworkError or ExternalAPIError failures
// trip the retry path, and only when their Retriable() flag is true.
// Other classes — even ones marked retriable in their constructor —
// are left to the caller's policy. This keeps the retry surface
// narrow per ADR-0010's "the runtime retries `retriable` errors"
// language combined with the table that names the eligible classes.
func Do[T any](ctx context.Context, p Policy, clk clock.Clock, op Op[T]) (T, *failure.Failure) {
	if p.MaxRetries < 0 {
		p.MaxRetries = 0
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = time.Second
	}
	var zero T
	var last *failure.Failure
	for attempt := 1; attempt <= p.MaxRetries+1; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, ctxFailure(err)
		}
		v, f := op(ctx, attempt)
		if f == nil {
			return v, nil
		}
		last = f
		if !shouldRetry(f) || attempt > p.MaxRetries {
			break
		}
		// Sleep then retry.
		delay := backoff(p, attempt)
		select {
		case <-ctx.Done():
			return zero, ctxFailure(ctx.Err())
		case <-clk.After(delay):
		}
	}
	if last != nil {
		// Stamp retried-count: number of retries actually performed
		// equals attempts-minus-one capped at MaxRetries.
		retries := p.MaxRetries
		if !shouldRetry(last) {
			retries = 0
		}
		if retries > 0 {
			last.SetDetail("retried", retries)
		}
	}
	return zero, last
}

// shouldRetry reports whether a failure is eligible per ADR-0010's
// table: NetworkError always; ExternalAPIError only for 429 / 5xx
// (which the failure constructor already encoded as Retriable()).
func shouldRetry(f *failure.Failure) bool {
	if f == nil || !f.Retriable() {
		return false
	}
	switch f.Class() {
	case failure.NetworkError, failure.ExternalAPIError:
		return true
	}
	return false
}

// backoff returns the delay before retry n, capped to a sensible
// upper bound and with random jitter applied.
//
//	n=1 → BaseDelay              (first retry)
//	n=2 → BaseDelay * 2
//	n=3 → BaseDelay * 4
func backoff(p Policy, attempt int) time.Duration {
	exponent := attempt - 1
	if exponent < 0 {
		exponent = 0
	}
	if exponent > 10 {
		// Cap to avoid overflow in pathological tests.
		exponent = 10
	}
	delay := p.BaseDelay << exponent
	if p.Jitter > 0 {
		// Multiply delay by a random factor in [1, 1+Jitter].
		factor := 1 + rand.Float64()*p.Jitter
		delay = time.Duration(float64(delay) * factor)
	}
	return delay
}

// ctxFailure wraps a context error as a NetworkError. Cancellation
// during retry is treated as a network-side abort so the agent sees
// a structured envelope rather than a raw Go error.
func ctxFailure(err error) *failure.Failure {
	if errors.Is(err, context.Canceled) {
		return failure.Network("context canceled while retrying", failure.WithCause(err))
	}
	return failure.Network("context deadline exceeded while retrying", failure.WithCause(err))
}
