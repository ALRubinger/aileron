package action

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/sandbox"
)

// fakeRuntime is a sandbox.Runtime that records what it was asked to
// compile/invoke and returns scripted results. Tests assert on the
// behavior of the executor (orchestration) without driving real WASM.
type fakeRuntime struct {
	connectors map[string]*fakeConnector // keyed by manifest FQN
}

type fakeConnector struct {
	manifest *cstore.Manifest
	binary   []byte
	calls    []sandbox.Call
	resp     map[string]any
	err      error
	closed   bool
}

func (r *fakeRuntime) Compile(_ context.Context, m *cstore.Manifest, b []byte) (sandbox.Connector, error) {
	if r.connectors == nil {
		r.connectors = map[string]*fakeConnector{}
	}
	c, ok := r.connectors[m.Connector.Name]
	if !ok {
		c = &fakeConnector{}
		r.connectors[m.Connector.Name] = c
	}
	c.manifest = m
	c.binary = b
	return c, nil
}

func (r *fakeRuntime) Close(_ context.Context) error { return nil }

func (c *fakeConnector) Invoke(_ context.Context, call sandbox.Call) (sandbox.Result, error) {
	c.calls = append(c.calls, call)
	if c.err != nil {
		return sandbox.Result{}, c.err
	}
	return sandbox.Result{Output: c.resp}, nil
}

func (c *fakeConnector) Close(_ context.Context) error {
	c.closed = true
	return nil
}

// installFakeConnector populates a cstore.Store with a fake connector
// entry whose manifest declares the given FQN+version. The hash
// returned matches the entry's on-disk location so the executor's
// connector lookup finds it.
func installFakeConnector(t *testing.T, store *cstore.Store, fqn, version string) string {
	t.Helper()
	manifestTOML := `[connector]
name = "` + fqn + `"
version = "` + version + `"
publisher = "test"
`
	tb := &cstore.Tarball{
		BinaryName: "connector.wasm",
		Binary:     []byte("FAKE-BINARY"),
		Manifest:   []byte(manifestTOML),
		Signature:  []byte("FAKE-SIG"),
	}
	hashHex := tb.CanonicalHashHex()
	dir := filepath.Join(store.Root(), "connectors", "sha256", hashHex)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "connector.wasm"), tb.Binary, 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), tb.Manifest, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "signature.sig"), tb.Signature, 0o644); err != nil {
		t.Fatalf("write signature: %v", err)
	}
	return "sha256:" + hashHex
}

