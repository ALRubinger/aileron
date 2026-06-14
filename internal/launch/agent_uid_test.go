package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	goruntime "runtime"
	"strings"
	"testing"
)

// agent_uid.go contract (U1):
//
//   - A numeric Config.User ("1000") is parsed directly, no `id -u` run.
//   - A "user:group" / "uid:gid" spec uses only the user component.
//   - A named Config.User ("agent") triggers a second `id -u <name>`
//     run whose integer output is the resolved UID.
//   - An empty (or "<no value>") Config.User resolves to root (0).
//   - An inspect command failure propagates as an error.
//   - Non-integer `id -u` output is a parse error.

// fakeUIDRunner records the argv of each Run call and returns canned
// stdout per invocation, in order. It substitutes for Docker so
// the resolver is testable without a container runtime.
type fakeUIDRunner struct {
	// outputs is consumed FIFO; each entry is the stdout written for the
	// matching Run call.
	outputs []string
	// errs is consumed FIFO alongside outputs; a non-nil entry makes the
	// matching Run call fail after writing its output.
	errs []error

	calls [][]string
}

func (f *fakeUIDRunner) Run(_ context.Context, name string, args []string, stdout, _ io.Writer) error {
	argv := append([]string{name}, args...)
	f.calls = append(f.calls, argv)
	idx := len(f.calls) - 1
	if idx < len(f.outputs) && f.outputs[idx] != "" {
		_, _ = io.WriteString(stdout, f.outputs[idx])
	}
	if idx < len(f.errs) {
		return f.errs[idx]
	}
	return nil
}

func TestResolveAgentUIDNumericUser(t *testing.T) {
	runner := &fakeUIDRunner{outputs: []string{"1000\n"}}
	uid, err := resolveAgentUID(context.Background(), runner, "docker", "img:test")
	if err != nil {
		t.Fatalf("resolveAgentUID: %v", err)
	}
	if uid != 1000 {
		t.Fatalf("uid = %d, want 1000", uid)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 run (inspect only), got %d: %v", len(runner.calls), runner.calls)
	}
	if got := strings.Join(runner.calls[0], " "); !strings.Contains(got, "image inspect") {
		t.Fatalf("first call should be image inspect, got %q", got)
	}
}

func TestResolveAgentUIDUserGroupForm(t *testing.T) {
	runner := &fakeUIDRunner{outputs: []string{"1000:2000\n"}}
	uid, err := resolveAgentUID(context.Background(), runner, "docker", "img:test")
	if err != nil {
		t.Fatalf("resolveAgentUID: %v", err)
	}
	if uid != 1000 {
		t.Fatalf("uid = %d, want 1000 (user component of uid:gid)", uid)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 run for numeric user:group, got %d", len(runner.calls))
	}
}

func TestResolveAgentUIDNamedUser(t *testing.T) {
	runner := &fakeUIDRunner{outputs: []string{"agent\n", "405\n"}}
	uid, err := resolveAgentUID(context.Background(), runner, "docker", "img:test")
	if err != nil {
		t.Fatalf("resolveAgentUID: %v", err)
	}
	if uid != 405 {
		t.Fatalf("uid = %d, want 405", uid)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 runs (inspect + id -u), got %d: %v", len(runner.calls), runner.calls)
	}
	second := strings.Join(runner.calls[1], " ")
	if !strings.Contains(second, "run --rm img:test id -u agent") {
		t.Fatalf("second call should resolve the named user, got %q", second)
	}
}

func TestResolveAgentUIDEmptyUserIsRoot(t *testing.T) {
	for _, out := range []string{"\n", "<no value>\n", "  "} {
		runner := &fakeUIDRunner{outputs: []string{out}}
		uid, err := resolveAgentUID(context.Background(), runner, "docker", "img:test")
		if err != nil {
			t.Fatalf("resolveAgentUID(%q): %v", out, err)
		}
		if uid != 0 {
			t.Fatalf("uid = %d for empty user %q, want 0 (root)", uid, out)
		}
		if len(runner.calls) != 1 {
			t.Fatalf("empty user should not trigger id -u, got %d calls", len(runner.calls))
		}
	}
}

func TestResolveAgentUIDInspectErrorPropagates(t *testing.T) {
	runner := &fakeUIDRunner{
		outputs: []string{"no such image\n"},
		errs:    []error{errors.New("exit status 1")},
	}
	_, err := resolveAgentUID(context.Background(), runner, "docker", "missing:img")
	if err == nil {
		t.Fatal("expected error when inspect fails")
	}
	if !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("error should name the inspect step, got %v", err)
	}
}

