package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
	api "github.com/ALRubinger/aileron/internal/api/gen"
)

// provenanceExecutor returns a fixed Result whose Provenance the test sets, so
// the RunAction 200 body can be asserted against the exact actor provenance the
// executor surfaced (issue #1753).
type provenanceExecutor struct {
	prov action.ActorProvenance
}

func (p provenanceExecutor) Execute(_ context.Context, _ string, _ map[string]any) (action.Result, error) {
	return action.Result{Content: `{"ok":true}`, Provenance: p.prov}, nil
}

// TestRunAction_SurfacesActorProvenance asserts the synchronous 200 response
// carries the connector build and non-secret identity/binding the executor
// resolved, plus consent_decision=unattended (issue #1753). This is the actor
// half of the output.materialized walk-back.
func TestRunAction_SurfacesActorProvenance(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.executor = provenanceExecutor{prov: action.ActorProvenance{
		ConnectorVersion:  "2.3.1",
		ConnectorHash:     "sha256:abc123",
		IdentityLabel:     "work",
		CredentialBinding: "api_key/slack/work",
	}}

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
	if got.ConnectorVersion == nil || *got.ConnectorVersion != "2.3.1" {
		t.Errorf("connector_version = %v, want 2.3.1", got.ConnectorVersion)
	}
	if got.ConnectorHash == nil || *got.ConnectorHash != "sha256:abc123" {
		t.Errorf("connector_hash = %v, want sha256:abc123", got.ConnectorHash)
	}
	if got.IdentityLabel == nil || *got.IdentityLabel != "work" {
		t.Errorf("identity_label = %v, want work", got.IdentityLabel)
	}
	if got.CredentialBinding == nil || *got.CredentialBinding != "api_key/slack/work" {
		t.Errorf("credential_binding = %v, want api_key/slack/work", got.CredentialBinding)
	}
	if got.ConsentDecision == nil || *got.ConsentDecision != "unattended" {
		t.Errorf("consent_decision = %v, want unattended (synchronous 200 path)", got.ConsentDecision)
	}
}

// TestRunAction_OmitsIdentityForCredentiallessAction asserts that when the
// executor resolved no credential binding, the 200 body omits identity_label
// and credential_binding entirely (an honest omission, not an empty string)
// while still carrying the connector build and consent posture.
func TestRunAction_OmitsIdentityForCredentiallessAction(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"ship-update.md": actionsTestManifest,
	})
	srv.executor = provenanceExecutor{prov: action.ActorProvenance{
		ConnectorVersion: "1.4.0",
		ConnectorHash:    "sha256:def456",
		// IdentityLabel and CredentialBinding intentionally empty.
	}}

	body := bytes.NewReader([]byte(`{"args":{}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/ship-update/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "ship-update")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Assert on the raw JSON so an omitted key is provably absent, not merely
	// decoded to a nil pointer.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, present := raw["identity_label"]; present {
		t.Error("identity_label must be omitted for a credential-less action")
	}
	if _, present := raw["credential_binding"]; present {
		t.Error("credential_binding must be omitted for a credential-less action")
	}
	if raw["connector_version"] != "1.4.0" {
		t.Errorf("connector_version = %v, want 1.4.0", raw["connector_version"])
	}
	if raw["consent_decision"] != "unattended" {
		t.Errorf("consent_decision = %v, want unattended", raw["consent_decision"])
	}
}
