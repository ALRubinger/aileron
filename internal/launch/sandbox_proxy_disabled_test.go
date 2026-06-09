package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDaemonClient_RecordSandboxProxyDisabled_PostsJSONBody verifies
// the launcher emits a well-formed JSON body matching the OpenAPI
// SandboxProxyDisabledRequest schema and only treats 204 as success.
func TestDaemonClient_RecordSandboxProxyDisabled_PostsJSONBody(t *testing.T) {
	var captured struct {
		method string
		path   string
		auth   string
		ctype  string
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		captured.ctype = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&captured.body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL, "daemon-token")
	if err := c.RecordSandboxProxyDisabled(context.Background(), "sess-1", sandboxProxyReasonUserOptOut, "docker", "ghcr.io/acme/agent:latest"); err != nil {
		t.Fatalf("RecordSandboxProxyDisabled: %v", err)
	}

	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if captured.path != "/v1/sandbox-proxy/disabled" {
		t.Errorf("path = %q", captured.path)
	}
	if captured.auth != "Bearer daemon-token" {
		t.Errorf("auth = %q", captured.auth)
	}
	if captured.ctype != "application/json" {
		t.Errorf("content-type = %q", captured.ctype)
	}
	for key, want := range map[string]string{
		"session_id":    "sess-1",
		"reason":        sandboxProxyReasonUserOptOut,
		"sandbox_mode":  "docker",
		"sandbox_image": "ghcr.io/acme/agent:latest",
	} {
		if got, _ := captured.body[key].(string); got != want {
			t.Errorf("body[%s] = %v, want %q", key, captured.body[key], want)
		}
	}
}

// TestDaemonClient_RecordSandboxProxyDisabled_OmitsBlankOptional
// fields keeps the wire payload sparse — sandbox_mode/sandbox_image
// are dropped when the launcher doesn't have them (e.g. the
// pre-session-register refuse path).
func TestDaemonClient_RecordSandboxProxyDisabled_OmitsBlankOptionalFields(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newDaemonClient(srv.URL, "")
	if err := c.RecordSandboxProxyDisabled(context.Background(), "sess-1", sandboxProxyReasonUserOptOut, "", ""); err != nil {
		t.Fatalf("RecordSandboxProxyDisabled: %v", err)
	}
	if _, ok := body["sandbox_mode"]; ok {
		t.Errorf("sandbox_mode should be omitted when blank: %v", body)
	}
	if _, ok := body["sandbox_image"]; ok {
		t.Errorf("sandbox_image should be omitted when blank: %v", body)
	}
}