func TestResolveAgentUIDIDErrorPropagates(t *testing.T) {
	runner := &fakeUIDRunner{
		outputs: []string{"agent\n", "id: no such user\n"},
		errs:    []error{nil, errors.New("exit status 1")},
	}
	_, err := resolveAgentUID(context.Background(), runner, "docker", "img:test")
	if err == nil {
		t.Fatal("expected error when id -u fails")
	}
	if !strings.Contains(err.Error(), "id -u") {
		t.Fatalf("error should name the id -u step, got %v", err)
	}
}

func TestResolveAgentUIDNonIntegerIDOutput(t *testing.T) {
	runner := &fakeUIDRunner{outputs: []string{"agent\n", "not-a-number\n"}}
	_, err := resolveAgentUID(context.Background(), runner, "docker", "img:test")
	if err == nil {
		t.Fatal("expected parse error for non-integer id -u output")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("error should mention parse failure, got %v", err)
	}
}

func TestResolveAgentUIDCachedHitsCacheAfterFirstResolve(t *testing.T) {
	// Use a unique image so this test never collides with the package-
	// level cache populated by other tests.
	img := "img:cache-" + t.Name()
	t.Cleanup(func() {
		agentUIDCacheMu.Lock()
		delete(agentUIDCache, agentUIDCacheKey{runtime: "docker", image: img})
		agentUIDCacheMu.Unlock()
	})

	runner := &fakeUIDRunner{outputs: []string{"1000\n"}}
	uid, err := resolveAgentUIDCached(context.Background(), runner, "docker", img)
	if err != nil || uid != 1000 {
		t.Fatalf("first resolve: uid=%d err=%v", uid, err)
	}
	// Second call must hit the cache: a runner with no canned output
	// would return an empty inspect (root, 0) on a cache miss, so a
	// cached 1000 proves the cache served it without re-running.
	empty := &fakeUIDRunner{}
	uid, err = resolveAgentUIDCached(context.Background(), empty, "docker", img)
	if err != nil {
		t.Fatalf("cached resolve: %v", err)
	}
	if uid != 1000 {
		t.Fatalf("cached uid = %d, want 1000 (served from cache, not re-resolved)", uid)
	}
	if len(empty.calls) != 0 {
		t.Fatalf("cache hit must not invoke the runner, got %d calls", len(empty.calls))
	}
}

func TestResolveAgentUIDCachedDoesNotCacheFailures(t *testing.T) {
	img := "img:cache-fail-" + t.Name()
	t.Cleanup(func() {
		agentUIDCacheMu.Lock()
		delete(agentUIDCache, agentUIDCacheKey{runtime: "docker", image: img})
		agentUIDCacheMu.Unlock()
	})

	failing := &fakeUIDRunner{
		outputs: []string{"boom\n"},
		errs:    []error{errors.New("exit status 1")},
	}
	if _, err := resolveAgentUIDCached(context.Background(), failing, "docker", img); err == nil {
		t.Fatal("expected error from failing resolve")
	}
	// A failure must not be cached: a subsequent successful resolve must
	// re-run and return the new value.
	ok := &fakeUIDRunner{outputs: []string{"1000\n"}}
	uid, err := resolveAgentUIDCached(context.Background(), ok, "docker", img)
	if err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if uid != 1000 {
		t.Fatalf("uid = %d, want 1000 (retry resolved fresh)", uid)
	}
	if len(ok.calls) == 0 {
		t.Fatal("retry after a failed resolve must re-run the runner")
	}
}

func TestNewAgentDirChownHookNilOnNonLinux(t *testing.T) {
	hook := newAgentDirChownHook(context.Background(), &fakeUIDRunner{}, "docker", "img")
	if goruntime.GOOS == "linux" {
		if hook == nil {
			t.Fatal("hook must be non-nil on Linux for a concrete runtime+image")
		}
	} else if hook != nil {
		t.Fatalf("hook must be nil on %s (Docker Desktop shim translates UIDs)", goruntime.GOOS)
	}

	// Missing runtime/image yields a nil hook on every OS so launch does
	// not emit a spurious chown warning when sandbox prep left them unset.
	if h := newAgentDirChownHook(context.Background(), &fakeUIDRunner{}, "", "img"); h != nil {
		t.Error("hook must be nil when runtime is empty")
	}
	if h := newAgentDirChownHook(context.Background(), &fakeUIDRunner{}, "docker", ""); h != nil {
		t.Error("hook must be nil when image is empty")
	}
}

