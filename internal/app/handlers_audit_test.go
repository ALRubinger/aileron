package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// Audit handler contract:
//   - GET /v1/audit returns events newest-first as AuditListResponse.
//   - Query params (since, audit_id, connector_fqn, class, limit) compose AND.
//   - GET /v1/audit/{audit_id} returns the event or 404.
//   - Bare server with no audit store → list returns empty, get returns 404.

func newAuditTestServer(t *testing.T, events ...audit.Event) (*apiServer, *audit.MemStore) {
	t.Helper()
	store := audit.NewMemStore()
	for _, e := range events {
		if err := store.Append(context.Background(), e); err != nil {
			t.Fatalf("seed Append: %v", err)
		}
	}
	return &apiServer{
		log:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		auditStore: store,
	}, store
}

func decodeAuditList(t *testing.T, rec *httptest.ResponseRecorder) api.AuditListResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp api.AuditListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body.String())
	}
	return resp
}

func TestListAudit_NoStoreReturnsEmpty(t *testing.T) {
	srv := &apiServer{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 0 {
		t.Errorf("events = %d, want 0", len(resp.Events))
	}
}

func TestListAudit_ReturnsSeededEventsNewestFirst(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	srv, _ := newAuditTestServer(t,
		audit.Event{EventID: "e0", Timestamp: t0, Payload: map[string]any{}},
		audit.Event{EventID: "e2", Timestamp: t0.Add(2 * time.Hour), Payload: map[string]any{}},
		audit.Event{EventID: "e1", Timestamp: t0.Add(time.Hour), Payload: map[string]any{}},
	)
	rec := httptest.NewRecorder()
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(resp.Events))
	}
	want := []string{"e2", "e1", "e0"}
	for i, e := range resp.Events {
		if e.AuditId != want[i] {
			t.Errorf("events[%d].AuditId = %q, want %q", i, e.AuditId, want[i])
		}
	}
}

func TestListAudit_FiltersForwardedToStore(t *testing.T) {
	const fqn = "github://aileron/slack"
	srv, _ := newAuditTestServer(t,
		audit.Event{
			EventID:   "match",
			EventType: model.EventTypeExecutionFailed,
			Payload: map[string]any{
				"class":   "binding_required",
				"details": map[string]any{"connector": fqn},
			},
			Timestamp: time.Now(),
		},
		audit.Event{
			EventID:   "wrong-class",
			EventType: model.EventTypeExecutionFailed,
			Payload: map[string]any{
				"class":   "policy_denied",
				"details": map[string]any{"connector": fqn},
			},
			Timestamp: time.Now(),
		},
	)
	rec := httptest.NewRecorder()
	class := "binding_required"
	connFQN := fqn
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{
		Class:        &class,
		ConnectorFqn: &connFQN,
	})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 1 || resp.Events[0].AuditId != "match" {
		t.Errorf("events = %+v; want only 'match'", resp.Events)
	}
}

func TestListAudit_LimitClamps(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	srv, _ := newAuditTestServer(t,
		audit.Event{EventID: "a", Timestamp: t0, Payload: map[string]any{}},
		audit.Event{EventID: "b", Timestamp: t0.Add(time.Hour), Payload: map[string]any{}},
		audit.Event{EventID: "c", Timestamp: t0.Add(2 * time.Hour), Payload: map[string]any{}},
	)
	limit := 2
	rec := httptest.NewRecorder()
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{Limit: &limit})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(resp.Events))
	}
	if resp.Events[0].AuditId != "c" || resp.Events[1].AuditId != "b" {
		t.Errorf("ids = %q,%q; want c,b", resp.Events[0].AuditId, resp.Events[1].AuditId)
	}
}

func TestGetAudit_FoundReturnsEvent(t *testing.T) {
	srv, _ := newAuditTestServer(t,
		audit.Event{
			EventID:   "audit-xyz",
			EventType: model.EventTypeActionInstalled,
			Actor:     model.ActorRef{Type: model.ActorTypeHuman, ID: "user"},
			Payload:   map[string]any{"name": "ship-update"},
			Timestamp: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		},
	)
	rec := httptest.NewRecorder()
	srv.GetAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/audit-xyz", nil), "audit-xyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got api.AuditEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AuditId != "audit-xyz" {
		t.Errorf("AuditId = %q", got.AuditId)
	}
	if got.EventType != string(model.EventTypeActionInstalled) {
		t.Errorf("EventType = %q", got.EventType)
	}
	if got.Actor.Id != "user" || got.Actor.Type != string(model.ActorTypeHuman) {
		t.Errorf("Actor = %+v", got.Actor)
	}
	if got.Payload["name"] != "ship-update" {
		t.Errorf("Payload.name = %v", got.Payload["name"])
	}
}

func TestGetAudit_NotFoundReturns404(t *testing.T) {
	srv, _ := newAuditTestServer(t)
	rec := httptest.NewRecorder()
	srv.GetAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/missing", nil), "missing")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestGetAudit_NoStoreReturns404(t *testing.T) {
	srv := &apiServer{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()
	srv.GetAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/x", nil), "x")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestListAudit_SinceFilterAlone narrows on Since only, exercising the
// param.Since branch independently of class/connector filters.
func TestListAudit_SinceFilterAlone(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	srv, _ := newAuditTestServer(t,
		audit.Event{EventID: "old", Timestamp: t0, Payload: map[string]any{}},
		audit.Event{EventID: "new", Timestamp: t0.Add(time.Hour), Payload: map[string]any{}},
	)
	cutoff := t0.Add(30 * time.Minute)
	rec := httptest.NewRecorder()
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{Since: &cutoff})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 1 || resp.Events[0].AuditId != "new" {
		t.Errorf("got = %+v; want only 'new'", resp.Events)
	}
}

// TestListAudit_AuditIDFilterAlone exercises the AuditId-only branch.
func TestListAudit_AuditIDFilterAlone(t *testing.T) {
	srv, _ := newAuditTestServer(t,
		audit.Event{EventID: "want", Timestamp: time.Now(), Payload: map[string]any{}},
		audit.Event{EventID: "skip", Timestamp: time.Now(), Payload: map[string]any{}},
	)
	id := "want"
	rec := httptest.NewRecorder()
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{AuditId: &id})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 1 || resp.Events[0].AuditId != "want" {
		t.Errorf("got = %+v; want only 'want'", resp.Events)
	}
}