// TestDaemonClient_RecordSandboxProxyDisabled_NonSuccessIsError covers
// the failure path: anything but 204 surfaces as an error so callers
// can log it; the launcher treats this as best-effort and continues.
func TestDaemonClient_RecordSandboxProxyDisabled_NonSuccessIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{"error":{"code":"invalid_body","message":"oops"}}`))
	}))
	defer srv.Close()
	c := newDaemonClient(srv.URL, "")
	err := c.RecordSandboxProxyDisabled(context.Background(), "sess-1", sandboxProxyReasonUserOptOut, "", "")
	if err == nil {
		t.Fatal("expected error from 400 response")
	}
	if !strings.Contains(err.Error(), "invalid_body") {
		t.Errorf("error = %v", err)
	}
}

// TestReportSandboxProxyDisabled_LogsOnError uses the package-level
// stderr override to confirm the best-effort wrapper does not
// propagate daemon failures to the caller — the launcher path that
// uses it must keep running, with a warning sent to stderr.
func TestReportSandboxProxyDisabled_LogsOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = r.Body.Close()
	}))
	defer srv.Close()
	c := newDaemonClient(srv.URL, "")

	var buf bytes.Buffer
	prev := stderrForProxyDisabledReport
	stderrForProxyDisabledReport = &buf
	t.Cleanup(func() { stderrForProxyDisabledReport = prev })

	reportSandboxProxyDisabled(context.Background(), c, "sess-1", sandboxProxyReasonUserOptOut, "docker", "")
	if !strings.Contains(buf.String(), "record sandbox.proxy.disabled") {
		t.Errorf("expected warning on stderr; got %q", buf.String())
	}
}

// TestReportSandboxProxyDisabled_NoopOnEmptyReason guards against
// double-reporting when a successful preflight path resets the
// resolution but accidentally calls the reporter. Empty reason
// means "no disabled event"; the helper must not POST.
func TestReportSandboxProxyDisabled_NoopOnEmptyReason(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newDaemonClient(srv.URL, "")
	reportSandboxProxyDisabled(context.Background(), c, "sess-1", "", "docker", "")
	if called {
		t.Fatal("reporter posted with empty reason")
	}
}

// TestReportSandboxProxyDisabled_NoopOnNilClient guards the
// refuse-before-client-resolved case — the launcher may not yet have
// a daemon client when it has to fail preflight; the reporter must
// tolerate that.
func TestReportSandboxProxyDisabled_NoopOnNilClient(t *testing.T) {
	reportSandboxProxyDisabled(context.Background(), nil, "sess-1", sandboxProxyReasonUserOptOut, "docker", "")
}

// TestDaemonClient_RecordSandboxProxyDisabled_NilReceiver covers the
// guard at the top of the daemon-client method.
func TestDaemonClient_RecordSandboxProxyDisabled_NilReceiver(t *testing.T) {
	var c *daemonClient
	err := c.RecordSandboxProxyDisabled(context.Background(), "sess-1", sandboxProxyReasonUserOptOut, "", "")
	if err == nil {
		t.Fatal("expected error on nil receiver")
	}
}

// TestSandboxProxyRefuseError_NamesModeAndOptOutFlag verifies the
// actionable refuse error names both the sandbox mode the user gave
// and the opt-out flag. Surface text is the contract; CI runs it
// against the same regression check users see in their terminal.
func TestSandboxProxyRefuseError_NamesModeAndOptOutFlag(t *testing.T) {
	err := sandboxProxyRefuseError(sandboxProxyReasonUnsupportedSandboxMode, "off")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{
		"sandbox proxy bootstrap",
		"--sandbox=off",
		"--sandbox=docker",
		"--sandbox-proxy=off",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestSandboxProxyRefuseError_BlankModeSurfacesOffDefault makes sure
// the user-visible message reads sensibly even when --sandbox isn't
// passed (the resolver falls into the "" path).
func TestSandboxProxyRefuseError_BlankModeSurfacesOffDefault(t *testing.T) {
	err := sandboxProxyRefuseError(sandboxProxyReasonUnsupportedSandboxMode, "")
	if !strings.Contains(err.Error(), "--sandbox=off") {
		t.Errorf("error %q should default to --sandbox=off", err.Error())
	}
}

// TestSandboxProxyRefuseError_UnknownReason falls back to a generic
// message rather than asserting the unsupported-mode shape.
func TestSandboxProxyRefuseError_UnknownReason(t *testing.T) {
	err := sandboxProxyRefuseError("something_else", "docker")
	if err == nil || !strings.Contains(err.Error(), "something_else") {
		t.Errorf("error = %v", err)
	}
}

// TestSandboxProxyPreflightFailedError_NamesContractDocsAndImage
// covers the preflight-failure error: it must include the resolved
// image, the contract docs URL, the opt-out flag, and the wrapped
// cause so the user can see exactly which helper failed.
func TestSandboxProxyPreflightFailedError_NamesContractDocsAndImage(t *testing.T) {
	cause := errors.New("sandbox proxy bootstrap requires aileron-install-proxy-ca")
	err := sandboxProxyPreflightFailedError("ghcr.io/acme/agent:latest", cause)
	for _, want := range []string{
		"ghcr.io/acme/agent:latest",
		sandboxProxyContractDocsURL,
		"--sandbox-proxy=off",
		"aileron-install-proxy-ca",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestIsSandboxProxyContractFailure walks the contract-specific
// markers the runtime probe emits when proxy bootstrap helpers are
// missing or fail. Other failures must not be misclassified as
// contract failures.
func TestIsSandboxProxyContractFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "install_helper_missing", err: errors.New("sandbox proxy bootstrap requires aileron-install-proxy-ca"), want: true},
		{name: "run_helper_missing", err: errors.New("sandbox proxy bootstrap requires aileron-run-with-proxy-ca"), want: true},
		{name: "ca_check_fail_install", err: errors.New("preflight: aileron-install-proxy-ca --check failed"), want: true},
		{name: "wget_missing", err: errors.New("generated Aileron connector shims require wget support"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSandboxProxyContractFailure(tc.err); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
