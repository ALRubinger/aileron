package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/sessions"
	"github.com/ALRubinger/aileron/internal/sessions/jsonl"
)

// /v1/sessions contract (#454 step 4):
//
//   - POST /v1/sessions creates a record with a daemon-minted ULID.
//   - POST /v1/sessions/{id}/end stamps EndedAt and stores ExitCode.
//   - GET /v1/sessions returns the persisted records, newest-first.
//   - GET /v1/sessions/{id} returns one or 404.
//   - All endpoints return 503 when no sessions store is configured.

func newSessionsServer(t *testing.T) (*apiServer, sessions.Store) {
	t.Helper()
	store, err := jsonl.New(filepath.Join(t.TempDir(), "sessions.jsonl"))
	if err != nil {
		t.Fatalf("jsonl.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &apiServer{
		log:      slog.Default(),
		sessions: store,
	}, store
}

func TestCreateSession_HappyPath(t *testing.T) {
	s, _ := newSessionsServer(t)

	body := mustJSON(t, api.CreateSessionRequest{
		Agent:      "claude",
		WorkingDir: ptr("/work/proj"),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.CreateSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got api.Session
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Id) != 26 {
		t.Errorf("Id length = %d, want 26 (ULID)", len(got.Id))
	}
	if got.Agent != "claude" || got.WorkingDir != "/work/proj" {
		t.Errorf("got = %+v", got)
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
	if got.EndedAt != nil {
		t.Errorf("EndedAt should be nil on creation, got %v", got.EndedAt)
	}
}

// TestCreateSession_MintsValidatableCaveat covers ADR-0024 / #958: when
// the daemon is configured with a caveat signing key, CreateSession
// returns a caveat_token bound to the new session id and carrying the
// full sandbox-mcp capability set. The token must validate under the
// same key.
func TestCreateSession_MintsValidatableCaveat(t *testing.T) {
	s, _ := newSessionsServer(t)
	key, err := auth.GenerateCaveatSigningKey()
	if err != nil {
		t.Fatalf("GenerateCaveatSigningKey: %v", err)
	}
	issuer := auth.NewCaveatIssuer(key, "aileron-daemon")
	s.caveatIssuer = issuer

	body := mustJSON(t, api.CreateSessionRequest{Agent: "claude", WorkingDir: ptr("/work")})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.CreateSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got api.CreateSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CaveatToken == nil || *got.CaveatToken == "" {
		t.Fatal("response missing caveat_token")
	}
	claims, err := issuer.Validate(*got.CaveatToken)
	if err != nil {
		t.Fatalf("minted caveat failed validation: %v", err)
	}
	if claims.Session != got.Id {
		t.Errorf("caveat session = %q, want bound to returned id %q", claims.Session, got.Id)
	}
	for _, want := range auth.SandboxMCPCaps {
		if !claims.HasCap(want) {
			t.Errorf("caveat missing capability %q", want)
		}
	}
}

// TestCreateSession_NoCaveatWhenUnconfigured covers the unprotected
// daemon path: with no signing key, CreateSession omits caveat_token
// (no bearer is required on an unprotected /v1/* surface).
func TestCreateSession_NoCaveatWhenUnconfigured(t *testing.T) {
	s, _ := newSessionsServer(t) // caveatIssuer nil

	body := mustJSON(t, api.CreateSessionRequest{Agent: "claude"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.CreateSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got api.CreateSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CaveatToken != nil {
		t.Errorf("caveat_token = %v, want nil on unconfigured daemon", *got.CaveatToken)
	}
}

func TestCreateSession_MissingAgentReturns400(t *testing.T) {
	s, _ := newSessionsServer(t)

	body := mustJSON(t, api.CreateSessionRequest{}) // no agent
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.CreateSession(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestCreateSession_InvalidJSONReturns400(t *testing.T) {
	s, _ := newSessionsServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	s.CreateSession(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestCreateSession_StoreNotConfiguredReturns503(t *testing.T) {
	s := &apiServer{log: slog.Default()}
	body := mustJSON(t, api.CreateSessionRequest{Agent: "claude"})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.CreateSession(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestEndSession_StampsEndedAtAndExitCode(t *testing.T) {
	s, store := newSessionsServer(t)

	id := seedSession(t, store, "claude", "/x")
	exit := 0
	body := mustJSON(t, api.EndSessionRequest{ExitCode: &exit})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+id+"/end", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.EndSession(w, req, id)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got api.Session
	mustDecode(t, w.Body, &got)
	if got.EndedAt == nil {
		t.Fatal("EndedAt is nil after End")
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", got.ExitCode)
	}
}

func TestEndSession_NoExitCodeMarksOrphanedShape(t *testing.T) {
	s, store := newSessionsServer(t)

	id := seedSession(t, store, "claude", "/x")
	body := mustJSON(t, api.EndSessionRequest{}) // no exit_code
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+id+"/end", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.EndSession(w, req, id)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got api.Session
	mustDecode(t, w.Body, &got)
	if got.EndedAt == nil {
		t.Fatal("EndedAt should be set")
	}
	if got.ExitCode != nil {
		t.Errorf("ExitCode should be nil for omitted exit_code; got %v", *got.ExitCode)
	}
}

func TestEndSession_UnknownIdReturns404(t *testing.T) {
	s, _ := newSessionsServer(t)
	body := mustJSON(t, api.EndSessionRequest{})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/00000000000000000000000000/end", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.EndSession(w, req, "00000000000000000000000000")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestEndSession_InvalidJSONReturns400(t *testing.T) {
	s, _ := newSessionsServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/end", strings.NewReader("nope"))
	w := httptest.NewRecorder()
	s.EndSession(w, req, "x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestEndSession_StoreNotConfiguredReturns503(t *testing.T) {
	s := &apiServer{log: slog.Default()}
	body := mustJSON(t, api.EndSessionRequest{})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/x/end", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.EndSession(w, req, "x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestGetSession_HappyPath(t *testing.T) {
	s, store := newSessionsServer(t)
	id := seedSession(t, store, "claude", "/x")

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+id, nil)
	w := httptest.NewRecorder()
	s.GetSession(w, req, id)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got api.Session
	mustDecode(t, w.Body, &got)
	if got.Id != id {
		t.Errorf("Id = %q, want %q", got.Id, id)
	}
}

func TestGetSession_UnknownIdReturns404(t *testing.T) {
	s, _ := newSessionsServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/00000000000000000000000000", nil)
	w := httptest.NewRecorder()
	s.GetSession(w, req, "00000000000000000000000000")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestGetSession_StoreNotConfiguredReturns503(t *testing.T) {
	s := &apiServer{log: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/x", nil)
	w := httptest.NewRecorder()
	s.GetSession(w, req, "x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestListSessions_NewestFirstWithFilters(t *testing.T) {
	s, store := newSessionsServer(t)
	now := time.Now().UTC().Truncate(time.Second)

	mustPut(t, store, sessions.Session{ID: "01HK0000000000000000000010", StartedAt: now.Add(-2 * time.Hour), Agent: "claude", WorkingDir: "/x"})
	mustPut(t, store, sessions.Session{ID: "01HK0000000000000000000011", StartedAt: now.Add(-1 * time.Hour), Agent: "pi", WorkingDir: "/x"})
	mustPut(t, store, sessions.Session{ID: "01HK0000000000000000000012", StartedAt: now, Agent: "claude", WorkingDir: "/x"})

	since := now.Add(-90 * time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	w := httptest.NewRecorder()
	s.ListSessions(w, req, api.ListSessionsParams{
		Agent: ptr("claude"),
		Since: &since,
		Limit: ptr(10),
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got api.SessionListResponse
	mustDecode(t, w.Body, &got)
	if len(got.Items) != 1 || got.Items[0].Id != "01HK0000000000000000000012" {
		t.Fatalf("want only the newest claude session; got %+v", got.Items)
	}
}

func TestListSessions_ActiveOnly(t *testing.T) {
	s, store := newSessionsServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	exit := 0
	ended := now

	mustPut(t, store, sessions.Session{ID: "01HK0000000000000000000020", StartedAt: now, Agent: "claude", WorkingDir: "/x"})
	mustPut(t, store, sessions.Session{ID: "01HK0000000000000000000021", StartedAt: now.Add(-time.Hour), EndedAt: &ended, Agent: "claude", WorkingDir: "/x", ExitCode: &exit})

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions?active_only=true", nil)
	w := httptest.NewRecorder()
	s.ListSessions(w, req, api.ListSessionsParams{ActiveOnly: ptr(true)})

	var got api.SessionListResponse
	mustDecode(t, w.Body, &got)
	if len(got.Items) != 1 || got.Items[0].Id != "01HK0000000000000000000020" {
		t.Fatalf("want only the running session; got %+v", got.Items)
	}
}

func TestListSessions_StoreNotConfiguredReturns503(t *testing.T) {
	s := &apiServer{log: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	w := httptest.NewRecorder()
	s.ListSessions(w, req, api.ListSessionsParams{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// --- helpers ---

func seedSession(t *testing.T, store sessions.Store, agent, dir string) string {
	t.Helper()
	id := sessions.NewID()
	if err := store.Put(context.Background(), sessions.Session{
		ID: id, StartedAt: time.Now().UTC(), Agent: agent, WorkingDir: dir,
	}); err != nil {
		t.Fatalf("seedSession: %v", err)
	}
	return id
}

func mustPut(t *testing.T, store sessions.Store, sess sessions.Session) {
	t.Helper()
	if err := store.Put(context.Background(), sess); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}
