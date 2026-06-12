package launch

import (
	"context"
	"errors"
	"io"
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
// stdout per invocation, in order. It substitutes for Docker/Podman so
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
	uid, err := resolveAgentUID(context.Background(), runner, "podman", "img:test")
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
