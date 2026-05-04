package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/failure"
)

const actionsTestManifest = `+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc123"
capabilities = ["chat:write", "channels:read"]

[match]
intent = "tell team I shipped"

[[execute]]
id = "post"
connector = "github://aileron/slack"
op = "post_message"
+++

# Ship Update

Posts a "shipped" announcement to a Slack channel.
`

const actionsTestBadManifest = `+++
name = "broken"
version = "x.y"
+++
`

func newActionsTestServer(t *testing.T, files map[string]string) *apiServer {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	store := action.NewStore(dir)
	if _, err := store.Load(); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	return &apiServer{
		log:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		actions:  store,
		executor: action.StubExecutor{},
		newID:    func() string { return "audit_test_id" },
	}
}

func TestListActions_ReturnsInstalledManifests(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/actions", nil)
	rec := httptest.NewRecorder()
	srv.ListActions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.ActionListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Items == nil || len(*got.Items) != 1 {
		t.Fatalf("Items = %v, want 1", got.Items)
	}
	a := (*got.Items)[0]
	if a.Name != "ship-update" {
		t.Errorf("Name = %q", a.Name)
	}
	if a.Match.Intent != "tell team I shipped" {
		t.Errorf("Match.Intent = %q", a.Match.Intent)
	}
	if a.Requires.Connectors == nil || len(*a.Requires.Connectors) != 1 {
		t.Fatalf("Requires.Connectors = %v", a.Requires.Connectors)
	}
	if a.Body == nil || (*a.Body == "") {
		t.Errorf("Body should be populated")
	}
	if got.LoadErrors != nil && len(*got.LoadErrors) != 0 {
		t.Errorf("LoadErrors = %v, want empty", got.LoadErrors)
	}
}

func TestListActions_SurfacesLoadErrors(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"good.md": actionsTestManifest,
		"bad.md":  actionsTestBadManifest,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/actions", nil)
	rec := httptest.NewRecorder()
	srv.ListActions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got api.ActionListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Items == nil || len(*got.Items) != 1 {
		t.Errorf("Items count = %v, want 1 (the valid manifest)", got.Items)
	}
	if got.LoadErrors == nil || len(*got.LoadErrors) != 1 {
		t.Fatalf("LoadErrors = %v, want one entry", got.LoadErrors)
	}
	le := (*got.LoadErrors)[0]
	if le.File == "" {
		t.Errorf("LoadError.File is empty")
	}
	if le.Class == "" {
		t.Errorf("LoadError.Class is empty")
	}
}

func TestListActions_NilStoreReturnsEmpty(t *testing.T) {
	srv := &apiServer{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodGet, "/v1/actions", nil)
	rec := httptest.NewRecorder()
	srv.ListActions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got api.ActionListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Items != nil && len(*got.Items) != 0 {
		t.Errorf("Items = %v, want nil/empty", got.Items)
	}
}

func TestGetAction_ReturnsManifest(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/actions/ship-update", nil)
	rec := httptest.NewRecorder()
	srv.GetAction(rec, req, "ship-update")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.Action
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "ship-update" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Body == nil {
		t.Fatal("Body is nil")
	}
	if got.Path == nil || *got.Path == "" {
		t.Errorf("Path is empty")
	}
	if len(got.Execute) != 1 {
		t.Errorf("Execute = %v, want 1 step", got.Execute)
	}
}

func TestGetAction_NotFound(t *testing.T) {
	srv := newActionsTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/actions/missing", nil)
	rec := httptest.NewRecorder()
	srv.GetAction(rec, req, "missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetAction_NilStoreReturns404(t *testing.T) {
	srv := &apiServer{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodGet, "/v1/actions/x", nil)
	rec := httptest.NewRecorder()
	srv.GetAction(rec, req, "x")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestLoadErrorToAPI_PopulatesBoundaryAndLine(t *testing.T) {
	got := loadErrorToAPI(&action.Error{
		Class:    action.ClassParseError,
		Message:  "boom",
		File:     "x.md",
		Line:     42,
		Boundary: action.BoundaryAction,
	})
	if got.Line == nil || *got.Line != 42 {
		t.Errorf("Line = %v, want *42", got.Line)
	}
	if got.Boundary == nil || *got.Boundary != string(action.BoundaryAction) {
		t.Errorf("Boundary = %v, want *action", got.Boundary)
	}
}

func TestLoadErrorToAPI_OmitsBlankBoundaryAndZeroLine(t *testing.T) {
	got := loadErrorToAPI(&action.Error{
		Class:   action.ClassParseError,
		Message: "boom",
		File:    "x.md",
		// Boundary intentionally empty; Line zero.
	})
	if got.Boundary != nil {
		t.Errorf("Boundary = %v, want nil when blank", got.Boundary)
	}
	if got.Line != nil {
		t.Errorf("Line = %v, want nil when zero", got.Line)
	}
	if got.Class != string(action.ClassParseError) {
		t.Errorf("Class = %q", got.Class)
	}
}

func TestManifestToAPI_OmitsBlankBodyAndPath(t *testing.T) {
	la := action.LoadedAction{
		Manifest: &action.Manifest{
			Name:    "x",
			Version: "1.0.0",
			Source:  "hub://aileron/x@1.0.0",
			Match:   action.Match{Intent: "hi"},
			Requires: action.Requires{Connectors: []action.RequiresConnector{
				{Name: "github://aileron/slack", Version: "1.0.0", Hash: "sha256:a", Capabilities: []string{"chat:write"}},
			}},
			Execute: []action.ExecuteStep{{ID: "s", Connector: "github://aileron/slack", Op: "post"}},
			// Body intentionally blank.
		},
		// Path intentionally blank.
	}
	got := manifestToAPI(la)
	if got.Body != nil {
		t.Errorf("Body = %v, want nil when blank", got.Body)
	}
	if got.Path != nil {
		t.Errorf("Path = %v, want nil when blank", got.Path)
	}
}

func TestRunAction_HappyPath(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})

	body := bytes.NewReader([]byte(`{"args":{"channel":"#engineering"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.ActionRunResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AuditId == "" {
		t.Error("expected non-empty audit_id")
	}
	if got.Result == nil || *got.Result == "" {
		t.Error("expected non-empty result content from stub executor")
	}
}

func TestRunAction_NotFound(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/missing/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunAction_VaultLocked(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.vaultLocked = true

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunAction_InvalidJSON(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})

	body := bytes.NewReader([]byte(`{not-json`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// failingExecutor is a test stub that returns a structured ADR-0010
// failure for the action-side error path (not a Go error).
type failingExecutor struct{ failure *failure.Failure }

func (f failingExecutor) Execute(_ context.Context, _ string, _ map[string]any) (action.Result, error) {
	return action.Result{Failure: f.failure}, nil
}

// erroringExecutor is a test stub that returns a Go error to exercise
// the gateway-fatal branch in RunAction.
type erroringExecutor struct{}

func (erroringExecutor) Execute(_ context.Context, _ string, _ map[string]any) (action.Result, error) {
	return action.Result{}, errors.New("executor blew up")
}

func TestRunAction_ActionSideFailure(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.executor = failingExecutor{failure: failure.BindingRequiredFailure(
		"slack binding missing",
	)}

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	// ADR-0010: binding_required maps to 412 Precondition Failed.
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"binding_required"`)) {
		t.Errorf("expected envelope class in body; got %s", rec.Body.String())
	}
}

func TestRunAction_ExecutorError(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.executor = erroringExecutor{}

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunAction_NilExecutor(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.executor = nil

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
