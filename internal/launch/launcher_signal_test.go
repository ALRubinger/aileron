package launch

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// signalTestHarness swaps the launchSandbox test seams for fakes and
// restores them on cleanup. It models a foreground container that
// blocks until either a clean exit is requested or a SIGINT/SIGTERM is
// delivered through the launcher's own signal channel.
type signalTestHarness struct {
	t *testing.T

	// runStarted closes once sandboxRunContainer is entered, so the
	// test can deterministically deliver a signal mid-run.
	runStarted chan struct{}
	// release lets the test unblock the simulated container run. The
	// run returns runErr (nil for a clean exit, non-nil to model the
	// runtime force-killing the container after a signal).
	release chan struct{}
	runErr  error

	// sigCh is the channel the launcher registered via
	// sandboxSignalNotify; the harness captures it so deliverSignal can
	// push a synthetic SIGINT exactly as the OS would.
	mu    sync.Mutex
	sigCh chan<- os.Signal

	stopCalls atomic.Int32
	stopName  atomic.Value // string
	stopErr   error
}

func newSignalTestHarness(t *testing.T) *signalTestHarness {
	t.Helper()
	h := &signalTestHarness{
		t:          t,
		runStarted: make(chan struct{}),
		release:    make(chan struct{}),
	}

	origRun := sandboxRunContainer
	origStop := sandboxStopContainer
	origNotify := sandboxSignalNotify
	origStop2 := sandboxSignalStop
	t.Cleanup(func() {
		sandboxRunContainer = origRun
		sandboxStopContainer = origStop
		sandboxSignalNotify = origNotify
		sandboxSignalStop = origStop2
	})

	var startOnce sync.Once
	sandboxRunContainer = func(_ context.Context, _ string, _, _ io.Writer, _ sandboxcontainer.RunOptions) (sandboxcontainer.RunResult, error) {
		startOnce.Do(func() { close(h.runStarted) })
		<-h.release
		return sandboxcontainer.RunResult{}, h.runErr
	}
	sandboxStopContainer = func(_ context.Context, _, name string, _ int, _, _ io.Writer) error {
		h.stopCalls.Add(1)
		h.stopName.Store(name)
		return h.stopErr
	}
	sandboxSignalNotify = func(ch chan<- os.Signal, _ ...os.Signal) {
		h.mu.Lock()
		h.sigCh = ch
		h.mu.Unlock()
	}
	sandboxSignalStop = func(chan<- os.Signal) {}

	return h
}

// deliverSignal pushes a SIGINT onto the launcher's registered channel,
// then unblocks the simulated run so it returns a force-kill error.
func (h *signalTestHarness) deliverSignal() {
	h.t.Helper()
	<-h.runStarted
	h.mu.Lock()
	ch := h.sigCh
	h.mu.Unlock()
	if ch == nil {
		h.t.Fatal("launcher did not register a signal channel")
	}
	ch <- syscall.SIGINT
	h.runErr = errors.New("container killed")
	close(h.release)
}

// cleanExit unblocks the simulated run so it returns nil.
func (h *signalTestHarness) cleanExit() {
	<-h.runStarted
	h.runErr = nil
	close(h.release)
}

func runSalvageLaunchSandbox(t *testing.T, captureOnce func()) (LaunchResult, error) {
	t.Helper()
	plan := SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}
	cfg := LaunchConfig{Agent: namedBinaryAgent{name: "claude"}}
	return launchSandbox(context.Background(), plan, cfg, map[string]string{}, "aileron-sbx-test", captureOnce, nil)
}

// TestLaunchSandboxSignalSalvagesCapture is the core regression: a
// SIGINT mid-run force-kills the container (Run returns non-nil), yet
// the salvage path stops the named container and fires Capture exactly
// once. Before the fix the clean-exit gate's runErr==nil guard would
// skip Capture and the in-container rotation would be lost.
func TestLaunchSandboxSignalSalvagesCapture(t *testing.T) {
	h := newSignalTestHarness(t)
	var captures atomic.Int32
	once := newCaptureOnce(func() { captures.Add(1) })

	go h.deliverSignal()
	_, err := runSalvageLaunchSandbox(t, once)
	if err == nil {
		t.Fatal("expected a non-nil run error after the simulated force-kill")
	}
	// The clean-exit gate only fires on runErr==nil; here runErr is
	// non-nil, so Capture must have come from the salvage path.
	if got := captures.Load(); got != 1 {
		t.Fatalf("Capture fired %d times, want exactly 1 (salvage path)", got)
	}
	if h.stopCalls.Load() != 1 {
		t.Fatalf("stop issued %d times, want exactly 1", h.stopCalls.Load())
	}
	if name, _ := h.stopName.Load().(string); name != "aileron-sbx-test" {
		t.Fatalf("stop targeted %q, want aileron-sbx-test", name)
	}
}

