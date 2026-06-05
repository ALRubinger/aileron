package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

func newSandboxShellTestServer() (*apiServer, *audit.MemStore) {
	store := audit.NewMemStore()
	return &apiServer{
		log:           slog.New(slog.NewJSONHandler(io.Discard, nil)),
		auditStore:    store,
		auditRecorder: audit.NewRecorder(store, nil, func() string { return "audit-shell-decision" }),
	}, store
}

func TestDecideSandboxShellCommand_AllowsAndAuditsSanitizedPayload(t *testing.T) {
	srv, auditStore := newSandboxShellTestServer()
	body := []byte(`{
		"command":"printf '%s' \"$SECRET\"",
		"cwd":"/home/agent/workspace",
		"shell":"/bin/bash",
		"pid":123,
		"ppid":45,
		"env":{"SECRET":"leak-me"},
		"output":"leak-me"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-shell/decide", bytes.NewReader(body))
	req.Header.Set("X-Aileron-Session-Id", "session-123")
	rec := httptest.NewRecorder()

	srv.DecideSandboxShellCommand(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp api.SandboxShellDecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Status != sandboxShellDecisionStatusDecided {
		t.Errorf("status = %q, want decided", resp.Status)
	}
	if resp.Decision != sandboxShellDecisionAllow {
		t.Errorf("decision = %q, want allow", resp.Decision)
	}
	if resp.AuditId != "audit-shell-decision" {
		t.Errorf("audit_id = %q", resp.AuditId)
	}
	if resp.Reason == nil || *resp.Reason == "" {
		t.Error("reason should explain allow-only contract")
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.EventType != model.EventTypeSandboxShellDecided {
		t.Fatalf("event type = %q", event.EventType)
	}
	if event.Actor.Type != model.ActorTypeAgent || event.Actor.ID != "sandbox-shell" {
		t.Fatalf("actor = %+v", event.Actor)
	}
	payload := event.Payload
	if payload["aileron.shell.boundary"] != "sandbox_shell" {
		t.Errorf("boundary = %v", payload["aileron.shell.boundary"])
	}
	if payload["aileron.shell.command"] != "printf '%s' \"$SECRET\"" {
		t.Errorf("command = %v", payload["aileron.shell.command"])
	}
	if payload["aileron.shell.cwd"] != "/home/agent/workspace" {
		t.Errorf("cwd = %v", payload["aileron.shell.cwd"])
	}
	if payload["aileron.shell.path"] != "/bin/bash" {
		t.Errorf("shell path = %v", payload["aileron.shell.path"])
	}
	if payload["aileron.shell.pid"] != float64(123) && payload["aileron.shell.pid"] != 123 {
		t.Errorf("pid = %v", payload["aileron.shell.pid"])
	}
	if payload["aileron.shell.ppid"] != float64(45) && payload["aileron.shell.ppid"] != 45 {
		t.Errorf("ppid = %v", payload["aileron.shell.ppid"])
	}
	if payload["aileron.shell.decision"] != "allow" {
		t.Errorf("decision = %v", payload["aileron.shell.decision"])
	}
	if payload["aileron.session.id"] != "session-123" {
		t.Errorf("session = %v", payload["aileron.session.id"])
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	for _, leaked := range []string{"leak-me", "env", "output"} {
		if strings.Contains(string(payloadJSON), leaked) {
			t.Fatalf("audit payload leaked disallowed field/value %q: %s", leaked, payloadJSON)
		}
	}
}

func TestDecideSandboxShellCommand_RequiresCommand(t *testing.T) {
	srv, auditStore := newSandboxShellTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-shell/decide", strings.NewReader(`{"cwd":"/home/agent/workspace"}`))
	rec := httptest.NewRecorder()

	srv.DecideSandboxShellCommand(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0", len(events))
	}
}

func TestDecideSandboxShellCommand_NoRecorderReturnsFallbackAuditID(t *testing.T) {
	srv, _ := newSandboxShellTestServer()
	srv.auditRecorder = nil
	srv.newID = func() string { return "audit-fallback" }
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-shell/decide", strings.NewReader(`{"command":"true"}`))
	rec := httptest.NewRecorder()

	srv.DecideSandboxShellCommand(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp api.SandboxShellDecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AuditId != "audit-fallback" {
		t.Fatalf("audit_id = %q, want fallback", resp.AuditId)
	}
}
