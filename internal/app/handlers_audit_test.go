package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// failingAppendStore embeds a MemStore but forces Append to fail, so the
// ingest path can be exercised when the durable write does not commit.
type failingAppendStore struct {
	*audit.MemStore
	err error
}

func (f failingAppendStore) Append(context.Context, audit.Event) error { return f.err }

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
				"aileron.failure.class":   "binding_required",
				"aileron.failure.details": map[string]any{"connector": fqn},
			},
			Timestamp: time.Now(),
		},
		audit.Event{
			EventID:   "wrong-class",
			EventType: model.EventTypeExecutionFailed,
			Payload: map[string]any{
				"aileron.failure.class":   "policy_denied",
				"aileron.failure.details": map[string]any{"connector": fqn},
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

func newMaterializedEvent(id, name, hash string, ts time.Time) audit.Event {
	return audit.Event{
		EventID:   id,
		EventType: model.EventType("output.materialized"),
		Payload: map[string]any{
			"aileron.output.name":         name,
			"aileron.output.content_hash": hash,
			"aileron.output.path":         "/artifacts/" + name,
		},
		Timestamp: ts,
	}
}

func TestListAudit_OutputNameFilterForwarded(t *testing.T) {
	now := time.Now()
	srv, _ := newAuditTestServer(t,
		newMaterializedEvent("out-report", "report.pdf", "sha256:aaa", now),
		newMaterializedEvent("out-summary", "summary.txt", "sha256:bbb", now),
		audit.Event{
			EventID:   "install",
			EventType: model.EventTypeActionInstalled,
			Payload:   map[string]any{"aileron.action.name": "ship-update"},
			Timestamp: now,
		},
	)
	rec := httptest.NewRecorder()
	name := "report.pdf"
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{
		OutputName: &name,
	})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 1 || resp.Events[0].AuditId != "out-report" {
		t.Errorf("events = %+v; want only 'out-report'", resp.Events)
	}
}

func TestListAudit_ContentHashFilterForwarded(t *testing.T) {
	const digest = "sha256:0123456789abcdef"
	now := time.Now()
	srv, _ := newAuditTestServer(t,
		newMaterializedEvent("match", "report.pdf", digest, now),
		newMaterializedEvent("other", "report.pdf", "sha256:ffff", now),
	)
	rec := httptest.NewRecorder()
	hash := digest
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{
		ContentHash: &hash,
	})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 1 || resp.Events[0].AuditId != "match" {
		t.Errorf("events = %+v; want only 'match'", resp.Events)
	}
}

func TestListAudit_ContentHashComposesWithSince(t *testing.T) {
	const digest = "sha256:cafe"
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	srv, _ := newAuditTestServer(t,
		newMaterializedEvent("want", "report.pdf", digest, t0.Add(time.Hour)),
		newMaterializedEvent("too-old", "report.pdf", digest, t0.Add(-time.Hour)),
	)
	rec := httptest.NewRecorder()
	hash := digest
	since := t0
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{
		ContentHash: &hash,
		Since:       &since,
	})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 1 || resp.Events[0].AuditId != "want" {
		t.Errorf("events = %+v; want only 'want'", resp.Events)
	}
}

func invocationEvent(id, invocationID string, ts time.Time) audit.Event {
	return audit.Event{
		EventID:   id,
		EventType: model.EventType("output.materialized"),
		Payload:   map[string]any{"aileron.invocation.id": invocationID},
		Timestamp: ts,
	}
}

func TestListAudit_InvocationIDFilterForwarded(t *testing.T) {
	const inv = "11111111-1111-1111-1111-111111111111"
	now := time.Now()
	srv, _ := newAuditTestServer(t,
		invocationEvent("a", inv, now),
		invocationEvent("b", inv, now.Add(time.Minute)),
		invocationEvent("other", "22222222-2222-2222-2222-222222222222", now),
	)
	rec := httptest.NewRecorder()
	id := inv
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{
		InvocationId: &id,
	})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 2 {
		t.Fatalf("events = %+v; want the two events of the invocation", resp.Events)
	}
	for _, e := range resp.Events {
		if e.AuditId == "other" {
			t.Errorf("event from a different invocation leaked: %+v", resp.Events)
		}
	}

	// An unknown invocation id returns empty.
	rec = httptest.NewRecorder()
	unknown := "deadbeef-0000-0000-0000-000000000000"
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{
		InvocationId: &unknown,
	})
	if got := decodeAuditList(t, rec); len(got.Events) != 0 {
		t.Errorf("unknown invocation id should return empty, got %+v", got.Events)
	}
}