// TestLaunchSandboxCleanExitDoesNotDoubleCapture verifies the clean
// exit path issues no stop and leaves Capture to the caller's gate
// (launchSandbox itself must not double-fire it).
func TestLaunchSandboxCleanExitDoesNotStopOrCapture(t *testing.T) {
	h := newSignalTestHarness(t)
	var captures atomic.Int32
	once := newCaptureOnce(func() { captures.Add(1) })

	go h.cleanExit()
	res, err := runSalvageLaunchSandbox(t, once)
	if err != nil {
		t.Fatalf("clean exit returned error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("clean exit code = %d, want 0", res.ExitCode)
	}
	if h.stopCalls.Load() != 0 {
		t.Fatalf("clean exit issued %d stops, want 0", h.stopCalls.Load())
	}
	if got := captures.Load(); got != 0 {
		t.Fatalf("launchSandbox fired Capture %d times on clean exit; the caller's gate owns it", got)
	}
}

// TestLaunchSandboxSignalThenCleanRaceCapturesOnce models the
// signal-then-clean-exit race: the salvage path and the clean-exit gate
// both call the same once-guarded Capture, so the vault write lands
// exactly once.
func TestLaunchSandboxSignalThenCleanRaceCapturesOnce(t *testing.T) {
	h := newSignalTestHarness(t)
	var captures atomic.Int32
	once := newCaptureOnce(func() { captures.Add(1) })

	go h.deliverSignal()
	_, _ = runSalvageLaunchSandbox(t, once)
	// Simulate the clean-exit gate also calling the same wrapper.
	once()
	if got := captures.Load(); got != 1 {
		t.Fatalf("Capture fired %d times across salvage + clean-exit gate, want 1", got)
	}
}

// TestLaunchSandboxSignalFailedStopStillCaptures verifies a failed
// graceful stop is non-fatal and Capture still runs.
func TestLaunchSandboxSignalFailedStopStillCaptures(t *testing.T) {
	h := newSignalTestHarness(t)
	h.stopErr = errors.New("stop failed")
	var captures atomic.Int32
	once := newCaptureOnce(func() { captures.Add(1) })

	go h.deliverSignal()
	_, err := runSalvageLaunchSandbox(t, once)
	if err == nil {
		t.Fatal("expected non-nil run error after force-kill")
	}
	if got := captures.Load(); got != 1 {
		t.Fatalf("Capture fired %d times after failed stop, want 1", got)
	}
}

// TestLaunchSandboxSignalNilCaptureIsNoOp verifies a nil-Capture salvage
// path (captureOnce wraps a nil CaptureFn) is a safe no-op.
func TestLaunchSandboxSignalNilCaptureIsNoOp(t *testing.T) {
	h := newSignalTestHarness(t)
	// captureOnce that does nothing models a nil CaptureFn.
	noop := func() {}

	go h.deliverSignal()
	_, err := runSalvageLaunchSandbox(t, noop)
	if err == nil {
		t.Fatal("expected non-nil run error after force-kill")
	}
	if h.stopCalls.Load() != 1 {
		t.Fatalf("stop issued %d times, want 1 even with nil Capture", h.stopCalls.Load())
	}
}

// newCaptureOnce mirrors the sync.Once wrapper Launch builds around the
// AuthSpec CaptureFn so the salvage path and the clean-exit gate share
// the exactly-once guard.
func newCaptureOnce(fn func()) func() {
	var once sync.Once
	return func() { once.Do(fn) }
}

// guard against an accidentally unbounded test if a seam is misnamed.
func TestLaunchSandboxSalvageDoesNotHang(t *testing.T) {
	done := make(chan struct{})
	go func() {
		h := newSignalTestHarness(t)
		once := newCaptureOnce(func() {})
		go h.deliverSignal()
		_, _ = runSalvageLaunchSandbox(t, once)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("launchSandbox salvage path hung")
	}
}
