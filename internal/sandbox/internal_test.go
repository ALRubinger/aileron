package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSandboxExecutor_ActionBoundary_DeniesURLOutsideSubset(t *testing.T) {
	t.Parallel()
	// Issue #359 acceptance #7: the action's declared capability
	// subset is enforced *in addition to* the connector manifest's
	// grant. Even when the connector manifest grants two hosts, an
	// action that declares only one still cannot dial the other.
	//
	// This test drives the runtime directly with a non-empty
	// AllowedAuthority so the host-side check is exercised in
	// isolation; the SandboxExecutor in internal/action drives the
	// same path via action manifest input.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	allowedHost := httpHostFromURL(srv.URL)

	rt := newTestRuntime(t, srv.Client())
	conn, err := rt.Compile(context.Background(),
		echoManifest(allowedHost, "secondary.example.com:443"), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	// Action allows only the secondary host; the connector grant has both.
	_, err = conn.Invoke(context.Background(), Call{
		Op:               "http",
		Args:             map[string]any{"url": srv.URL + "/"},
		AllowedAuthority: []string{"secondary.example.com:443"},
	})
	if err == nil {
		t.Fatal("expected denial; got nil error")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Class != ClassCapabilityDenied {
		t.Fatalf("err class = %v, want capability_denied", err)
	}
	// Boundary detail names "action" so callers can disambiguate which
	// of the two checks denied (per ADR-0010 boundary semantics).
	if got, _ := serr.Details["boundary_detail"].(string); got != "action" {
		t.Errorf("details.boundary_detail = %q, want action", got)
	}
}

func TestSandboxExecutor_ActionBoundary_AllowsURLInSubset(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	allowedHost := httpHostFromURL(srv.URL)

	rt := newTestRuntime(t, srv.Client())
	conn, err := rt.Compile(context.Background(), echoManifest(allowedHost), echoWASM)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	_, err = conn.Invoke(context.Background(), Call{
		Op:               "http",
		Args:             map[string]any{"url": srv.URL + "/"},
		AllowedAuthority: []string{allowedHost}, // explicit subset matching connector grant
	})
	if err != nil {
		t.Errorf("expected success when URL is in the action subset; got %v", err)
	}
}

func TestWazeroRuntime_Close_IsIdempotent(t *testing.T) {
	rt, err := NewWazeroRuntime(context.Background())
	if err != nil {
		t.Fatalf("NewWazeroRuntime: %v", err)
	}
	if err := rt.Close(context.Background()); err != nil {
		t.Errorf("first Close: %v", err)
	}
	// Closing again is safe — runtime swallows the second call.
	_ = rt.Close(context.Background())
}

func TestWazeroRuntime_NilCloseOnNilReceiver(t *testing.T) {
	var rt *WazeroRuntime
	if err := rt.Close(context.Background()); err != nil {
		t.Errorf("nil receiver Close = %v, want nil", err)
	}
}

func TestWazeroConnector_NilCloseOnNilReceiver(t *testing.T) {
	var c *wazeroConnector
	if err := c.Close(context.Background()); err != nil {
		t.Errorf("nil receiver Close = %v, want nil", err)
	}
}

func TestWithLogger_OptionSetsField(t *testing.T) {
	custom := slog.New(slog.NewTextHandler(nil, nil))
	rt := &WazeroRuntime{}
	WithLogger(custom)(rt)
	if rt.log != custom {
		t.Errorf("WithLogger did not set the logger field")
	}
}

func TestEncodeInvocationInput_NilArgsBecomesEmptyObject(t *testing.T) {
	// The connector reads `{"op": "...", "args": {...}}` from stdin.
	// When the call has no args, the runtime emits an empty object
	// rather than `null` so the connector's JSON parser doesn't have
	// to handle the optional case.
	got, err := encodeInvocationInput(Call{Op: "x"})
	if err != nil {
		t.Fatalf("encodeInvocationInput: %v", err)
	}
	want := `{"args":{},"op":"x"}`
	if string(got) != want {
		t.Errorf("encodeInvocationInput = %q, want %q", string(got), want)
	}
}

func TestToSet_EmptyAndPopulated(t *testing.T) {
	if got := toSet(nil); got != nil {
		t.Errorf("toSet(nil) = %v, want nil", got)
	}
	if got := toSet([]string{}); got != nil {
		t.Errorf("toSet([]) = %v, want nil", got)
	}
	got := toSet([]string{"a", "b"})
	if len(got) != 2 || got["a"] != struct{}{} || got["b"] != struct{}{} {
		t.Errorf("toSet([a,b]) = %v", got)
	}
}

func TestTail_TruncatesFromTheFront(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 4, ""},
		{"abc", 4, "abc"},
		{"abcd", 4, "abcd"},
		{"abcde", 4, "...bcde"},
	}
	for _, tc := range cases {
		if got := tail(tc.in, tc.n); got != tc.want {
			t.Errorf("tail(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestDecodeResult_EmptyStdoutIsAllowed(t *testing.T) {
	// Connectors with no result write nothing to stdout; the
	// runtime returns a Result with nil Output rather than an
	// error.
	c := &wazeroConnector{}
	res, err := c.decodeResult(&hostState{}, nil)
	if err != nil {
		t.Errorf("decodeResult on empty stdout err = %v", err)
	}
	if res.Output != nil {
		t.Errorf("Output = %v, want nil for empty stdout", res.Output)
	}
}

func TestDecodeResult_MalformedJSONSurfacesAsRuntimeError(t *testing.T) {
	c := &wazeroConnector{}
	_, err := c.decodeResult(&hostState{}, []byte("not json"))
	if err == nil {
		t.Fatal("expected runtime error for malformed stdout")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Class != ClassConnectorRuntimeError {
		t.Errorf("err class = %v, want connector_runtime_error", err)
	}
}

func TestDecodeResult_ErrorEnvelopeIsPropagated(t *testing.T) {
	// A connector signals failure via {"error":{"class":"...","message":"..."}}.
	// The runtime turns that into a structured *sandbox.Error so
	// the executor surfaces it to the LLM with the same envelope shape.
	c := &wazeroConnector{}
	_, err := c.decodeResult(&hostState{}, []byte(`{"error":{"class":"http_unauthorized","message":"401"}}`))
	if err == nil {
		t.Fatal("expected error for non-empty error envelope")
	}
	var serr *Error
	if !errors.As(err, &serr) {
		t.Fatalf("err type = %T", err)
	}
	if serr.Class != "http_unauthorized" {
		t.Errorf("class = %s, want http_unauthorized", serr.Class)
	}
	if serr.Boundary != BoundarySandbox {
		t.Errorf("boundary = %s, want sandbox", serr.Boundary)
	}
}

func TestHostFromURL_DefaultPortsByScheme(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://slack.com/x", "slack.com:443"},
		{"http://slack.com/x", "slack.com:80"},
		{"https://slack.com:8443/x", "slack.com:8443"},
		{"not a url", "not a url"},
	}
	for _, tc := range cases {
		if got := hostFromURL(tc.in); got != tc.want {
			t.Errorf("hostFromURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeHostPort_LeavesUnparseableAlone(t *testing.T) {
	// Inputs without a port (e.g., a bare host) flow through
	// untouched so they still fail to match — the policy is a closed
	// set and unparseable entries simply never match a request.
	if got := normalizeHostPort("notahostport"); got != "notahostport" {
		t.Errorf("normalizeHostPort = %q, want unchanged", got)
	}
}

func TestNewHostPolicy_NilManifestProducesEmptyGrant(t *testing.T) {
	p := NewHostPolicy(nil)
	if len(p.AllowedHosts()) != 0 {
		t.Errorf("nil manifest produced %d allowed hosts; want 0", len(p.AllowedHosts()))
	}
	if err := p.CheckURL("https://anything.example/"); err == nil {
		t.Errorf("nil-manifest policy should deny everything")
	}
}

func TestErrorString_FormatsClassMessageBoundary(t *testing.T) {
	e := newCapabilityDenied("nope", map[string]any{"requested": "x"})
	got := e.Error()
	if got != "capability_denied: nope (boundary=sandbox)" {
		t.Errorf("Error() = %q", got)
	}
}

func TestErrorString_NilReceiverIsEmpty(t *testing.T) {
	var e *Error
	if e.Error() != "" {
		t.Errorf("nil Error() = %q, want empty", e.Error())
	}
}
