package sandbox

import (
	_ "embed"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// echoWASM is the precompiled test fixture from internal/sandbox/testdata/echo/.
// Regenerate by running:
//
//	cd internal/sandbox/testdata/echo && GOOS=wasip1 GOARCH=wasm go build -o ../echo.wasm .
//
//go:embed testdata/echo.wasm
var echoWASM []byte

// echoManifest builds a connector manifest with the supplied network
// hosts so each test can scope which URLs the fixture's "http" op may
// dial.
func echoManifest(hosts ...string) *cstore.Manifest {
	m := &cstore.Manifest{
		Connector: cstore.ManifestConnector{
			Name:    "github://aileron-test/echo",
			Version: "1.0.0",
		},
	}
	if len(hosts) > 0 {
		m.Capabilities.Network = &cstore.ManifestNetwork{Hosts: hosts}
	}
	return m
}

// newTestRuntime builds a WazeroRuntime with an injected fake HTTP
// doer so tests can verify the network-gating logic without real I/O.
func newTestRuntime(t *testing.T, doer HTTPDoer) Runtime {
	t.Helper()
	rt, err := NewWazeroRuntime(context.Background(), WithHTTPDoer(doer))
	if err != nil {
		t.Fatalf("NewWazeroRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	return rt
}

func TestWazeroRuntime_PerCallInstantiation_HappyPathRoundTrip(t *testing.T) {
	// ADR-0005 acceptance #1: per-call connector instantiation works.
	// AC #8: connector instances are scoped to a single invocation
	// and torn down on completion. Two invocations produce independent
	// state — neither sees the other's writes.
	rt := newTestRuntime(t, http.DefaultClient)
	conn, err := rt.Compile(context.Background(), echoManifest(), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	res, err := conn.Invoke(context.Background(), Call{
		Op:   "echo",
		Args: map[string]any{"value": "hello"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := res.Output["echo"]; got != "hello" {
		t.Errorf("Output.echo = %v, want hello", got)
	}

	// Second invocation succeeds independently with new args.
	res2, err := conn.Invoke(context.Background(), Call{
		Op:   "echo",
		Args: map[string]any{"value": "world"},
	})
	if err != nil {
		t.Fatalf("second Invoke: %v", err)
	}
	if got := res2.Output["echo"]; got != "world" {
		t.Errorf("second Output.echo = %v, want world", got)
	}
}

func TestWazeroRuntime_AileronLog_CapturedAsLogLines(t *testing.T) {
	// The connector calls aileron_host.log to emit a log line; the
	// runtime captures those into Result.Logs. This is the seam
	// audit emission will use post-MVP (#365).
	rt := newTestRuntime(t, http.DefaultClient)
	conn, err := rt.Compile(context.Background(), echoManifest(), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	res, err := conn.Invoke(context.Background(), Call{Op: "log"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(res.Logs) != 1 || res.Logs[0].Message != "hello from connector" {
		t.Errorf("Logs = %+v, want one entry from connector", res.Logs)
	}
}

func TestWazeroRuntime_HTTPRequestAllowedHostSucceeds(t *testing.T) {
	// ADR-0005 acceptance #3: network access is gated by
	// [capabilities.network]. A URL whose host:port is in the grant
	// reaches the host's HTTP doer and the response flows back to
	// the connector via the http_response_* host functions.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hi"))
	}))
	t.Cleanup(srv.Close)

	rt := newTestRuntime(t, srv.Client())
	conn, err := rt.Compile(context.Background(), echoManifest(httpHostFromURL(srv.URL)), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	res, err := conn.Invoke(context.Background(), Call{
		Op:   "http",
		Args: map[string]any{"url": srv.URL + "/path"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := res.Output["status"]; got != float64(200) {
		t.Errorf("Output.status = %v, want 200", got)
	}
	if got := res.Output["body"]; got != "hi" {
		t.Errorf("Output.body = %v, want hi", got)
	}
}

func TestWazeroRuntime_HTTPRequestDeniedHostReturnsCapabilityDenied(t *testing.T) {
	// ADR-0005 example "Capability denial at the WASM sandbox boundary":
	// the runtime refuses outbound calls to hosts not in the grant.
	// The error class is capability_denied with boundary=sandbox; the
	// `requested` and `granted` details name the offending and
	// permitted hosts respectively.
	rt := newTestRuntime(t, http.DefaultClient)
	conn, err := rt.Compile(context.Background(), echoManifest("slack.com:443"), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	_, err = conn.Invoke(context.Background(), Call{
		Op:   "http",
		Args: map[string]any{"url": "https://evil.example.com/"},
	})
	if err == nil {
		t.Fatal("expected denial; got nil error")
	}
	var serr *Error
	if !errors.As(err, &serr) {
		t.Fatalf("err type = %T; want *sandbox.Error", err)
	}
	if serr.Class != ClassCapabilityDenied {
		t.Errorf("class = %s, want capability_denied", serr.Class)
	}
	if serr.Boundary != BoundarySandbox {
		t.Errorf("boundary = %s, want sandbox", serr.Boundary)
	}
	if got, _ := serr.Details["requested"].(string); got != "network:evil.example.com:443" {
		t.Errorf("details.requested = %q", got)
	}
}

func TestWazeroRuntime_WallTimeLimitTerminatesAndNamesLimit(t *testing.T) {
	// ADR-0005 §"Resource limits": hitting a limit terminates the
	// instance and surfaces a structured error to the calling action.
	// AC #6: terminations produce class=resource_limit_exceeded with
	// the specific limit named in details.
	if testing.Short() {
		t.Skip("infinite-loop test takes ~2s; skipped under -short")
	}
	rt := newTestRuntime(t, http.DefaultClient)
	conn, err := rt.Compile(context.Background(), echoManifest(), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	start := time.Now()
	_, err = conn.Invoke(context.Background(), Call{
		Op:     "loop",
		Limits: Limits{WallTime: 500 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("expected resource_limit_exceeded; got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("invocation ran for %v; should have terminated near 500ms", elapsed)
	}
	var serr *Error
	if !errors.As(err, &serr) {
		t.Fatalf("err type = %T; want *sandbox.Error", err)
	}
	if serr.Class != ClassResourceLimitExceeded {
		t.Errorf("class = %s, want resource_limit_exceeded", serr.Class)
	}
	if got, _ := serr.Details["limit"].(string); got != "wall_time" {
		t.Errorf("details.limit = %q, want wall_time (the limit must be named)", got)
	}
}

func TestWazeroRuntime_ConnectorPanicSurfacesAsRuntimeError(t *testing.T) {
	// A connector that exits non-zero produces a connector_runtime_error.
	// The class lets the LLM (and audit log) reason about the failure
	// without parsing prose; the message is bounded so an enormous
	// stderr blob doesn't leak unbounded text.
	rt := newTestRuntime(t, http.DefaultClient)
	conn, err := rt.Compile(context.Background(), echoManifest(), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	_, err = conn.Invoke(context.Background(), Call{Op: "panic"})
	if err == nil {
		t.Fatal("expected connector_runtime_error; got nil")
	}
	var serr *Error
	if !errors.As(err, &serr) {
		t.Fatalf("err type = %T; want *sandbox.Error", err)
	}
	if serr.Class != ClassConnectorRuntimeError {
		t.Errorf("class = %s, want connector_runtime_error", serr.Class)
	}
}

func TestWazeroRuntime_MalformedBinaryFailsCompile(t *testing.T) {
	// ADR-0005 acceptance #2: out-of-grant imports refuse at
	// instantiation. Malformed binaries fail at compile, which is
	// the earlier failure mode. Both produce connector_load_failed.
	rt := newTestRuntime(t, http.DefaultClient)
	_, err := rt.Compile(context.Background(), echoManifest(), []byte("not a wasm module"))
	if err == nil {
		t.Fatal("expected compile failure on garbage input")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Class != ClassConnectorLoadFailed {
		t.Errorf("err class = %v, want connector_load_failed", err)
	}
}

func TestWazeroRuntime_NilManifestRejected(t *testing.T) {
	rt := newTestRuntime(t, http.DefaultClient)
	if _, err := rt.Compile(context.Background(), nil, echoWASM); err == nil {
		t.Fatal("expected error for nil manifest")
	}
}

// httpHostFromURL strips the scheme and trailing path from an httptest
// server URL to produce the `host:port` form the manifest grants.
func httpHostFromURL(rawURL string) string {
	const scheme = "http://"
	if len(rawURL) > len(scheme) && rawURL[:len(scheme)] == scheme {
		return rawURL[len(scheme):]
	}
	return rawURL
}
