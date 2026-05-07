package spawn_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/daemon/spawn"
)

// fakeDaemon binds an ephemeral port and writes daemon.json so
// readAlive's TCP probe succeeds. The returned cleanup closes the
// listener; tests defer it to avoid leaking sockets.
func fakeDaemon(t *testing.T, stateDir string) (url string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url = "http://" + ln.Addr().String()
	if err := discovery.Write(stateDir, discovery.Info{
		URL:       url,
		PID:       12345,
		Version:   "test",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		_ = ln.Close()
		t.Fatalf("discovery.Write: %v", err)
	}
	return url, func() { _ = ln.Close() }
}

func TestResolve_AILERON_API_URL_BypassesEverything(t *testing.T) {
	dir := t.TempDir() // empty — no daemon.json
	got, err := spawn.Resolve(context.Background(), spawn.Options{
		StateDir:  dir,
		EnvLookup: func(k string) string { return map[string]string{"AILERON_API_URL": "http://override:9999"}[k] },
		SpawnFn: func(context.Context, string) error {
			t.Error("SpawnFn should not be called when AILERON_API_URL is set")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "http://override:9999" {
		t.Fatalf("got %q, want override URL", got)
	}
}

func TestResolve_FastPathWhenDaemonAlive(t *testing.T) {
	dir := t.TempDir()
	want, cleanup := fakeDaemon(t, dir)
	defer cleanup()

	got, err := spawn.Resolve(context.Background(), spawn.Options{
		StateDir:  dir,
		EnvLookup: emptyEnv,
		SpawnFn: func(context.Context, string) error {
			t.Error("SpawnFn should not be called when daemon is already alive")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolve_SpawnsAndReturnsURL(t *testing.T) {
	dir := t.TempDir()
	var spawned atomic.Int32

	got, err := spawn.Resolve(context.Background(), spawn.Options{
		StateDir:     dir,
		EnvLookup:    emptyEnv,
		SpawnTimeout: 2 * time.Second,
		SpawnFn: func(_ context.Context, stateDir string) error {
			spawned.Add(1)
			// Simulate the daemon publishing daemon.json after a short delay.
			go func() {
				time.Sleep(20 * time.Millisecond)
				_, _ = fakeDaemon(t, stateDir)
			}()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == "" {
		t.Fatal("got empty URL")
	}
	if spawned.Load() != 1 {
		t.Errorf("SpawnFn called %d times, want 1", spawned.Load())
	}
}

func TestResolve_StaleDiscoveryTriggersRespawn(t *testing.T) {
	dir := t.TempDir()
	// Write daemon.json pointing at a port that nothing is listening on.
	if err := discovery.Write(dir, discovery.Info{
		URL:       "http://127.0.0.1:1", // port 1 — privileged, nothing answers
		PID:       1,
		Version:   "stale",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	var spawned atomic.Int32
	got, err := spawn.Resolve(context.Background(), spawn.Options{
		StateDir:        dir,
		EnvLookup:       emptyEnv,
		LivenessTimeout: 50 * time.Millisecond,
		SpawnTimeout:    2 * time.Second,
		SpawnFn: func(_ context.Context, stateDir string) error {
			spawned.Add(1)
			_, _ = fakeDaemon(t, stateDir)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == "" {
		t.Fatal("got empty URL")
	}
	if spawned.Load() != 1 {
		t.Errorf("SpawnFn called %d times, want 1", spawned.Load())
	}
}

// TestResolve_ConcurrentClientsSpawnExactlyOnce mimics the real
// auto-spawn race: ten clients call Resolve simultaneously against
// an empty state dir. The lock + recheck pattern must funnel all of
// them through one SpawnFn call.
func TestResolve_ConcurrentClientsSpawnExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	var spawned atomic.Int32
	var publishOnce sync.Once

	const N = 10
	results := make(chan string, N)
	errs := make(chan error, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			got, err := spawn.Resolve(context.Background(), spawn.Options{
				StateDir:     dir,
				EnvLookup:    emptyEnv,
				SpawnTimeout: 5 * time.Second,
				SpawnFn: func(_ context.Context, stateDir string) error {
					spawned.Add(1)
					publishOnce.Do(func() { _, _ = fakeDaemon(t, stateDir) })
					return nil
				},
			})
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("Resolve: %v", err)
	}
	var first string
	count := 0
	for got := range results {
		if first == "" {
			first = got
		}
		if got != first {
			t.Fatalf("client got %q, expected the same %q", got, first)
		}
		count++
	}
	if count != N {
		t.Fatalf("got %d successful results, want %d", count, N)
	}
	if got := spawned.Load(); got != 1 {
		t.Fatalf("SpawnFn called %d times, want 1", got)
	}
}

func TestResolve_TimeoutWhenDaemonNeverAppears(t *testing.T) {
	dir := t.TempDir()
	got, err := spawn.Resolve(context.Background(), spawn.Options{
		StateDir:     dir,
		EnvLookup:    emptyEnv,
		SpawnTimeout: 200 * time.Millisecond,
		PollInterval: 25 * time.Millisecond,
		SpawnFn: func(context.Context, string) error {
			return nil // intentionally do nothing — daemon.json never appears
		},
	})
	if err == nil {
		t.Fatalf("Resolve should time out, got URL %q", got)
	}
}

func TestResolve_PropagatesSpawnError(t *testing.T) {
	dir := t.TempDir()
	wantErr := errors.New("spawn-failed-on-purpose")
	_, err := spawn.Resolve(context.Background(), spawn.Options{
		StateDir:  dir,
		EnvLookup: emptyEnv,
		SpawnFn:   func(context.Context, string) error { return wantErr },
	})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("got %v, want %v wrapped", err, wantErr)
	}
}

func TestResolve_CtxCancellationDuringPoll(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan error, 1)
	go func() {
		_, err := spawn.Resolve(ctx, spawn.Options{
			StateDir:     dir,
			EnvLookup:    emptyEnv,
			SpawnTimeout: 5 * time.Second,
			PollInterval: 25 * time.Millisecond,
			SpawnFn:      func(context.Context, string) error { return nil },
		})
		resultCh <- err
	}()

	// Let Resolve enter the polling loop, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve did not return after context cancel")
	}
}

func TestResolve_RequiresStateDir(t *testing.T) {
	_, err := spawn.Resolve(context.Background(), spawn.Options{StateDir: ""})
	if err == nil {
		t.Fatal("Resolve should fail when StateDir is empty")
	}
}

func TestResolve_DefaultSpawnFnFailsWithEmptyBinary(t *testing.T) {
	dir := t.TempDir()
	_, err := spawn.Resolve(context.Background(), spawn.Options{
		StateDir:     dir,
		EnvLookup:    emptyEnv,
		Binary:       "", // missing
		SpawnTimeout: 100 * time.Millisecond,
	})
	if err == nil || !errors.Is(err, spawn.ErrNoBinary) {
		t.Fatalf("got %v, want ErrNoBinary wrapped", err)
	}
}

func TestResolve_DefaultSpawnFnForkExecsBinary(t *testing.T) {
	dir := t.TempDir()
	// Use /bin/true (exits 0 immediately) as the "daemon" binary —
	// it won't write daemon.json, so Resolve will time out, but the
	// fork-exec itself happens. Verifies the default SpawnFn wires
	// the binary path through correctly without depending on a real
	// daemon binary in tests.
	_, err := spawn.Resolve(context.Background(), spawn.Options{
		StateDir:     dir,
		EnvLookup:    emptyEnv,
		Binary:       "/bin/true",
		SpawnTimeout: 200 * time.Millisecond,
		PollInterval: 50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout because /bin/true does not write daemon.json")
	}
}

// TestResolve_ReleasesLockBeforeWaitForDaemon is the regression test
// for the spawn/daemon deadlock that surfaced while running the
// ADR-0012 manual acceptance tests on a fresh machine.
//
// The bug: the helper held [discovery.Lock] through both SpawnFn and
// the subsequent waitForDaemon polling loop. The daemon child (in
// `internal/server.run`) tries to acquire the same lock for its own
// singleton check with a 250ms timeout. Holding the lock through
// waitForDaemon caused the daemon to time out, exit, and never
// publish daemon.json — the helper then timed out at SpawnTimeout
// with the misleading "daemon did not become ready within Ns".
//
// The contract the daemon depends on: by the time waitForDaemon is
// polling, the spawn helper has released the lock so the daemon can
// take it for itself. This test asserts that contract by attempting
// to acquire the same lock from a goroutine started during SpawnFn
// — which mirrors what the real daemon does at startup.
func TestResolve_ReleasesLockBeforeWaitForDaemon(t *testing.T) {
	dir := t.TempDir()

	// Synthetic "daemon" goroutine: tries to grab the lock (the way
	// the real daemon does), then writes daemon.json so Resolve's
	// polling completes.
	daemonStarted := make(chan struct{})
	daemonLockErr := make(chan error, 1)

	spawnFn := func(_ context.Context, stateDir string) error {
		go func() {
			// Wait for the helper to enter waitForDaemon. A small
			// delay is enough — the helper releases the lock between
			// SpawnFn returning and the first poll iteration.
			time.Sleep(50 * time.Millisecond)
			lockCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			release, err := discovery.Lock(lockCtx, stateDir)
			daemonLockErr <- err
			if err != nil {
				return
			}
			defer func() { _ = release() }()
			// Stand in for the daemon's listen + discovery.Write.
			_, _ = fakeDaemon(t, stateDir)
			close(daemonStarted)
		}()
		return nil
	}

	got, err := spawn.Resolve(context.Background(), spawn.Options{
		StateDir:     dir,
		EnvLookup:    emptyEnv,
		SpawnTimeout: 3 * time.Second,
		PollInterval: 25 * time.Millisecond,
		SpawnFn:      spawnFn,
	})

	// Whether Resolve returns success or not, the daemon's lock attempt
	// must succeed — that's the contract under test.
	select {
	case lockErr := <-daemonLockErr:
		if lockErr != nil {
			t.Fatalf("daemon-side Lock acquisition failed (spawn helper still holding it?): %v", lockErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon goroutine never reported its lock attempt — Resolve probably blocked forever")
	}

	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == "" {
		t.Fatal("got empty URL")
	}
	select {
	case <-daemonStarted:
	default:
		t.Error("daemon goroutine never wrote daemon.json — Resolve returned but the synthetic daemon didn't run")
	}
}

func emptyEnv(string) string { return "" }
