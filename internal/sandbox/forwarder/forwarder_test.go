package forwarder_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/sandbox/forwarder"
	"github.com/ALRubinger/aileron/internal/sandbox/sandboxtest"
)

// gitcrawlManifest is a representative forwarder-shaped manifest: a
// connector that wraps the git CLI via the spawn primitive and opts
// into the daemon-embedded forwarder. Used as the canonical fixture
// across these end-to-end tests.
func gitcrawlManifest() *cstore.Manifest {
	return &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:      "github://aileron-test/gitcrawl",
			Version:   "0.0.1",
			Forwarder: cstore.BuiltinForwarderSpawn,
		},
		Capabilities: cstore.ManifestCapabilities{
			Spawn: &cstore.ManifestSpawn{
				Programs: []cstore.ManifestSpawnProgram{
					{Path: "/usr/bin/git"},
				},
				Operations: map[string]cstore.ManifestSpawnOperation{
					"log":    {Argv: "git log --since={since}"},
					"status": {Argv: "git status"},
				},
				EnvPassthrough: []string{"GIT_AUTHOR_NAME"},
				FSRead:         []string{"~/code/"},
			},
		},
	}
}

func TestWASM_Embedded(t *testing.T) {
	if len(forwarder.WASM) == 0 {
		t.Fatal("forwarder.WASM is empty; rerun task build:forwarder")
	}
	// WASM modules start with the four-byte magic 0x00 0x61 0x73 0x6d.
	if len(forwarder.WASM) < 4 {
		t.Fatalf("forwarder.WASM too small: %d bytes", len(forwarder.WASM))
	}
	if !(forwarder.WASM[0] == 0x00 && forwarder.WASM[1] == 0x61 &&
		forwarder.WASM[2] == 0x73 && forwarder.WASM[3] == 0x6d) {
		t.Errorf("forwarder.WASM does not begin with the WASM magic; first bytes: %x", forwarder.WASM[:4])
	}
}

func TestForwarder_DispatchesToSpawnOp(t *testing.T) {
	t.Parallel()
	// End-to-end: compile the embedded forwarder, invoke it with a
	// {op, args} envelope on stdin, verify the runtime's spawn host
	// function received the manifest-derived spawn envelope.
	exec := &sandboxtest.RecordingExecutor{
		Result: sandbox.SpawnResult{ExitCode: 0, Stdout: []byte("commit-list"), Stderr: nil},
	}
	rt, err := sandbox.NewWazeroRuntime(context.Background(), sandbox.WithSpawnExecutor(exec))
	if err != nil {
		t.Fatalf("NewWazeroRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	conn, err := rt.Compile(context.Background(), gitcrawlManifest(), forwarder.WASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	res, err := conn.Invoke(context.Background(), sandbox.Call{
		Op:   "log",
		Args: map[string]any{"since": "2026-01-01"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got, _ := res.Output["exit"].(float64); got != 0 {
		t.Errorf("output.exit = %v, want 0", res.Output["exit"])
	}
	if got, _ := res.Output["stdout"].(string); got != "commit-list" {
		t.Errorf("output.stdout = %q", got)
	}

	calls := exec.Calls()
	if len(calls) != 1 {
		t.Fatalf("executor saw %d calls, want 1", len(calls))
	}
	got := calls[0]
	if got.Program != "/usr/bin/git" {
		t.Errorf("Program = %q", got.Program)
	}
	wantArgv := []string{"git", "log", "--since=2026-01-01"}
	if !sliceEq(got.Argv, wantArgv) {
		t.Errorf("Argv = %v, want %v", got.Argv, wantArgv)
	}
}

func TestForwarder_UndeclaredOpDenied(t *testing.T) {
	t.Parallel()
	exec := &sandboxtest.RecordingExecutor{}
	rt, err := sandbox.NewWazeroRuntime(context.Background(), sandbox.WithSpawnExecutor(exec))
	if err != nil {
		t.Fatalf("NewWazeroRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	conn, err := rt.Compile(context.Background(), gitcrawlManifest(), forwarder.WASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	// "push" is not in the manifest's operations. The runtime should
	// deny before the executor sees anything.
	res, err := conn.Invoke(context.Background(), sandbox.Call{
		Op:   "push",
		Args: map[string]any{},
	})
	// The forwarder catches the spawn_op rc != 0 and emits its own
	// error envelope. The runtime surfaces it as a sandbox error.
	if err == nil {
		t.Logf("res: %+v", res)
	}
	if exec.CallCount() != 0 {
		t.Errorf("executor should not run for an undeclared op; got %d calls", exec.CallCount())
	}
}

func TestForwarder_MissingPlaceholderDenied(t *testing.T) {
	t.Parallel()
	exec := &sandboxtest.RecordingExecutor{}
	rt, _ := sandbox.NewWazeroRuntime(context.Background(), sandbox.WithSpawnExecutor(exec))
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	conn, _ := rt.Compile(context.Background(), gitcrawlManifest(), forwarder.WASM)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	// `log` requires {since}; pass empty args.
	res, _ := conn.Invoke(context.Background(), sandbox.Call{
		Op:   "log",
		Args: map[string]any{},
	})
	// Forwarder catches the denial and emits an error envelope.
	if exec.CallCount() != 0 {
		t.Errorf("executor should not run when a placeholder is missing; got %d calls", exec.CallCount())
	}
	body, _ := json.Marshal(res.Output)
	if strings.Contains(string(body), "commit-list") {
		t.Errorf("output should not contain executor results: %s", body)
	}
}

func TestForwarder_StableOperationsAcrossInvocations(t *testing.T) {
	t.Parallel()
	// One forwarder instance, two invocations of different operations.
	// Each invocation must independently lookup its op against the
	// manifest; state from the first call must not leak into the
	// second.
	hook := func(env sandbox.SpawnEnvelope) (sandbox.SpawnResult, error) {
		return sandbox.SpawnResult{
			ExitCode: 0,
			Stdout:   []byte("op:" + strings.Join(env.Argv, " ")),
		}, nil
	}
	exec := &sandboxtest.RecordingExecutor{Hook: hook}
	rt, _ := sandbox.NewWazeroRuntime(context.Background(), sandbox.WithSpawnExecutor(exec))
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	conn, _ := rt.Compile(context.Background(), gitcrawlManifest(), forwarder.WASM)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	res1, err := conn.Invoke(context.Background(), sandbox.Call{
		Op:   "status",
		Args: map[string]any{},
	})
	if err != nil {
		t.Fatalf("first invoke: %v", err)
	}
	res2, err := conn.Invoke(context.Background(), sandbox.Call{
		Op:   "log",
		Args: map[string]any{"since": "2026-01-01"},
	})
	if err != nil {
		t.Fatalf("second invoke: %v", err)
	}
	stdout1, _ := res1.Output["stdout"].(string)
	stdout2, _ := res2.Output["stdout"].(string)
	if !strings.Contains(stdout1, "status") {
		t.Errorf("res1.stdout = %q, want to contain 'status'", stdout1)
	}
	if !strings.Contains(stdout2, "log") || !strings.Contains(stdout2, "2026-01-01") {
		t.Errorf("res2.stdout = %q, want log + date", stdout2)
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
