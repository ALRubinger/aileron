package failure

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Closed taxonomy:
//   - AllClasses() / AllBoundaries() enumerate every valid value.
//   - Each value is unique, non-empty, and lowercase-snake-case.
//   - Constructors set the canonical class and boundary; callers
//     can override boundary only via WithBoundary.

func TestAllClasses_Enumerates9(t *testing.T) {
	got := AllClasses()
	if len(got) != 9 {
		t.Fatalf("AllClasses count = %d, want 9", len(got))
	}
	seen := map[FailureClass]bool{}
	for _, c := range got {
		if c == "" {
			t.Errorf("empty class in AllClasses")
		}
		if seen[c] {
			t.Errorf("duplicate class %q", c)
		}
		seen[c] = true
	}
}

func TestAllBoundaries_Enumerates5(t *testing.T) {
	got := AllBoundaries()
	if len(got) != 5 {
		t.Fatalf("AllBoundaries count = %d, want 5", len(got))
	}
	seen := map[Boundary]bool{}
	for _, b := range got {
		if b == "" {
			t.Errorf("empty boundary in AllBoundaries")
		}
		if seen[b] {
			t.Errorf("duplicate boundary %q", b)
		}
		seen[b] = true
	}
}

// Constructors set canonical class and retriable defaults.

func TestNetwork_RetriableTrue(t *testing.T) {
	f := Network("dial tcp: connection refused")
	if f.Class() != NetworkError {
		t.Errorf("class = %q, want network_error", f.Class())
	}
	if !f.Retriable() {
		t.Error("Network() must be retriable")
	}
	if f.Boundary() != External {
		t.Errorf("boundary = %q, want external", f.Boundary())
	}
}

func TestExternal_5xxRetriable(t *testing.T) {
	for _, s := range []int{500, 502, 503, 599} {
		f := Upstream(s, "upstream error")
		if !f.Retriable() {
			t.Errorf("Upstream(%d) not retriable", s)
		}
		if f.Details()["status"] != s {
			t.Errorf("Upstream(%d).Details[status] = %v", s, f.Details()["status"])
		}
	}
}

func TestExternal_429Retriable(t *testing.T) {
	if !Upstream(429, "rate limited").Retriable() {
		t.Error("Upstream(429) must be retriable")
	}
}

func TestExternal_4xxNotRetriable(t *testing.T) {
	for _, s := range []int{400, 401, 403, 404, 409, 422} {
		if Upstream(s, "client error").Retriable() {
			t.Errorf("Upstream(%d) must NOT be retriable", s)
		}
	}
}

func TestCapabilityDenied_BoundaryRequired(t *testing.T) {
	for _, b := range []Boundary{Sandbox, ConnectorManifest, Action} {
		f := CapabilityDeniedAt(b, "denied")
		if f.Boundary() != b {
			t.Errorf("CapabilityDeniedAt(%q) boundary = %q", b, f.Boundary())
		}
		if f.Retriable() {
			t.Error("CapabilityDenied must not be retriable")
		}
	}
}

func TestBindingRequired_NotRetriable(t *testing.T) {
	f := BindingRequiredFailure("bind oauth2/slack")
	if f.Retriable() {
		t.Error("BindingRequired must not be retriable until bound")
	}
}

func TestBindingFailed_RetriableCallerChosen(t *testing.T) {
	if !BindingFailedFailure("token refresh", true).Retriable() {
		t.Error("BindingFailed retriable=true did not stick")
	}
	if BindingFailedFailure("revoked", false).Retriable() {
		t.Error("BindingFailed retriable=false did not stick")
	}
}

func TestHashMismatch_NotRetriable(t *testing.T) {
	if HashMismatchFailure("sha mismatch").Retriable() {
		t.Error("HashMismatch must not be retriable")
	}
}

func TestSignatureFailure_NotRetriable(t *testing.T) {
	if SignatureFailureFailure("bad sig").Retriable() {
		t.Error("SignatureFailure must not be retriable")
	}
}

func TestResourceLimit_NotRetriable(t *testing.T) {
	f := ResourceLimitFailure("oom")
	if f.Retriable() {
		t.Error("ResourceLimitExceeded must not be retriable")
	}
	if f.Boundary() != Sandbox {
		t.Errorf("boundary = %q, want sandbox", f.Boundary())
	}
}

func TestConnectorRuntime_RetriableCallerChosen(t *testing.T) {
	if ConnectorRuntime("panic", true).Retriable() != true {
		t.Error("ConnectorRuntime retriable=true did not stick")
	}
	if ConnectorRuntime("contract", false).Retriable() {
		t.Error("ConnectorRuntime retriable=false did not stick")
	}
}

// Options.

func TestWithDetails_AttachesAndOverwrites(t *testing.T) {
	f := Network("x", WithDetails(map[string]any{"host": "api.example", "attempt": 1}))
	if f.Details()["host"] != "api.example" {
		t.Error("WithDetails didn't attach host")
	}
	if f.Details()["attempt"] != 1 {
		t.Error("WithDetails didn't attach attempt")
	}
}

func TestWithDetails_MergesWithExternalDefaults(t *testing.T) {
	f := Upstream(503, "x", WithDetails(map[string]any{"retry_after_ms": 500}))
	if f.Details()["status"] != 503 {
		t.Error("External default detail status lost")
	}
	if f.Details()["retry_after_ms"] != 500 {
		t.Error("WithDetails didn't merge")
	}
}

