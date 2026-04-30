package retry

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/clock"
	"github.com/ALRubinger/aileron/internal/failure"
)

// retry.Do contract:
//   - Op called once on success; returns immediately.
//   - Retriable failure (NetworkError or ExternalAPIError 5xx/429) is
//     retried up to MaxRetries times; final failure carries
//     details.retried = MaxRetries.
//   - Non-retriable failure: no retry; details.retried not set.
//   - Other classes (ConnectorRuntimeError etc.) even if marked
//     retriable: not retried by this primitive.
//   - Context cancellation aborts retry mid-sleep with a NetworkError
//     wrapping the cancellation.

func TestDo_SuccessOnFirstTry(t *testing.T) {
	var calls int
	v, f := Do(context.Background(), DefaultPolicy(), clock.System{}, func(_ context.Context, attempt int) (string, *failure.Failure) {
		calls++
		return "ok", nil
	})
	if f != nil {
		t.Fatalf("Do returned failure: %v", f)
	}
	if v != "ok" {
		t.Errorf("v = %q, want ok", v)
	}
	if calls != 1 {
		t.Errorf("called %d times, want 1", calls)
	}
}

func TestDo_RetriesNetworkErrorThenSucceeds(t *testing.T) {
	c := clock.NewFake(time.Time{})
	policy := Policy{MaxRetries: 3, BaseDelay: time.Second, Jitter: 0}
	var calls int
	op := func(_ context.Context, attempt int) (string, *failure.Failure) {
		calls++
		if calls < 3 {
			return "", failure.Network("dial timeout")
		}
		return "ok", nil
	}
	// Run Do on a goroutine; Advance to release the fake's After channels.
	resultCh := make(chan struct {
		v string
		f *failure.Failure
	})
	go func() {
		v, f := Do(context.Background(), policy, c, op)
		resultCh <- struct {
			v string
			f *failure.Failure
		}{v, f}
	}()
	// First retry waits 1s; second retry waits 2s. Wait briefly between
	// advances so the goroutine has a chance to schedule the next After.
	for i := 0; i < 2; i++ {
		waitForPending(t, c, 1)
		c.Advance(10 * time.Second)
	}
	res := <-resultCh
	if res.f != nil {
		t.Fatalf("got failure: %v", res.f)
	}
	if res.v != "ok" {
		t.Errorf("v = %q", res.v)
	}
	if calls != 3 {
		t.Errorf("called %d times, want 3", calls)
	}
}

func TestDo_RetriesUpstream5xxThenSucceeds(t *testing.T) {
	c := clock.NewFake(time.Time{})
	policy := Policy{MaxRetries: 3, BaseDelay: time.Second, Jitter: 0}
	var calls int
	op := func(_ context.Context, _ int) (int, *failure.Failure) {
		calls++
		if calls == 1 {
			return 0, failure.Upstream(503, "service unavailable")
		}
		return 200, nil
	}
	resultCh := runAsync(c, policy, op)
	waitForPending(t, c, 1)
	c.Advance(2 * time.Second)
	res := <-resultCh
	if res.f != nil {
		t.Fatalf("failure = %v", res.f)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestDo_DoesNotRetryNon4xx_5xx(t *testing.T) {
	policy := Policy{MaxRetries: 3, BaseDelay: time.Second, Jitter: 0}
	var calls int
	op := func(_ context.Context, _ int) (int, *failure.Failure) {
		calls++
		return 0, failure.Upstream(404, "not found")
	}
	_, f := Do(context.Background(), policy, clock.System{}, op)
	if f == nil {
		t.Fatal("expected failure")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (404 is not retriable)", calls)
	}
	if _, ok := f.Details()["retried"]; ok {
		t.Error("details.retried set on a non-retried failure")
	}
}

func TestDo_DoesNotRetryNonRetriableClasses(t *testing.T) {
	policy := Policy{MaxRetries: 3, BaseDelay: time.Second, Jitter: 0}
	cases := map[string]*failure.Failure{
		"capability_denied":       failure.CapabilityDeniedAt(failure.Action, "denied"),
		"binding_required":        failure.BindingRequiredFailure("bind"),
		"resource_limit_exceeded": failure.ResourceLimitFailure("oom"),
		"hash_mismatch":           failure.HashMismatchFailure("bad hash"),
		"signature_failure":       failure.SignatureFailureFailure("bad sig"),
		// connector_runtime_error marked retriable: still skipped by
		// retry.Do because it's not network/external.
		"connector_runtime_retriable": failure.ConnectorRuntime("transient", true),
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			var calls int
			_, got := Do(context.Background(), policy, clock.System{}, func(_ context.Context, _ int) (any, *failure.Failure) {
				calls++
				return nil, f
			})
			if got != f {
				t.Errorf("returned different failure: %v", got)
			}
			if calls != 1 {
				t.Errorf("calls = %d, want 1 (class %q not retriable by retry.Do)", calls, f.Class())
			}
		})
	}
}

