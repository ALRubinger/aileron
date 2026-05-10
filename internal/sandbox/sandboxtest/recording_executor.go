package sandboxtest

import (
	"context"
	"sync"

	"github.com/ALRubinger/aileron/internal/sandbox"
)

// RecordingExecutor implements [sandbox.SpawnExecutor] for tests. It
// records every envelope it sees and returns a scripted result.
// Substitute via [sandbox.WithSpawnExecutor].
//
// Concurrency: safe for parallel use by multiple Invoke goroutines.
//
// Scripted behavior:
//
//	Result   — the SpawnResult to return on every call. Tests can mutate
//	           between calls to script different outcomes for sequential
//	           invocations.
//	Err      — non-nil overrides Result and surfaces as an error from
//	           Spawn. Pair with [sandbox.ErrSpawnUnavailable] to assert
//	           the runtime's spawn_sandbox_unavailable error class.
//	Hook     — optional callback invoked on every Spawn before recording.
//	           Lets tests choose Result/Err per envelope rather than
//	           globally; e.g. emit different stdout for different argv
//	           shapes.
type RecordingExecutor struct {
	Result sandbox.SpawnResult
	Err    error
	Hook   func(env sandbox.SpawnEnvelope) (sandbox.SpawnResult, error)

	mu    sync.Mutex
	calls []sandbox.SpawnEnvelope
}

// Spawn implements sandbox.SpawnExecutor.
func (r *RecordingExecutor) Spawn(_ context.Context, env sandbox.SpawnEnvelope) (sandbox.SpawnResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, env)
	r.mu.Unlock()
	if r.Hook != nil {
		return r.Hook(env)
	}
	return r.Result, r.Err
}

// Calls returns a snapshot of every envelope this executor has seen.
// The returned slice is safe to inspect without holding any lock.
func (r *RecordingExecutor) Calls() []sandbox.SpawnEnvelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sandbox.SpawnEnvelope, len(r.calls))
	copy(out, r.calls)
	return out
}

// CallCount returns the number of Spawn invocations seen so far.
func (r *RecordingExecutor) CallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// LastCall returns the most recent envelope or the zero value when no
// Spawn has been invoked. Convenient when a test only cares about the
// final invocation.
func (r *RecordingExecutor) LastCall() sandbox.SpawnEnvelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return sandbox.SpawnEnvelope{}
	}
	return r.calls[len(r.calls)-1]
}

// Reset clears the recorded calls and any scripted result/error so
// the same executor can be reused across subtests.
func (r *RecordingExecutor) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
	r.Result = sandbox.SpawnResult{}
	r.Err = nil
	r.Hook = nil
}

// Compile-time check.
var _ sandbox.SpawnExecutor = (*RecordingExecutor)(nil)