func TestNewTransientReclaimHookNilConditions(t *testing.T) {
	hook := newTransientReclaimHook(context.Background(), &fakeUIDRunner{}, "docker", "img")
	if goruntime.GOOS == "linux" {
		if hook == nil {
			t.Fatal("reclaim hook must be non-nil on Linux for a concrete runtime+image")
		}
	} else if hook != nil {
		t.Fatalf("reclaim hook must be nil on %s (the chown is only needed on Linux)", goruntime.GOOS)
	}
	if h := newTransientReclaimHook(context.Background(), &fakeUIDRunner{}, "", "img"); h != nil {
		t.Error("reclaim hook must be nil when runtime is empty")
	}
	if h := newTransientReclaimHook(context.Background(), &fakeUIDRunner{}, "docker", ""); h != nil {
		t.Error("reclaim hook must be nil when image is empty")
	}
}

// TestChownHooksDelegateToRuntimeOnLinux drives both the forward chown
// hook and the reclaim hook (Linux-only, where they are non-nil) and
// asserts they delegate to the privileged runtime chown rather than a
// host-side os.Chown — the whole point of the fix.
func TestChownHooksDelegateToRuntimeOnLinux(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skipf("chown hooks are only non-nil on Linux; GOOS=%s", goruntime.GOOS)
	}

	// Forward hook: a numeric image user resolves without an id -u run,
	// then the closure delegates the chown to the runtime as the agent UID.
	img := "img:chownhook-" + t.Name()
	t.Cleanup(func() {
		agentUIDCacheMu.Lock()
		delete(agentUIDCache, agentUIDCacheKey{runtime: "docker", image: img})
		agentUIDCacheMu.Unlock()
	})
	runner := &fakeUIDRunner{outputs: []string{"1000\n"}}
	hook := newAgentDirChownHook(context.Background(), runner, "docker", img)
	if hook == nil {
		t.Fatal("expected non-nil forward hook on Linux")
	}
	if err := hook("/host/transient"); err != nil {
		t.Fatalf("forward chown hook: %v", err)
	}
	last := strings.Join(runner.calls[len(runner.calls)-1], " ")
	if !strings.Contains(last, "--entrypoint chown") || !strings.Contains(last, "-R 1000 /mnt") {
		t.Errorf("forward hook must delegate to the runtime chown as the agent UID; last call %q", last)
	}

	// A root image (empty USER) resolves to UID 0, so the chown is skipped
	// entirely (root already owns host-created files) — no chown run.
	rootImg := "img:roothook-" + t.Name()
	t.Cleanup(func() {
		agentUIDCacheMu.Lock()
		delete(agentUIDCache, agentUIDCacheKey{runtime: "docker", image: rootImg})
		agentUIDCacheMu.Unlock()
	})
	rootRunner := &fakeUIDRunner{outputs: []string{"\n"}}
	rootHook := newAgentDirChownHook(context.Background(), rootRunner, "docker", rootImg)
	if err := rootHook("/host/transient"); err != nil {
		t.Fatalf("root-image chown hook: %v", err)
	}
	if len(rootRunner.calls) != 1 {
		t.Errorf("root image must only inspect (UID 0 skips the chown), got %d calls: %v", len(rootRunner.calls), rootRunner.calls)
	}

	// Reclaim hook: chowns back to the host operator's UID via the runtime.
	reclaimRunner := &fakeUIDRunner{}
	reclaim := newTransientReclaimHook(context.Background(), reclaimRunner, "docker", img)
	if reclaim == nil {
		t.Fatal("expected non-nil reclaim hook on Linux")
	}
	if err := reclaim("/host/transient"); err != nil {
		t.Fatalf("reclaim hook: %v", err)
	}
	rc := strings.Join(reclaimRunner.calls[0], " ")
	if !strings.Contains(rc, "--entrypoint chown") || !strings.Contains(rc, fmt.Sprintf("-R %d /mnt", os.Getuid())) {
		t.Errorf("reclaim hook must chown back to the host UID via the runtime; call %q", rc)
	}
}