// installFakeAction writes an action manifest that references the
// supplied connector FQN+version+hash with the given declared
// capabilities. Returns the action store rooted at the temp dir.
func installFakeAction(t *testing.T, fqn, version, hash string, capabilities []string, executes []string) *Store {
	t.Helper()
	dir := t.TempDir()
	caps := `["` + strings.Join(capabilities, `", "`) + `"]`
	executeBlocks := ""
	for _, op := range executes {
		executeBlocks += `

[[execute]]
id = "` + op + `"
connector = "` + fqn + `"
op = "` + op + `"
`
	}
	manifest := `+++
name = "test-action"
version = "1.0.0"
source = "hub://test/test-action@1.0.0"

[[requires.connectors]]
name = "` + fqn + `"
version = "` + version + `"
hash = "` + hash + `"
capabilities = ` + caps + `

[match]
intent = "test"` + executeBlocks + `+++

# Test Action
`
	if err := os.WriteFile(filepath.Join(dir, "test-action.md"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write action: %v", err)
	}
	store := NewStore(dir)
	if _, err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return store
}

func TestSandboxExecutor_HappyPath_RunsStepsInOrder(t *testing.T) {
	// ADR-0003 §"Multi-step actions": linear chains. The executor
	// invokes each step in declared order and returns the last
	// step's output as the action result.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash, []string{"echo"}, []string{"echo"})

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/echo": {resp: map[string]any{"value": "hi"}}}

	exec := NewSandboxExecutor(actions, store, rt)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	res, err := exec.Execute(context.Background(), "test-action", map[string]any{"value": "hi"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Failure != nil {
		t.Errorf("expected non-error result; got %+v", res)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Content), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["action"] != "test-action" {
		t.Errorf("body.action = %v", body["action"])
	}
	if c := rt.connectors["github://test/echo"]; len(c.calls) != 1 {
		t.Errorf("connector called %d times, want 1", len(c.calls))
	}
}

func TestSandboxExecutor_ActionBoundary_DeniesUndeclaredOp(t *testing.T) {
	// ADR-0003 §"defense in depth" / issue #359 acceptance #7:
	// "the action's declared capability subset is enforced *in
	// addition to* the connector's manifest grant". The executor
	// calls EnforceCapability with the step's op name; an op not in
	// the action's capabilities list returns IsError=true with the
	// capability_denied envelope shape.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	// Action's [[execute]] uses op "secret_op" but capabilities only
	// declares "echo" → must be denied at the action boundary.
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash, []string{"echo"}, []string{"secret_op"})

	rt := &fakeRuntime{}
	exec := NewSandboxExecutor(actions, store, rt)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	res, err := exec.Execute(context.Background(), "test-action", nil)
	if err != nil {
		t.Fatalf("Execute returned a Go error; want failure result: %v", err)
	}
	if res.Failure == nil {
		t.Fatal("expected Result.Failure on undeclared op")
	}
	if res.Failure.Class() != failure.CapabilityDenied {
		t.Errorf("class = %q, want capability_denied", res.Failure.Class())
	}
	if res.Failure.Boundary() != failure.Action {
		t.Errorf("boundary = %q, want action", res.Failure.Boundary())
	}
	// The runtime must NOT have been invoked when the action-boundary
	// check denies — defense in depth means the inner check is unreachable.
	if c, ok := rt.connectors["github://test/echo"]; ok && len(c.calls) > 0 {
		t.Errorf("connector was invoked despite action-boundary denial")
	}
}

func TestSandboxExecutor_FirstFailureTerminates_NoLaterStepsRun(t *testing.T) {
	// ADR-0010 §"Multi-step actions: atomic failure, no
	// auto-compensation". When step 1 fails, step 2 must not run;
	// no rollback of prior steps happens either.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash,
		[]string{"step1", "step2"}, []string{"step1", "step2"})

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/echo": {
		err: &sandbox.Error{Class: sandbox.ClassCapabilityDenied, Boundary: sandbox.BoundarySandbox, Message: "boom"},
	}}

	exec := NewSandboxExecutor(actions, store, rt)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	res, err := exec.Execute(context.Background(), "test-action", nil)
	if err != nil {
		t.Fatalf("Execute Go error: %v", err)
	}
	if res.Failure == nil {
		t.Fatal("expected IsError=true on first-step failure")
	}
	// step2 must not have been invoked.
	c := rt.connectors["github://test/echo"]
	if len(c.calls) != 1 {
		t.Errorf("connector calls = %d; want exactly 1 (step1 failed → step2 skipped)", len(c.calls))
	}
	if c.calls[0].Op != "step1" {
		t.Errorf("first call was %q, want step1", c.calls[0].Op)
	}
}

func TestSandboxExecutor_ActionNotFoundReturnsGoError(t *testing.T) {
	// ADR-0010: a returned Go error is reserved for gateway-fatal
	// conditions like missing actions; tool-result-shaped failures
	// (action ran but failed) come back as IsError Results.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	actions := NewStore(t.TempDir())
	if _, err := actions.Load(); err != nil {
		t.Fatalf("load empty: %v", err)
	}
	exec := NewSandboxExecutor(actions, store, &fakeRuntime{})
	_, err := exec.Execute(context.Background(), "missing", nil)
	if err == nil {
		t.Fatal("expected Go error for missing action")
	}
}

func TestSandboxExecutor_RejectsNilDependencies(t *testing.T) {
	exec := &SandboxExecutor{}
	if _, err := exec.Execute(context.Background(), "x", nil); err == nil {
		t.Fatal("expected error when Actions/Store/Runtime are nil")
	}
}

func TestSandboxExecutor_CompiledConnectorIsCached(t *testing.T) {
	// Wazero compilation is meaningful work; the executor caches the
	// compiled handle by content hash so repeated invocations of the
	// same connector don't recompile. Two Execute calls referencing
	// the same connector hash must produce only one Compile call on
	// the runtime.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash, []string{"echo"}, []string{"echo"})

	rt := &countingRuntime{inner: &fakeRuntime{
		connectors: map[string]*fakeConnector{"github://test/echo": {resp: map[string]any{"v": 1}}},
	}}
	exec := NewSandboxExecutor(actions, store, rt)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	for i := 0; i < 3; i++ {
		if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
			t.Fatalf("Execute #%d: %v", i, err)
		}
	}
	if rt.compiles != 1 {
		t.Errorf("Compile called %d times; want 1 (cache must dedupe by hash)", rt.compiles)
	}
}