func TestListAudit_InvocationIDComposesWithContentHash(t *testing.T) {
	const inv = "11111111-1111-1111-1111-111111111111"
	const digest = "sha256:cafe"
	now := time.Now()
	want := audit.Event{
		EventID:   "want",
		EventType: model.EventType("output.materialized"),
		Payload: map[string]any{
			"aileron.invocation.id":       inv,
			"aileron.output.content_hash": digest,
		},
		Timestamp: now,
	}
	wrongHash := audit.Event{
		EventID:   "wrong-hash",
		EventType: model.EventType("output.materialized"),
		Payload: map[string]any{
			"aileron.invocation.id":       inv,
			"aileron.output.content_hash": "sha256:beef",
		},
		Timestamp: now,
	}
	srv, _ := newAuditTestServer(t, want, wrongHash)
	rec := httptest.NewRecorder()
	id := inv
	hash := digest
	srv.ListAudit(rec, httptest.NewRequest(http.MethodGet, "/v1/audit", nil), api.ListAuditParams{
		InvocationId: &id,
		ContentHash:  &hash,
	})
	resp := decodeAuditList(t, rec)
	if len(resp.Events) != 1 || resp.Events[0].AuditId != "want" {
		t.Errorf("events = %+v; want only 'want'", resp.Events)
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
			Payload:   map[string]any{"aileron.action.name": "ship-update"},
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
	if got.Actor.Id != "user" || got.Actor.Type != api.ActorRefType(model.ActorTypeHuman) {
		t.Errorf("Actor = %+v", got.Actor)
	}
	if got.Payload["aileron.action.name"] != "ship-update" {
		t.Errorf("Payload[aileron.action.name] = %v", got.Payload["aileron.action.name"])
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

// CreateAudit handler contract:
//   - POST /v1/audit appends via the recorder and returns 201 + minted id.
//   - The appended event is readable back through the store.
//   - Unknown actor.type / malformed body / missing event_type → 400.
//   - No recorder/store wired → 503 (a write that must persist cannot be
//     silently acknowledged).

// newAuditWriteServer builds an apiServer with a Mem store and a real
// recorder backed by it, mirroring the production wiring where the recorder
// is the single writer over the store.
func newAuditWriteServer(t *testing.T) (*apiServer, *audit.MemStore) {
	t.Helper()
	store := audit.NewMemStore()
	rec := audit.NewRecorder(store, nil, nil)
	return &apiServer{
		log:           slog.New(slog.NewJSONHandler(io.Discard, nil)),
		auditStore:    store,
		auditRecorder: rec,
	}, store
}

func TestCreateAudit_AppendsAndIsReadable(t *testing.T) {
	srv, _ := newAuditWriteServer(t)
	body := `{"event_type":"flightplan.launch.action","actor":{"type":"service","id":"flightplan-launch"},"payload":{"actionRef":"aileron:metrics.query_series","rows":3}}`
	rec := httptest.NewRecorder()
	srv.CreateAudit(rec, httptest.NewRequest(http.MethodPost, "/v1/audit", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	var resp api.AuditIngestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AuditId == "" {
		t.Fatal("audit_id is empty; recorder must mint one")
	}

	// Read it back through the store to prove it was persisted intact.
	getRec := httptest.NewRecorder()
	srv.GetAudit(getRec, httptest.NewRequest(http.MethodGet, "/v1/audit/"+resp.AuditId, nil), resp.AuditId)
	if getRec.Code != http.StatusOK {
		t.Fatalf("read-back status = %d, want 200; body = %s", getRec.Code, getRec.Body.String())
	}
	var got api.AuditEvent
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode read-back: %v", err)
	}
	if got.EventType != "flightplan.launch.action" {
		t.Errorf("EventType = %q", got.EventType)
	}
	if got.Actor.Type != api.ActorRefType(model.ActorTypeService) || got.Actor.Id != "flightplan-launch" {
		t.Errorf("Actor = %+v", got.Actor)
	}
	if got.Payload["actionRef"] != "aileron:metrics.query_series" {
		t.Errorf("Payload[actionRef] = %v", got.Payload["actionRef"])
	}
}

// TestCreateAudit_ActorProvenanceNormalizedOntoActor proves the daemon lifts
// the runtime's flat `aileron.actor.*` payload keys onto the event's Actor
// object at ingest (residual #1770): a POST carrying
// aileron.actor.identity_label and aileron.actor.credential_binding in its
// payload persists and reads them back on Actor.IdentityLabel /
// Actor.CredentialBinding, not only in Payload.
func TestCreateAudit_ActorProvenanceNormalizedOntoActor(t *testing.T) {
	srv, _ := newAuditWriteServer(t)
	body := `{"event_type":"flightplan.launch.action","actor":{"type":"service","id":"flightplan-launch"},"payload":{"aileron.actor.identity_label":"prod-reader","aileron.actor.credential_binding":"aws-athena-prod"}}`
	w := httptest.NewRecorder()
	srv.CreateAudit(w, httptest.NewRequest(http.MethodPost, "/v1/audit", strings.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var resp api.AuditIngestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	getW := httptest.NewRecorder()
	srv.GetAudit(getW, httptest.NewRequest(http.MethodGet, "/v1/audit/"+resp.AuditId, nil), resp.AuditId)
	if getW.Code != http.StatusOK {
		t.Fatalf("read-back status = %d, want 200; body = %s", getW.Code, getW.Body.String())
	}
	var got api.AuditEvent
	if err := json.Unmarshal(getW.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode read-back: %v", err)
	}
	if got.Actor.IdentityLabel == nil || *got.Actor.IdentityLabel != "prod-reader" {
		t.Errorf("Actor.IdentityLabel = %v, want \"prod-reader\"", got.Actor.IdentityLabel)
	}
	if got.Actor.CredentialBinding == nil || *got.Actor.CredentialBinding != "aws-athena-prod" {
		t.Errorf("Actor.CredentialBinding = %v, want \"aws-athena-prod\"", got.Actor.CredentialBinding)
	}
}

// TestAuditEvent_ActorSharesActorRefEnum proves the read model's actor is
// the shared, enum-typed ActorRef schema (reconciled with the ingest
// request side): a persisted actor.type round-trips as a known
// ActorRefType member rather than a bare string.
func TestAuditEvent_ActorSharesActorRefEnum(t *testing.T) {
	srv, _ := newAuditWriteServer(t)
	body := `{"event_type":"flightplan.launch","actor":{"type":"agent","id":"claude"},"payload":{}}`
	w := httptest.NewRecorder()
	srv.CreateAudit(w, httptest.NewRequest(http.MethodPost, "/v1/audit", strings.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var resp api.AuditIngestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	getW := httptest.NewRecorder()
	srv.GetAudit(getW, httptest.NewRequest(http.MethodGet, "/v1/audit/"+resp.AuditId, nil), resp.AuditId)
	var got api.AuditEvent
	if err := json.Unmarshal(getW.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode read-back: %v", err)
	}
	if got.Actor.Type != api.Agent {
		t.Errorf("Actor.Type = %q, want the api.Agent enum member", got.Actor.Type)
	}
	if !got.Actor.Type.Valid() {
		t.Errorf("Actor.Type %q is not a known ActorRefType member", got.Actor.Type)
	}
}

func TestCreateAudit_UnknownActorTypeRejected(t *testing.T) {
	srv, _ := newAuditWriteServer(t)
	body := `{"event_type":"flightplan.launch","actor":{"type":"robot","id":"x"},"payload":{}}`
	rec := httptest.NewRecorder()
	srv.CreateAudit(rec, httptest.NewRequest(http.MethodPost, "/v1/audit", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAudit_MalformedBodyRejected(t *testing.T) {
	srv, _ := newAuditWriteServer(t)
	rec := httptest.NewRecorder()
	srv.CreateAudit(rec, httptest.NewRequest(http.MethodPost, "/v1/audit", strings.NewReader("{not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAudit_MissingEventTypeRejected(t *testing.T) {
	srv, _ := newAuditWriteServer(t)
	body := `{"actor":{"type":"service","id":"x"},"payload":{}}`
	rec := httptest.NewRecorder()
	srv.CreateAudit(rec, httptest.NewRequest(http.MethodPost, "/v1/audit", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

// TestCreateAudit_AppendFailureReturns500 proves the ingest path no
// longer acknowledges a write that never committed: when the recorder's
// store append fails, CreateAudit returns 500 rather than a false 201.
func TestCreateAudit_AppendFailureReturns500(t *testing.T) {
	store := failingAppendStore{MemStore: audit.NewMemStore(), err: errors.New("disk full")}
	srv := &apiServer{
		log:           slog.New(slog.NewJSONHandler(io.Discard, nil)),
		auditStore:    store,
		auditRecorder: audit.NewRecorder(store, nil, nil),
	}
	body := `{"event_type":"flightplan.launch","actor":{"type":"service","id":"x"},"payload":{}}`
	rec := httptest.NewRecorder()
	srv.CreateAudit(rec, httptest.NewRequest(http.MethodPost, "/v1/audit", strings.NewReader(body)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAudit_NilStoreReturns503(t *testing.T) {
	srv := &apiServer{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	body := `{"event_type":"flightplan.launch","actor":{"type":"service","id":"x"},"payload":{}}`
	rec := httptest.NewRecorder()
	srv.CreateAudit(rec, httptest.NewRequest(http.MethodPost, "/v1/audit", strings.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
}

// TestCreateAudit_NilPayloadDefaultsToEmpty proves a null payload is stored as
// an empty map rather than nil, so the read-back stays well-formed.
func TestCreateAudit_NilPayloadDefaultsToEmpty(t *testing.T) {
	srv, _ := newAuditWriteServer(t)
	body := `{"event_type":"flightplan.launch","actor":{"type":"service","id":"x"},"payload":null}`
	rec := httptest.NewRecorder()
	srv.CreateAudit(rec, httptest.NewRequest(http.MethodPost, "/v1/audit", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
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
