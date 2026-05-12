package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
)

// withStateStore augments a baseline test server with a writable overlay
// store rooted in a fresh tempdir so each test has a clean slate.
func withStateStore(t *testing.T, srv *apiServer) *apiServer {
	t.Helper()
	store, err := action.NewFileStateStore(filepath.Join(t.TempDir(), "action-state.json"))
	if err != nil {
		t.Fatalf("NewFileStateStore: %v", err)
	}
	srv.actionState = store
	return srv
}

func boolPtr(b bool) *bool { return &b }

func TestListActions_DefaultsEnabledTrueWithoutOverlay(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
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
		t.Fatalf("Items = %v, want 1", got.Items)
	}
	a := (*got.Items)[0]
	if a.Enabled == nil || !*a.Enabled {
		t.Errorf("Enabled = %v, want true (default when no overlay)", a.Enabled)
	}
}

func TestListActions_ReflectsDisabledOverlay(t *testing.T) {
	srv := withStateStore(t, newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	}))
	if err := srv.actionState.Set("ship-update", action.State{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/actions", nil)
	rec := httptest.NewRecorder()
	srv.ListActions(rec, req)

	var got api.ActionListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Items == nil || len(*got.Items) != 1 {
		t.Fatalf("Items count = %v, want 1", got.Items)
	}
	a := (*got.Items)[0]
	if a.Enabled == nil || *a.Enabled {
		t.Errorf("Enabled = %v, want false after Set(enabled=false)", a.Enabled)
	}
}

func TestPatchAction_DisablesAndPersists(t *testing.T) {
	srv := withStateStore(t, newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	}))

	body := bytes.NewReader([]byte(`{"enabled":false}`))
	req := httptest.NewRequest(http.MethodPatch, "/v1/actions/ship-update", body)
	rec := httptest.NewRecorder()
	srv.PatchAction(rec, req, "ship-update")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.Action
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled == nil || *got.Enabled {
		t.Errorf("response Enabled = %v, want false", got.Enabled)
	}

	// Overlay persistence: a follow-up GET sees the same state.
	if srv.actionState.Get("ship-update").IsEnabled() {
		t.Errorf("overlay store still reports enabled=true after PATCH")
	}
}

func TestPatchAction_Reenable(t *testing.T) {
	srv := withStateStore(t, newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	}))
	if err := srv.actionState.Set("ship-update", action.State{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	body := bytes.NewReader([]byte(`{"enabled":true}`))
	req := httptest.NewRequest(http.MethodPatch, "/v1/actions/ship-update", body)
	rec := httptest.NewRecorder()
	srv.PatchAction(rec, req, "ship-update")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !srv.actionState.Get("ship-update").IsEnabled() {
		t.Errorf("after PATCH enabled=true, overlay still reports disabled")
	}
}

func TestPatchAction_OmittedFieldsLeaveOverlayUnchanged(t *testing.T) {
	srv := withStateStore(t, newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	}))
	if err := srv.actionState.Set("ship-update", action.State{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Empty body PATCH — every field omitted. Per the spec, omitted
	// fields preserve the current overlay value.
	req := httptest.NewRequest(http.MethodPatch, "/v1/actions/ship-update", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	srv.PatchAction(rec, req, "ship-update")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if srv.actionState.Get("ship-update").IsEnabled() {
		t.Errorf("empty PATCH flipped the overlay; want disabled preserved")
	}
}

func TestPatchAction_UnknownActionReturns404(t *testing.T) {
	srv := withStateStore(t, newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	}))

	req := httptest.NewRequest(http.MethodPatch, "/v1/actions/no-such-action",
		bytes.NewReader([]byte(`{"enabled":false}`)))
	rec := httptest.NewRecorder()
	srv.PatchAction(rec, req, "no-such-action")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPatchAction_MalformedBodyReturns400(t *testing.T) {
	srv := withStateStore(t, newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	}))

	req := httptest.NewRequest(http.MethodPatch, "/v1/actions/ship-update",
		bytes.NewReader([]byte(`not-json`)))
	rec := httptest.NewRecorder()
	srv.PatchAction(rec, req, "ship-update")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestPatchAction_NilStoreReturns500(t *testing.T) {
	// Daemon couldn't load the overlay at startup — surface the
	// constraint rather than silently dropping the toggle.
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	// actionState left nil intentionally.

	req := httptest.NewRequest(http.MethodPatch, "/v1/actions/ship-update",
		bytes.NewReader([]byte(`{"enabled":false}`)))
	rec := httptest.NewRecorder()
	srv.PatchAction(rec, req, "ship-update")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when actionState is nil", rec.Code)
	}
}

func TestPatchAction_NilActionsStoreReturns404(t *testing.T) {
	// The daemon has no actions store at all (e.g. tests, very early
	// boot). PatchAction has nothing to patch — return the same 404 a
	// missing action would.
	srv := &apiServer{}
	req := httptest.NewRequest(http.MethodPatch, "/v1/actions/x",
		bytes.NewReader([]byte(`{"enabled":false}`)))
	rec := httptest.NewRecorder()
	srv.PatchAction(rec, req, "x")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when actions store is nil", rec.Code)
	}
}

func TestRunAction_DisabledReturnsConflict(t *testing.T) {
	srv := withStateStore(t, newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	}))
	if err := srv.actionState.Set("ship-update", action.State{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	body := bytes.NewReader([]byte(`{"args":{"channel":"#engineering"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 conflict for disabled action; body=%s", rec.Code, rec.Body.String())
	}
}