// countingRuntime counts Compile invocations, delegating execution to
// the inner runtime.
type countingRuntime struct {
	inner    sandbox.Runtime
	compiles int
}

func (r *countingRuntime) Compile(ctx context.Context, m *cstore.Manifest, b []byte) (sandbox.Connector, error) {
	r.compiles++
	return r.inner.Compile(ctx, m, b)
}

func (r *countingRuntime) Close(ctx context.Context) error { return r.inner.Close(ctx) }

func TestSandboxExecutor_ConnectorBinaryMissingFromStoreSurfacesError(t *testing.T) {
	// The action declares hash X, but the cstore has no entry for X
	// (e.g., the user copied actions over but hasn't run sync). The
	// executor surfaces this as an IsError result rather than
	// silently skipping or stubbing.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	// NOTE: not installing the connector — store is empty.
	actions := installFakeAction(t, "github://test/echo", "1.0.0", "sha256:deadbeef", []string{"echo"}, []string{"echo"})
	exec := NewSandboxExecutor(actions, store, &fakeRuntime{})
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	res, err := exec.Execute(context.Background(), "test-action", nil)
	if err != nil {
		t.Fatalf("Execute Go err: %v", err)
	}
	if res.Failure == nil {
		t.Errorf("expected IsError=true when connector binary missing")
	}
}

func TestSandboxExecutor_Close_ClosesCachedConnectors(t *testing.T) {
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash, []string{"echo"}, []string{"echo"})

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/echo": {resp: map[string]any{}}}
	exec := NewSandboxExecutor(actions, store, rt)
	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := exec.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !rt.connectors["github://test/echo"].closed {
		t.Errorf("Close did not close the cached connector")
	}
}

func TestFailureFromError_PreservesStructuredErrors(t *testing.T) {
	// Recognized error types map to failure.Failure preserving the
	// boundary, class, and details so the LLM and audit log see the
	// canonical ADR-0010 envelope.
	cases := []struct {
		name         string
		err          error
		wantClass    failure.FailureClass
		wantBoundary failure.Boundary
		wantMessage  string
	}{
		{
			"action capability_denied",
			&Error{Class: ClassCapabilityDenied, Boundary: BoundaryAction, Message: "no", Details: map[string]any{"x": 1}},
			failure.CapabilityDenied, failure.Action, "no",
		},
		{
			"sandbox resource_limit_exceeded",
			&sandbox.Error{Class: sandbox.ClassResourceLimitExceeded, Boundary: sandbox.BoundarySandbox, Message: "boom"},
			failure.ResourceLimitExceeded, failure.Sandbox, "boom",
		},
		{
			"cstore hash_mismatch",
			&cstore.Error{Class: cstore.ClassHashMismatch, Boundary: cstore.BoundaryRuntime, Message: "diff"},
			failure.HashMismatch, failure.Runtime, "diff",
		},
		{
			"plain error → connector_runtime_error",
			errors.New("ouch"),
			failure.ConnectorRuntimeError, failure.Runtime, "ouch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := failureFromError(tc.err)
			if got == nil {
				t.Fatalf("failureFromError(%v) = nil", tc.err)
			}
			if got.Class() != tc.wantClass {
				t.Errorf("class = %q, want %q", got.Class(), tc.wantClass)
			}
			if got.Boundary() != tc.wantBoundary {
				t.Errorf("boundary = %q, want %q", got.Boundary(), tc.wantBoundary)
			}
			if got.Message() != tc.wantMessage {
				t.Errorf("message = %q, want %q", got.Message(), tc.wantMessage)
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("errors.Is(failure, original) = false; cause not wrapped")
			}
		})
	}
}

func TestFailureFromError_PreservesActionDetails(t *testing.T) {
	src := &Error{
		Class:    ClassCapabilityDenied,
		Boundary: BoundaryAction,
		Message:  "no",
		Details:  map[string]any{"x": 1},
	}
	got := failureFromError(src)
	if got.Details()["x"] != 1 {
		t.Errorf("details.x = %v, want 1", got.Details()["x"])
	}
}

func TestFailureFromError_NilErrorReturnsNil(t *testing.T) {
	if got := failureFromError(nil); got != nil {
		t.Errorf("failureFromError(nil) = %v, want nil", got)
	}
}
