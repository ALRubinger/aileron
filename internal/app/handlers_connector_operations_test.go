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
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
)

func newConnectorOperationTestServer(specs []connectorspec.Spec) (*apiServer, *audit.MemStore) {
	store := audit.NewMemStore()
	return &apiServer{
		log:           slog.New(slog.NewJSONHandler(io.Discard, nil)),
		auditStore:    store,
		auditRecorder: audit.NewRecorder(store, nil, func() string { return "audit-connector-operation" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			return specs, nil
		},
	}, store
}

func TestRunConnectorOperation_RecognizedOperationAuditsAndFailsClosed(t *testing.T) {
	const connectorFQN = "github://acme/aileron-connector-google"
	srv, auditStore := newConnectorOperationTestServer([]connectorspec.Spec{
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: connectorFQN, Version: "1.0.0"},
			Tools: []connectorspec.Tool{
				{
					Name: "Google",
					Operations: []connectorspec.Operation{
						{
							Name:        "gmail.messages.search",
							Method:      "GET",
							Path:        "/gmail/v1/users/me/messages",
							Idempotency: "idempotent",
							Credential:  "oauth2",
						},
					},
				},
			},
		},
	})

	body := []byte(`{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.search","args":{"q":"from:alice@example.com"}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/connector-operations/run", bytes.NewReader(body))
	req.Header.Set("X-Aileron-Session-Id", "session-123")
	rec := httptest.NewRecorder()

	srv.RunConnectorOperation(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	var resp api.ConnectorOperationRunRejectedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Status != api.Rejected {
		t.Errorf("status = %q, want rejected", resp.Status)
	}
	if resp.AuditId != "audit-connector-operation" {
		t.Errorf("audit_id = %q", resp.AuditId)
	}
	if resp.Tool != "google" || resp.Operation != "gmail.messages.search" || resp.ConnectorFqn != connectorFQN {
		t.Errorf("response identity = %+v", resp)
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.EventType != model.EventTypeConnectorOperationRejected {
		t.Fatalf("event type = %q", event.EventType)
	}
	payload := event.Payload
	if payload["aileron.connector.fqn"] != connectorFQN {
		t.Errorf("connector fqn payload = %v", payload["aileron.connector.fqn"])
	}
	if payload["aileron.connector.tool"] != "google" {
		t.Errorf("tool payload = %v", payload["aileron.connector.tool"])
	}
	if payload["aileron.connector.operation"] != "gmail.messages.search" {
		t.Errorf("operation payload = %v", payload["aileron.connector.operation"])
	}
	if payload["aileron.connector.credential"] != "oauth2" {
		t.Errorf("credential kind payload = %v", payload["aileron.connector.credential"])
	}
	if payload["aileron.connector.reject_reason"] != "data_plane_not_implemented" {
		t.Errorf("reject reason payload = %v", payload["aileron.connector.reject_reason"])
	}
	if payload["aileron.session.id"] != "session-123" {
		t.Errorf("session payload = %v", payload["aileron.session.id"])
	}
}

func TestRunConnectorOperation_UnknownOperationReturns404WithoutAudit(t *testing.T) {
	srv, auditStore := newConnectorOperationTestServer([]connectorspec.Spec{
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-google"},
			Tools: []connectorspec.Tool{
				{Name: "google", Operations: []connectorspec.Operation{{Name: "gmail.messages.search"}}},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/connector-operations/run", bytes.NewReader([]byte(`{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.delete"}`)))
	rec := httptest.NewRecorder()

	srv.RunConnectorOperation(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0", len(events))
	}
}

func TestRunConnectorOperation_RequiresIdentityFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "connector fqn", body: `{"tool":"google","operation":"gmail.messages.search"}`},
		{name: "tool", body: `{"connector_fqn":"github://acme/aileron-connector-google","operation":"gmail.messages.search"}`},
		{name: "operation", body: `{"connector_fqn":"github://acme/aileron-connector-google","tool":"google"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newConnectorOperationTestServer(nil)
			req := httptest.NewRequest(http.MethodPost, "/v1/connector-operations/run", bytes.NewReader([]byte(tt.body)))
			rec := httptest.NewRecorder()

			srv.RunConnectorOperation(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRunConnectorOperation_RejectsInvalidJSON(t *testing.T) {
	srv, _ := newConnectorOperationTestServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/connector-operations/run", bytes.NewReader([]byte(`{`)))
	rec := httptest.NewRecorder()

	srv.RunConnectorOperation(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunConnectorOperation_SpecLoaderErrorReturns500(t *testing.T) {
	srv, _ := newConnectorOperationTestServer(nil)
	srv.specLoader = func() ([]connectorspec.Spec, error) {
		return nil, errors.New("spec store unavailable")
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/connector-operations/run", bytes.NewReader([]byte(`{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.search"}`)))
	rec := httptest.NewRecorder()

	srv.RunConnectorOperation(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunConnectorOperation_SpecConflictReturns500(t *testing.T) {
	srv, _ := newConnectorOperationTestServer([]connectorspec.Spec{
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: "github://acme/one"},
			Tools:         []connectorspec.Tool{{Name: "google", Operations: []connectorspec.Operation{{Name: "one.search"}}}},
		},
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: "github://acme/two"},
			Tools:         []connectorspec.Tool{{Name: "Google", Operations: []connectorspec.Operation{{Name: "two.search"}}}},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/connector-operations/run", bytes.NewReader([]byte(`{"connector_fqn":"github://acme/one","tool":"google","operation":"one.search"}`)))
	rec := httptest.NewRecorder()

	srv.RunConnectorOperation(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunConnectorOperation_NoRecorderReturnsFallbackAuditID(t *testing.T) {
	const connectorFQN = "github://acme/aileron-connector-google"
	srv, _ := newConnectorOperationTestServer([]connectorspec.Spec{
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: connectorFQN},
			Tools:         []connectorspec.Tool{{Name: "google", Operations: []connectorspec.Operation{{Name: "gmail.messages.search"}}}},
		},
	})
	srv.auditRecorder = nil
	srv.newID = func() string { return "audit-fallback" }
	req := httptest.NewRequest(http.MethodPost, "/v1/connector-operations/run", bytes.NewReader([]byte(`{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.search"}`)))
	rec := httptest.NewRecorder()

	srv.RunConnectorOperation(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	var resp api.ConnectorOperationRunRejectedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.AuditId != "audit-fallback" {
		t.Fatalf("audit_id = %q, want fallback", resp.AuditId)
	}
}