// TestAgentDirChownHookSurfacesResolverError covers the forward-hook
// closure's resolver-error branch (agent_uid.go): when
// resolveAgentUIDCached fails inside the closure (e.g. a transient
// inspect / `id -u` failure on a live launch), the closure must
// propagate that error — so prepareAuthSpec warns and continues — and
// must NOT attempt a chown. Linux-only, where the hook is non-nil.
func TestAgentDirChownHookSurfacesResolverError(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skipf("chown hooks are only non-nil on Linux; GOOS=%s", goruntime.GOOS)
	}

	// Unique image so the failure never lands in (or reads from) a cache
	// entry shared with another test; the cleanup evicts it regardless.
	img := "img:resolverr-" + t.Name()
	t.Cleanup(func() {
		agentUIDCacheMu.Lock()
		delete(agentUIDCache, agentUIDCacheKey{runtime: "docker", image: img})
		agentUIDCacheMu.Unlock()
	})

	// The first (inspect) call fails, so the resolver returns an error
	// before any UID is known and the closure must bail out.
	runner := &fakeUIDRunner{
		outputs: []string{"no such image\n"},
		errs:    []error{errors.New("exit status 1")},
	}
	hook := newAgentDirChownHook(context.Background(), runner, "docker", img)
	if hook == nil {
		t.Fatal("expected non-nil forward hook on Linux")
	}

	err := hook("/host/transient")
	if err == nil {
		t.Fatal("expected the closure to surface the resolver error")
	}
	if !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("error should name the failing resolver step (inspect), got %v", err)
	}
	// Only the inspect call may have run; a resolver failure must not be
	// followed by a chown run.
	if len(runner.calls) != 1 {
		t.Fatalf("resolver failure must not trigger a chown; got %d calls: %v", len(runner.calls), runner.calls)
	}
	for _, call := range runner.calls {
		if got := strings.Join(call, " "); strings.Contains(got, "--entrypoint chown") {
			t.Fatalf("no chown run may be issued after a resolver failure; saw %q", got)
		}
	}
}

func TestChownTreeViaRuntimeRunsPrivilegedChown(t *testing.T) {
	// The host launcher is unprivileged, so the chown is delegated to a
	// root container that bind-mounts the tree. Assert the argv encodes
	// that contract: a one-shot run as --user 0 with chown as the
	// entrypoint and the tree mounted at /mnt.
	runner := &fakeUIDRunner{}
	if err := chownTreeViaRuntime(context.Background(), runner, "docker", "img:test", "/host/transient", 405); err != nil {
		t.Fatalf("chownTreeViaRuntime: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected exactly 1 runtime call, got %d: %v", len(runner.calls), runner.calls)
	}
	got := strings.Join(runner.calls[0], " ")
	for _, want := range []string{
		"docker run --rm",
		"--user 0",
		"--entrypoint chown",
		"--volume /host/transient:/mnt",
		"img:test",
		"-R 405 /mnt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("chown argv missing %q; got %q", want, got)
		}
	}
}

func TestChownTreeViaRuntimeRunnerErrorPropagates(t *testing.T) {
	runner := &fakeUIDRunner{
		outputs: []string{"chown: /mnt: Operation not permitted\n"},
		errs:    []error{errors.New("exit status 1")},
	}
	err := chownTreeViaRuntime(context.Background(), runner, "docker", "img:test", "/host/transient", 405)
	if err == nil {
		t.Fatal("expected error when the runtime chown fails")
	}
	if !strings.Contains(err.Error(), "chown") || !strings.Contains(err.Error(), "405") {
		t.Fatalf("error should name the chown target uid, got %v", err)
	}
}

func TestChownTreeViaRuntimeValidatesInputs(t *testing.T) {
	if err := chownTreeViaRuntime(context.Background(), nil, "docker", "img", "/d", 1); err == nil {
		t.Fatal("expected error for nil runner")
	}
	if err := chownTreeViaRuntime(context.Background(), &fakeUIDRunner{}, "", "img", "/d", 1); err == nil {
		t.Fatal("expected error for empty runtime")
	}
	if err := chownTreeViaRuntime(context.Background(), &fakeUIDRunner{}, "docker", "", "/d", 1); err == nil {
		t.Fatal("expected error for empty image")
	}
}

func TestResolveAgentUIDValidatesInputs(t *testing.T) {
	if _, err := resolveAgentUID(context.Background(), nil, "docker", "img"); err == nil {
		t.Fatal("expected error for nil runner")
	}
	if _, err := resolveAgentUID(context.Background(), &fakeUIDRunner{}, "", "img"); err == nil {
		t.Fatal("expected error for empty runtime")
	}
	if _, err := resolveAgentUID(context.Background(), &fakeUIDRunner{}, "docker", ""); err == nil {
		t.Fatal("expected error for empty image")
	}
}
