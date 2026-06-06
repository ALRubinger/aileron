package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/vault"
)

func newSandboxShellTestServer() (*apiServer, *audit.MemStore) {
	store := audit.NewMemStore()
	return &apiServer{
		log:           slog.New(slog.NewJSONHandler(io.Discard, nil)),
		auditStore:    store,
		auditRecorder: audit.NewRecorder(store, nil, func() string { return "audit-shell-decision" }),
	}, store
}

// withDenyPattern returns a new test server whose deny pattern is the supplied
// regex source. Tests for the slice-6 deny path use this helper to set up the
// daemon-scoped pattern the production code reads at startup.
func newSandboxShellTestServerWithDenyPattern(t *testing.T, patternSource string) (*apiServer, *audit.MemStore) {
	t.Helper()
	srv, store := newSandboxShellTestServer()
	re, err := regexp.Compile(patternSource)
	if err != nil {
		t.Fatalf("compile test deny pattern %q: %v", patternSource, err)
	}
	srv.sandboxShellDenyPattern = re
	return srv, store
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
	if resp.Decision != api.SandboxShellDecisionResponseDecisionAllow {
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
	switch latency := payload["aileron.shell.latency_ms"].(type) {
	case int64:
		if latency < 0 {
			t.Errorf("latency_ms = %d, want non-negative", latency)
		}
	case float64:
		if latency < 0 {
			t.Errorf("latency_ms = %v, want non-negative", latency)
		}
	default:
		t.Errorf("latency_ms missing or wrong type: %v (%T)", payload["aileron.shell.latency_ms"], payload["aileron.shell.latency_ms"])
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

func TestDecideSandboxShellCommand_DenyMatchEmitsDenyDecisionAndAudit(t *testing.T) {
	const pattern = "^rm -rf"
	srv, auditStore := newSandboxShellTestServerWithDenyPattern(t, pattern)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-shell/decide", strings.NewReader(`{"command":"rm -rf /","cwd":"/home/agent/workspace","shell":"/bin/bash","pid":99,"env":{"SECRET":"leak-me"},"output":"leak-me"}`))
	req.Header.Set("X-Aileron-Session-Id", "session-deny")
	rec := httptest.NewRecorder()

	srv.DecideSandboxShellCommand(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp api.SandboxShellDecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Decision != api.SandboxShellDecisionResponseDecisionDeny {
		t.Errorf("decision = %q, want deny", resp.Decision)
	}
	if resp.Reason == nil {
		t.Fatalf("reason should be populated on deny")
	}
	// KTD2 stable format: "matched deny pattern: <pattern source>" — assert the
	// prefix is the locked observable shape downstream consumers will pattern
	// match on, and assert the verbatim pattern follows.
	prefixRE := regexp.MustCompile(`^matched deny pattern: `)
	if !prefixRE.MatchString(*resp.Reason) {
		t.Errorf("reason = %q, want prefix %q", *resp.Reason, "matched deny pattern: ")
	}
	wantReason := "matched deny pattern: " + pattern
	if *resp.Reason != wantReason {
		t.Errorf("reason = %q, want %q", *resp.Reason, wantReason)
	}
	if resp.MatchedPattern == nil || *resp.MatchedPattern != pattern {
		t.Errorf("matched_pattern = %v, want %q", resp.MatchedPattern, pattern)
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	payload := events[0].Payload
	if payload["aileron.shell.decision"] != "deny" {
		t.Errorf("audit decision = %v, want deny", payload["aileron.shell.decision"])
	}
	if payload["aileron.shell.reason"] != wantReason {
		t.Errorf("audit reason = %v, want %q", payload["aileron.shell.reason"], wantReason)
	}
	if payload["aileron.shell.matched_pattern"] != pattern {
		t.Errorf("audit matched_pattern = %v, want %q", payload["aileron.shell.matched_pattern"], pattern)
	}
	switch latency := payload["aileron.shell.latency_ms"].(type) {
	case int64:
		if latency < 0 {
			t.Errorf("latency_ms = %d, want non-negative on deny", latency)
		}
	default:
		t.Errorf("latency_ms missing or wrong type on deny: %v (%T)", payload["aileron.shell.latency_ms"], payload["aileron.shell.latency_ms"])
	}
	// Payload leak guard reused verbatim from the allow test: env/output/secret
	// values stay out of the audit payload on the deny path too.
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

func TestDecideSandboxShellCommand_DenyPatternMissAllowsWithNoMatchReason(t *testing.T) {
	const pattern = "^never-matches"
	srv, auditStore := newSandboxShellTestServerWithDenyPattern(t, pattern)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-shell/decide", strings.NewReader(`{"command":"ls -la"}`))
	rec := httptest.NewRecorder()

	srv.DecideSandboxShellCommand(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp api.SandboxShellDecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Decision != api.SandboxShellDecisionResponseDecisionAllow {
		t.Errorf("decision = %q, want allow", resp.Decision)
	}
	if resp.Reason == nil || *resp.Reason != sandboxShellDecisionReasonAllowNoMatch {
		t.Errorf("reason = %v, want %q", resp.Reason, sandboxShellDecisionReasonAllowNoMatch)
	}
	if resp.MatchedPattern != nil {
		t.Errorf("matched_pattern = %v, want absent on allow", *resp.MatchedPattern)
	}
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Payload["aileron.shell.decision"] != "allow" {
		t.Errorf("audit decision = %v, want allow", events[0].Payload["aileron.shell.decision"])
	}
	if _, ok := events[0].Payload["aileron.shell.matched_pattern"]; ok {
		t.Errorf("audit matched_pattern present on allow: %v", events[0].Payload["aileron.shell.matched_pattern"])
	}
}

func TestLoadSandboxShellDenyPattern_UnsetReturnsNil(t *testing.T) {
	t.Setenv(sandboxShellDenyPatternEnv, "")
	re, err := loadSandboxShellDenyPattern()
	if err != nil {
		t.Fatalf("unset env returned error: %v", err)
	}
	if re != nil {
		t.Errorf("unset env produced non-nil regex: %v", re)
	}
}

func TestLoadSandboxShellDenyPattern_ValidCompiles(t *testing.T) {
	t.Setenv(sandboxShellDenyPatternEnv, "^rm -rf")
	re, err := loadSandboxShellDenyPattern()
	if err != nil {
		t.Fatalf("valid pattern returned error: %v", err)
	}
	if re == nil {
		t.Fatal("valid pattern produced nil regex")
	}
	if !re.MatchString("rm -rf /") {
		t.Error("compiled regex did not match expected input")
	}
}

func TestLoadSandboxShellDenyPattern_InvalidRefusesToStart(t *testing.T) {
	const bad = "^("
	t.Setenv(sandboxShellDenyPatternEnv, bad)
	re, err := loadSandboxShellDenyPattern()
	if err == nil {
		t.Fatalf("invalid pattern returned nil error; regex=%v", re)
	}
	if re != nil {
		t.Errorf("invalid pattern produced non-nil regex: %v", re)
	}
	// Error message must name the env var so the operator can locate the
	// misconfig immediately; it must also include something that anchors back
	// to the supplied (invalid) source so log-readers know which value broke.
	if !strings.Contains(err.Error(), sandboxShellDenyPatternEnv) {
		t.Errorf("error %q does not name the env var %q", err, sandboxShellDenyPatternEnv)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error %q does not echo the invalid pattern %q", err, bad)
	}
}

// TestNewHandlerWithConfig_RefusesInvalidDenyPattern proves R7 fail-closed at
// the constructor seam: a set-but-invalid AILERON_SANDBOX_SHELL_DENY_PATTERN
// causes daemon construction to fail. The handler is never returned, so
// /v1/sandbox-shell/decide is never registered, which is the operator-visible
// promise — a sandbox the user thought was locked down cannot silently fall
// through to allow.
func TestNewHandlerWithConfig_RefusesInvalidDenyPattern(t *testing.T) {
	t.Setenv(sandboxShellDenyPatternEnv, "^(")
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler, err := NewHandlerWithConfig(log, Config{Vault: vault.NewMemVault()})
	if err == nil {
		t.Fatalf("expected constructor to fail on invalid deny pattern; got handler=%v", handler)
	}
	if handler != nil {
		t.Errorf("constructor returned a handler despite invalid pattern: %v", handler)
	}
	if !strings.Contains(err.Error(), sandboxShellDenyPatternEnv) {
		t.Errorf("error %q does not name the env var %q", err, sandboxShellDenyPatternEnv)
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