func TestDo_StampsRetriedCountOnFinalFailure(t *testing.T) {
	c := clock.NewFake(time.Time{})
	policy := Policy{MaxRetries: 3, BaseDelay: time.Second, Jitter: 0}
	op := func(_ context.Context, _ int) (any, *failure.Failure) {
		return nil, failure.Network("always failing")
	}
	resultCh := runAsync(c, policy, op)
	for i := 0; i < 3; i++ {
		waitForPending(t, c, 1)
		c.Advance(10 * time.Second)
	}
	res := <-resultCh
	if res.f == nil {
		t.Fatal("expected failure after exhausting retries")
	}
	got, ok := res.f.Details()["retried"].(int)
	if !ok {
		t.Fatalf("details.retried missing or wrong type: %v", res.f.Details()["retried"])
	}
	if got != 3 {
		t.Errorf("details.retried = %d, want 3", got)
	}
}

func TestDo_ContextCancelDuringSleep(t *testing.T) {
	c := clock.NewFake(time.Time{})
	policy := Policy{MaxRetries: 3, BaseDelay: time.Second, Jitter: 0}
	ctx, cancel := context.WithCancel(context.Background())
	op := func(_ context.Context, _ int) (any, *failure.Failure) {
		return nil, failure.Network("flaky")
	}
	type result struct {
		f *failure.Failure
	}
	out := make(chan result, 1)
	go func() {
		_, f := Do(ctx, policy, c, op)
		out <- result{f}
	}()
	waitForPending(t, c, 1)
	cancel()
	res := <-out
	if res.f == nil {
		t.Fatal("expected failure on cancel")
	}
	if res.f.Class() != failure.NetworkError {
		t.Errorf("class = %q, want network_error", res.f.Class())
	}
}

func TestDo_ZeroMaxRetries_OnlyAttemptsOnce(t *testing.T) {
	policy := Policy{MaxRetries: 0, BaseDelay: time.Second, Jitter: 0}
	var calls int
	_, f := Do(context.Background(), policy, clock.System{}, func(_ context.Context, _ int) (any, *failure.Failure) {
		calls++
		return nil, failure.Network("flaky")
	})
	if f == nil {
		t.Fatal("expected failure")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestBackoff_DoublesPerAttempt(t *testing.T) {
	p := Policy{BaseDelay: time.Second, Jitter: 0}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
	}
	for _, tc := range cases {
		got := backoff(p, tc.attempt)
		if got != tc.want {
			t.Errorf("backoff(attempt=%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestBackoff_AppliesJitter(t *testing.T) {
	p := Policy{BaseDelay: time.Second, Jitter: 0.5}
	got := backoff(p, 1)
	// Jitter expands the delay to [1, 1.5)s.
	if got < time.Second || got >= time.Second*3/2 {
		t.Errorf("jittered backoff out of range: %v", got)
	}
}

func TestBackoff_CapsExponent(t *testing.T) {
	p := Policy{BaseDelay: time.Second, Jitter: 0}
	// attempt = 100 would normally overflow; the cap at 10 keeps it sane.
	got := backoff(p, 100)
	want := time.Second << 10
	if got != want {
		t.Errorf("backoff caps at 2^10 BaseDelay; got %v, want %v", got, want)
	}
}

func TestBackoff_ZeroAttempt_TreatsAsFirst(t *testing.T) {
	p := Policy{BaseDelay: time.Second, Jitter: 0}
	got := backoff(p, 0)
	if got != time.Second {
		t.Errorf("backoff(0) = %v, want 1s", got)
	}
}

func TestCtxFailure_DeadlineExceeded(t *testing.T) {
	f := ctxFailure(context.DeadlineExceeded)
	if f.Class() != failure.NetworkError {
		t.Errorf("class = %q, want network_error", f.Class())
	}
}

func TestDo_NormalisesNegativePolicy(t *testing.T) {
	policy := Policy{MaxRetries: -5, BaseDelay: -time.Second, Jitter: 0}
	var calls int
	_, f := Do(context.Background(), policy, clock.System{}, func(_ context.Context, _ int) (int, *failure.Failure) {
		calls++
		return 0, failure.Network("flaky")
	})
	if f == nil {
		t.Fatal("expected failure")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (negative MaxRetries clamped to 0)", calls)
	}
}

func TestDefaultPolicy_MatchesADR0010(t *testing.T) {
	p := DefaultPolicy()
	if p.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", p.MaxRetries)
	}
	if p.BaseDelay != time.Second {
		t.Errorf("BaseDelay = %v, want 1s", p.BaseDelay)
	}
	if p.Jitter <= 0 || p.Jitter > 1 {
		t.Errorf("Jitter = %v, want in (0,1]", p.Jitter)
	}
}

// --- helpers ---

type asyncResult[T any] struct {
	v T
	f *failure.Failure
}

// runAsync runs Do on a goroutine using the supplied clock and
// returns a channel for the result. Tests advance the clock to
// release retry sleeps.
func runAsync[T any](c clock.Clock, policy Policy, op Op[T]) <-chan asyncResult[T] {
	out := make(chan asyncResult[T], 1)
	go func() {
		v, f := Do(context.Background(), policy, c, op)
		out <- asyncResult[T]{v, f}
	}()
	return out
}

// waitForPending busy-spins (with a timeout) until the fake clock
// has at least n pending timers. Used to coordinate between the
// goroutine running Do (which schedules After) and the test
// (which advances the fake).
func waitForPending(t *testing.T, c *clock.Fake, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Pending() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("clock had %d pending timers, want ≥ %d (deadlocked?)", c.Pending(), n)
}