func TestWithCause_Wraps(t *testing.T) {
	cause := errors.New("dial: timeout")
	f := Network("network failure", WithCause(cause))
	if !errors.Is(f, cause) {
		t.Errorf("errors.Is(f, cause) = false, want true")
	}
	if f.Unwrap() != cause {
		t.Error("Unwrap() != cause")
	}
}

func TestWithBoundary_OverridesDefault(t *testing.T) {
	f := CapabilityDeniedAt(Sandbox, "x", WithBoundary(Action))
	if f.Boundary() != Action {
		t.Errorf("boundary = %q, want action (override)", f.Boundary())
	}
}

// Error/Unwrap interface.

func TestError_FormatsClassAndMessage(t *testing.T) {
	f := Network("dial timeout")
	got := f.Error()
	if got != "network_error: dial timeout" {
		t.Errorf("Error() = %q", got)
	}
}

func TestError_FormatsCauseWhenPresent(t *testing.T) {
	f := Network("dial timeout", WithCause(errors.New("broken pipe")))
	got := f.Error()
	if got != "network_error: dial timeout: broken pipe" {
		t.Errorf("Error() = %q", got)
	}
}

// SetAuditID, SetDetail mutators.

func TestSetAuditID_Persists(t *testing.T) {
	f := Network("x")
	f.SetAuditID("audit-abc")
	if f.AuditID() != "audit-abc" {
		t.Errorf("AuditID = %q", f.AuditID())
	}
}

func TestSetDetail_AttachesAndOverwrites(t *testing.T) {
	f := Network("x")
	f.SetDetail("retried", 3)
	if f.Details()["retried"] != 3 {
		t.Errorf("Details[retried] = %v", f.Details()["retried"])
	}
	f.SetDetail("retried", 5)
	if f.Details()["retried"] != 5 {
		t.Error("SetDetail did not overwrite")
	}
}

// Envelope and HTTP.

func TestToEnvelope_RoundTripsToJSON(t *testing.T) {
	f := Upstream(503, "Slack 503", WithDetails(map[string]any{"host": "slack.com"}))
	f.SetAuditID("audit-x")
	body, err := json.Marshal(ToEnvelope(f))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	inner, _ := got["error"].(map[string]any)
	if inner["class"] != "external_api_error" {
		t.Errorf("class = %v", inner["class"])
	}
	if inner["retriable"] != true {
		t.Errorf("retriable = %v", inner["retriable"])
	}
	if inner["audit_id"] != "audit-x" {
		t.Errorf("audit_id = %v", inner["audit_id"])
	}
	d, _ := inner["details"].(map[string]any)
	if d["status"].(float64) != 503 {
		t.Errorf("details.status = %v", d["status"])
	}
}

func TestHTTPStatus_PerClass(t *testing.T) {
	cases := []struct {
		f    *Failure
		want int
	}{
		{CapabilityDeniedAt(Action, "x"), http.StatusForbidden},
		{BindingRequiredFailure("x"), http.StatusPreconditionFailed},
		{BindingFailedFailure("x", false), http.StatusPreconditionFailed},
		{ResourceLimitFailure("x"), http.StatusInsufficientStorage},
		{ConnectorRuntime("x", false), http.StatusInternalServerError},
		{Network("x"), http.StatusBadGateway},
		{Upstream(500, "x"), http.StatusBadGateway},
		{HashMismatchFailure("x"), http.StatusUnprocessableEntity},
		{SignatureFailureFailure("x"), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		got := HTTPStatus(tc.f)
		if got != tc.want {
			t.Errorf("HTTPStatus(%s) = %d, want %d", tc.f.Class(), got, tc.want)
		}
	}
}

func TestWriteHTTP_WritesEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	WriteHTTP(w, Upstream(503, "upstream"))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", w.Header().Get("Content-Type"))
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	inner, _ := got["error"].(map[string]any)
	if inner["class"] != "external_api_error" {
		t.Errorf("class = %v", inner["class"])
	}
}

// Tool-result mappers.

func TestToOpenAIToolMessage_Shape(t *testing.T) {
	f := Upstream(503, "upstream busy")
	msg := ToOpenAIToolMessage(f, "call_1", "ship_update")
	if msg["role"] != "tool" {
		t.Errorf("role = %v", msg["role"])
	}
	if msg["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id = %v", msg["tool_call_id"])
	}
	if msg["name"] != "ship_update" {
		t.Errorf("name = %v", msg["name"])
	}
	content, _ := msg["content"].(string)
	if content == "" {
		t.Error("content empty")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatalf("content not JSON: %v", err)
	}
	if payload["class"] != "external_api_error" {
		t.Errorf("payload.class = %v", payload["class"])
	}
}

func TestToAnthropicToolResult_SetsIsError(t *testing.T) {
	f := Upstream(503, "upstream busy")
	block := ToAnthropicToolResult(f, "tu_1")
	if block["type"] != "tool_result" {
		t.Errorf("type = %v", block["type"])
	}
	if block["tool_use_id"] != "tu_1" {
		t.Errorf("tool_use_id = %v", block["tool_use_id"])
	}
	if block["is_error"] != true {
		t.Errorf("is_error = %v, want true", block["is_error"])
	}
}
