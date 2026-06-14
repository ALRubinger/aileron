package app

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// newSandboxProxyDisabledTestServer returns an apiServer with a
// MemStore-backed audit recorder, suitable for asserting on the
// emitted sandbox.proxy.disabled event payloads.
func newSandboxProxyDisabledTestServer(t *testing.T) (*apiServer, *audit.MemStore) {
	t.Helper()
	store := audit.NewMemStore()
	srv := &apiServer{
		auditRecorder: audit.NewRecorder(store, nil, func() string { return "audit-sandbox-proxy-disabled" }),
	}
	return srv, store
}

// recordSandboxProxyDisabledRequest builds a POST request body with the
// given fields and dispatches it to the handler. Returns the recorder
// for caller assertions.
func recordSandboxProxyDisabledRequest(t *testing.T, srv *apiServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/disabled", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.RecordSandboxProxyDisabled(rec, req)
	return rec
}

// TestRecordSandboxProxyDisabled_UserOptOut covers reason=user_opt_out:
// the launcher disabled bootstrap via --sandbox-proxy=off or
// AILERON_SANDBOX_PROXY=off and posts a record so audit captures it.
func TestRecordSandboxProxyDisabled_UserOptOut(t *testing.T) {
	srv, store := newSandboxProxyDisabledTestServer(t)
	rec := recordSandboxProxyDisabledRequest(t, srv, `{
		"session_id": "01HKABC",
		"reason": "user_opt_out",
		"sandbox_mode": "docker",
		"sandbox_image": "ghcr.io/acme/agent:latest"
	}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	events, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyDisabled {
		t.Fatalf("event type = %q, want %q", events[0].EventType, model.EventTypeSandboxProxyDisabled)
	}
	payload := events[0].Payload
	for key, want := range map[string]string{
		"aileron.proxy.source":          "launcher",
		"aileron.proxy.boundary":        "https_proxy",
		"aileron.proxy.decision":        "disabled",
		"aileron.proxy.disabled_reason": "user_opt_out",
		"aileron.session.id":            "01HKABC",
		"aileron.sandbox.mode":          "docker",
		"aileron.sandbox.image":         "ghcr.io/acme/agent:latest",
	} {
		if got, _ := payload[key].(string); got != want {
			t.Errorf("payload[%s] = %v, want %q", key, payload[key], want)
		}
	}
}

// TestRecordSandboxProxyDisabled_PreflightFailed covers
// reason=preflight_failed: bootstrap was requested but the resolved
// image lacks the required helpers, so the launcher refuses to start
// the container and emits the event for audit.
func TestRecordSandboxProxyDisabled_PreflightFailed(t *testing.T) {
	srv, store := newSandboxProxyDisabledTestServer(t)
	rec := recordSandboxProxyDisabledRequest(t, srv, `{
		"session_id": "01HKABC",
		"reason": "preflight_failed",
		"sandbox_mode": "docker",
		"sandbox_image": "ghcr.io/acme/agent:latest"
	}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	events, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if got := events[0].Payload["aileron.proxy.disabled_reason"]; got != "preflight_failed" {
		t.Fatalf("disabled_reason = %v, want preflight_failed", got)
	}
}

// TestRecordSandboxProxyDisabled_UnsupportedSandboxMode covers
// reason=unsupported_sandbox_mode: --sandbox=off (or any non-container
// mode) doesn't support bootstrap, so audit records why no proxy
// activated for the launch.
func TestRecordSandboxProxyDisabled_UnsupportedSandboxMode(t *testing.T) {
	srv, store := newSandboxProxyDisabledTestServer(t)
	rec := recordSandboxProxyDisabledRequest(t, srv, `{
		"session_id": "01HKABC",
		"reason": "unsupported_sandbox_mode",
		"sandbox_mode": "off"
	}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	events, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	payload := events[0].Payload
	if payload["aileron.proxy.disabled_reason"] != "unsupported_sandbox_mode" {
		t.Errorf("disabled_reason = %v", payload["aileron.proxy.disabled_reason"])
	}
	if payload["aileron.sandbox.mode"] != "off" {
		t.Errorf("sandbox.mode = %v", payload["aileron.sandbox.mode"])
	}
	if _, ok := payload["aileron.sandbox.image"]; ok {
		t.Errorf("sandbox.image should be absent when not provided; payload=%v", payload)
	}
}

// TestRecordSandboxProxyDisabled_RejectsMalformedPayloads covers the
// 400 paths: missing session_id, missing reason, unknown reason
// enum, and malformed JSON. None of these record an audit event.
func TestRecordSandboxProxyDisabled_RejectsMalformedPayloads(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "missing_session_id", body: `{"reason":"user_opt_out"}`, want: "session_id"},
		{name: "missing_reason", body: `{"session_id":"sess-1"}`, want: "reason"},
		{name: "unknown_reason", body: `{"session_id":"sess-1","reason":"wat"}`, want: "reason"},
		{name: "blank_session_id", body: `{"session_id":"   ","reason":"user_opt_out"}`, want: "session_id"},
		{name: "invalid_json", body: `not-json`, want: "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, store := newSandboxProxyDisabledTestServer(t)
			rec := recordSandboxProxyDisabledRequest(t, srv, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body = %s, missing %q", rec.Body.String(), tc.want)
			}
			events, err := store.ListEvents(context.Background(), audit.EventFilter{})
			if err != nil {
				t.Fatalf("ListEvents: %v", err)
			}
			if len(events) != 0 {
				t.Errorf("malformed payload should not record events, got %d", len(events))
			}
		})
	}
}

// TestRecordSandboxProxyDisabled_AcceptsWithoutAuditRecorder covers
// the daemon-not-fully-configured path: when auditRecorder is nil the
// handler still returns 204 (the endpoint is the audit-emission
// boundary; with no recorder there's nothing to do, but treating it
// as a server error would leak daemon config shape to the launcher).
func TestRecordSandboxProxyDisabled_AcceptsWithoutAuditRecorder(t *testing.T) {
	srv := &apiServer{} // no auditRecorder
	rec := recordSandboxProxyDisabledRequest(t, srv, `{"session_id":"sess-1","reason":"user_opt_out"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}
