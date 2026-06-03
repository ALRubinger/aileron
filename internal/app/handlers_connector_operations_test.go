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
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/sandbox/discovery"
	"github.com/ALRubinger/aileron/internal/vault"
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

type sandboxProxyRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn sandboxProxyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type sandboxProxyTestResolver struct {
	cred credential.Credential
	err  error
}

func (r sandboxProxyTestResolver) Resolve(context.Context) (credential.Credential, error) {
	return r.cred, r.err
}

type sandboxProxyTestBindingStore struct {
	resolver credential.Resolver
}

func (s sandboxProxyTestBindingStore) List(context.Context) ([]binding.Binding, error) {
	return nil, nil
}

func (s sandboxProxyTestBindingStore) Get(context.Context, binding.Name) (binding.Binding, error) {
	return binding.Binding{}, binding.ErrNotFound
}

func (s sandboxProxyTestBindingStore) Put(context.Context, binding.Binding, []byte, binding.PutMode) error {
	return nil
}

func (s sandboxProxyTestBindingStore) Delete(context.Context, binding.Name) error {
	return nil
}

func (s sandboxProxyTestBindingStore) UpdateMetadata(context.Context, binding.Binding) error {
	return nil
}

func (s sandboxProxyTestBindingStore) Resolve(context.Context, string, string) (binding.Binding, error) {
	return binding.Binding{}, binding.ErrNotFound
}

func (s sandboxProxyTestBindingStore) ResolverFor(context.Context, string, string) credential.Resolver {
	return s.resolver
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
	if resp.Status != api.ConnectorOperationRunRejectedResponseStatusRejected {
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

func TestRunConnectorOperation_BodylessGETProxiesThroughSandboxDataPlane(t *testing.T) {
	const connectorFQN = "github://acme/aileron-connector-google"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("upstream method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/gmail/v1/users/me/messages" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		if got := r.URL.Query()["labelIds"]; len(got) != 2 || got[0] != "INBOX" || got[1] != "UNREAD" {
			t.Errorf("labelIds query = %#v", got)
		}
		if got := r.URL.Query().Get("includeSpamTrash"); got != "true" {
			t.Errorf("includeSpamTrash query = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gmail_secret" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[]}`))
	}))
	defer upstream.Close()

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
							Hosts:       []string{strings.TrimPrefix(upstream.URL, "https://")},
							Idempotency: "idempotent",
							Credential:  "oauth2",
							Inputs: []connectorspec.Input{
								{Name: "labelIds", Type: "array"},
								{Name: "includeSpamTrash", Type: "boolean"},
							},
						},
					},
				},
			},
		},
	})
	srv.sandboxProxyClient = upstream.Client()
	srv.bindings = sandboxProxyTestBindingStore{resolver: sandboxProxyTestResolver{
		cred: credential.Credential{Kind: "oauth2", Value: []byte("gmail_secret")},
	}}

	body := []byte(`{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.search","args":{"labelIds":["INBOX","UNREAD"],"includeSpamTrash":true}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/connector-operations/run", bytes.NewReader(body))
	req.Header.Set("X-Aileron-Session-Id", "session-123")
	rec := httptest.NewRecorder()

	srv.RunConnectorOperation(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp api.SandboxProxyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Status != api.Proxied {
		t.Errorf("status = %q, want proxied", resp.Status)
	}
	if resp.UpstreamStatus != http.StatusOK {
		t.Errorf("upstream status = %d, want 200", resp.UpstreamStatus)
	}
	if string(resp.BodyBase64) != `{"messages":[]}` {
		t.Errorf("body = %s", resp.BodyBase64)
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.EventType != model.EventTypeConnectorProxyProxied {
		t.Fatalf("event type = %q, want %q", event.EventType, model.EventTypeConnectorProxyProxied)
	}
	payload := event.Payload
	if payload["aileron.connector.boundary"] != "https_proxy" {
		t.Errorf("boundary payload = %v", payload["aileron.connector.boundary"])
	}
	if payload["aileron.session.id"] != "session-123" {
		t.Errorf("session payload = %v", payload["aileron.session.id"])
	}
	payloadJSON, _ := json.Marshal(payload)
	if strings.Contains(string(payloadJSON), "gmail_secret") || strings.Contains(string(payloadJSON), "INBOX") {
		t.Fatalf("audit payload leaked credential or query args: %s", payloadJSON)
	}
}

func TestRunConnectorOperation_PostWithArgsStillFailsClosed(t *testing.T) {
	const connectorFQN = "github://acme/aileron-connector-google"
	srv, auditStore := newConnectorOperationTestServer([]connectorspec.Spec{
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: connectorFQN},
			Tools: []connectorspec.Tool{
				{Name: "google", Operations: []connectorspec.Operation{{
					Name: "gmail.messages.send", Method: "POST", Path: "/gmail/v1/users/me/messages/send", Hosts: []string{"gmail.googleapis.com"}, Credential: "oauth2",
				}}},
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/connector-operations/run", bytes.NewReader([]byte(`{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.send","args":{"to":"alice@example.com"}}`)))
	rec := httptest.NewRecorder()

	srv.RunConnectorOperation(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	var resp api.ConnectorOperationRunRejectedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Status != api.ConnectorOperationRunRejectedResponseStatusRejected {
		t.Errorf("status = %q, want rejected", resp.Status)
	}
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeConnectorOperationRejected {
		t.Fatalf("event type = %q, want %q", events[0].EventType, model.EventTypeConnectorOperationRejected)
	}
}

func TestConnectorOperationRunUpstreamURL(t *testing.T) {
	tests := []struct {
		name      string
		operation connectorspec.Operation
		args      *map[string]any
		wantURL   string
		wantProxy bool
		wantErr   string
	}{
		{
			name: "delete query args",
			operation: connectorspec.Operation{
				Name: "messages.delete", Method: "DELETE", Path: "/gmail/v1/users/me/messages", Hosts: []string{"gmail.googleapis.com"},
			},
			args:      &map[string]any{"id": "msg-123", "dryRun": false, "attempt": float64(2), "empty": nil},
			wantURL:   "https://gmail.googleapis.com/gmail/v1/users/me/messages?attempt=2&dryRun=false&empty=&id=msg-123",
			wantProxy: true,
		},
		{
			name: "head without args",
			operation: connectorspec.Operation{
				Name: "messages.head", Method: "HEAD", Path: "/gmail/v1/users/me/messages", Hosts: []string{"gmail.googleapis.com"},
			},
			wantURL:   "https://gmail.googleapis.com/gmail/v1/users/me/messages",
			wantProxy: true,
		},
		{
			name: "missing method",
			operation: connectorspec.Operation{
				Name: "messages.search", Path: "/gmail/v1/users/me/messages", Hosts: []string{"gmail.googleapis.com"},
			},
			wantProxy: false,
		},
		{
			name: "missing hosts",
			operation: connectorspec.Operation{
				Name: "messages.search", Method: "GET", Path: "/gmail/v1/users/me/messages",
			},
			wantProxy: false,
		},
		{
			name: "relative path",
			operation: connectorspec.Operation{
				Name: "messages.search", Method: "GET", Path: "gmail/v1/users/me/messages", Hosts: []string{"gmail.googleapis.com"},
			},
			wantProxy: false,
		},
		{
			name: "post not eligible",
			operation: connectorspec.Operation{
				Name: "messages.send", Method: "POST", Path: "/gmail/v1/users/me/messages/send", Hosts: []string{"gmail.googleapis.com"},
			},
			args:      &map[string]any{"to": "alice@example.com"},
			wantProxy: false,
		},
		{
			name: "invalid host",
			operation: connectorspec.Operation{
				Name: "messages.search", Method: "GET", Path: "/gmail/v1/users/me/messages", Hosts: []string{"bad host"},
			},
			wantProxy: false,
		},
		{
			name: "empty arg key",
			operation: connectorspec.Operation{
				Name: "messages.search", Method: "GET", Path: "/gmail/v1/users/me/messages", Hosts: []string{"gmail.googleapis.com"},
			},
			args:    &map[string]any{" ": "value"},
			wantErr: "args contains an empty query parameter name",
		},
		{
			name: "object arg rejected",
			operation: connectorspec.Operation{
				Name: "messages.search", Method: "GET", Path: "/gmail/v1/users/me/messages", Hosts: []string{"gmail.googleapis.com"},
			},
			args:    &map[string]any{"filter": map[string]any{"q": "secret"}},
			wantErr: "args.filter must be a string, number, boolean, null, or array of those values for bodyless proxy execution",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := api.ConnectorOperationRunRequest{Args: tt.args}
			upstream, canProxy, err := connectorOperationRunUpstreamURL(req, discovery.SpecOperationHelp{
				Name:       tt.operation.Name,
				Method:     tt.operation.Method,
				Path:       tt.operation.Path,
				Hosts:      tt.operation.Hosts,
				Credential: tt.operation.Credential,
			})
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("connectorOperationRunUpstreamURL error: %v", err)
			}
			if canProxy != tt.wantProxy {
				t.Fatalf("canProxy = %v, want %v", canProxy, tt.wantProxy)
			}
			if !tt.wantProxy {
				if upstream != nil {
					t.Fatalf("upstream = %s, want nil", upstream)
				}
				return
			}
			if upstream.String() != tt.wantURL {
				t.Fatalf("upstream = %s, want %s", upstream, tt.wantURL)
			}
		})
	}
}

func TestRecordSandboxProxyRequest_RecognizedHTTPSAttemptInjectsCredentialAndProxies(t *testing.T) {
	const connectorFQN = "github://acme/aileron-connector-google"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("upstream method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/gmail/v1/users/me/messages" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("q") != "secret" {
			t.Errorf("upstream query q = %q", r.URL.Query().Get("q"))
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ghp_proxy_secret" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

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
							Hosts:       []string{strings.TrimPrefix(upstream.URL, "https://")},
							Idempotency: "idempotent",
							Credential:  "oauth2",
						},
					},
				},
			},
		},
	})
	srv.sandboxProxyClient = upstream.Client()
	bindings := &binding.VaultStore{Vault: vault.NewMemVault()}
	name, err := binding.MakeName("oauth2", "google", "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := bindings.Put(context.Background(), binding.Binding{
		Name:         name,
		Kind:         "oauth2",
		Service:      "google",
		Identity:     "work",
		ConnectorFQN: connectorFQN,
	}, []byte(`{"access_token":"ghp_proxy_secret"}`), binding.PutCreate); err != nil {
		t.Fatalf("binding put: %v", err)
	}
	srv.bindings = bindings

	body := []byte(`{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.search","method":"GET","upstream_url":"` + upstream.URL + `/gmail/v1/users/me/messages?q=secret"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/requests", bytes.NewReader(body))
	req.Header.Set("X-Aileron-Session-Id", "session-123")
	rec := httptest.NewRecorder()

	srv.RecordSandboxProxyRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp api.SandboxProxyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Status != api.Proxied {
		t.Errorf("status = %q, want proxied", resp.Status)
	}
	if resp.AuditId != "audit-connector-operation" {
		t.Errorf("audit_id = %q", resp.AuditId)
	}
	if resp.Tool != "google" || resp.Operation != "gmail.messages.search" || resp.ConnectorFqn != connectorFQN {
		t.Errorf("response identity = %+v", resp)
	}
	if resp.UpstreamStatus != http.StatusOK {
		t.Errorf("upstream status = %d", resp.UpstreamStatus)
	}
	if string(resp.BodyBase64) != `{"ok":true}` {
		t.Errorf("body = %s", resp.BodyBase64)
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.EventType != model.EventTypeConnectorProxyProxied {
		t.Fatalf("event type = %q", event.EventType)
	}
	payload := event.Payload
	if payload["aileron.connector.boundary"] != "https_proxy" {
		t.Errorf("boundary payload = %v", payload["aileron.connector.boundary"])
	}
	if payload["aileron.connector.mediation"] != "https_proxy" {
		t.Errorf("mediation payload = %v", payload["aileron.connector.mediation"])
	}
	if payload["aileron.connector.decision"] != "proxied" {
		t.Errorf("decision payload = %v", payload["aileron.connector.decision"])
	}
	if payload["aileron.proxy.upstream.host"] != strings.TrimPrefix(upstream.URL, "https://") {
		t.Errorf("host payload = %v", payload["aileron.proxy.upstream.host"])
	}
	if payload["aileron.proxy.upstream.path"] != "/gmail/v1/users/me/messages" {
		t.Errorf("path payload = %v", payload["aileron.proxy.upstream.path"])
	}
	if payload["aileron.proxy.upstream.status"] != float64(http.StatusOK) && payload["aileron.proxy.upstream.status"] != http.StatusOK {
		t.Errorf("status payload = %v", payload["aileron.proxy.upstream.status"])
	}
	if _, ok := payload["aileron.proxy.upstream.query"]; ok {
		t.Errorf("query should not be audited: %v", payload["aileron.proxy.upstream.query"])
	}
	payloadJSON, _ := json.Marshal(payload)
	if strings.Contains(string(payloadJSON), "ghp_proxy_secret") {
		t.Fatalf("audit payload leaked credential: %s", payloadJSON)
	}
	if payload["aileron.session.id"] != "session-123" {
		t.Errorf("session payload = %v", payload["aileron.session.id"])
	}
}

func TestRecordSandboxProxyRequest_CredentialOperationWithoutBindingFailsBeforeUpstream(t *testing.T) {
	const connectorFQN = "github://acme/aileron-connector-google"
	upstreamCalled := false
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

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
							Hosts:       []string{strings.TrimPrefix(upstream.URL, "https://")},
							Idempotency: "idempotent",
							Credential:  "oauth2",
						},
					},
				},
			},
		},
	})
	srv.sandboxProxyClient = upstream.Client()

	body := []byte(`{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.search","method":"GET","upstream_url":"` + upstream.URL + `/gmail/v1/users/me/messages"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/requests", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	srv.RecordSandboxProxyRequest(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Fatal("upstream was called before credential binding was available")
	}
	var resp api.SandboxProxyRejectedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.Status != api.SandboxProxyRejectedResponseStatusRejected {
		t.Errorf("status = %q, want rejected", resp.Status)
	}
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	payload := events[0].Payload
	if payload["aileron.connector.reject_reason"] != "binding_store_unavailable" {
		t.Errorf("reject reason payload = %v", payload["aileron.connector.reject_reason"])
	}
}

func TestRecordSandboxProxyRequest_APIKeyCredentialInjectsBearerToken(t *testing.T) {
	const connectorFQN = "github://acme/aileron-connector-linear"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer lin_secret" {
			t.Errorf("authorization = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	srv, _ := newConnectorOperationTestServer([]connectorspec.Spec{
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: connectorFQN},
			Tools: []connectorspec.Tool{
				{Name: "linear", Operations: []connectorspec.Operation{{
					Name: "issues.list", Method: "GET", Path: "/graphql", Hosts: []string{strings.TrimPrefix(upstream.URL, "https://")}, Credential: "api_key",
				}}},
			},
		},
	})
	srv.sandboxProxyClient = upstream.Client()
	srv.bindings = sandboxProxyTestBindingStore{resolver: sandboxProxyTestResolver{
		cred: credential.Credential{Kind: "api_key", Value: []byte("lin_secret")},
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/requests", strings.NewReader(`{"connector_fqn":"github://acme/aileron-connector-linear","tool":"linear","operation":"issues.list","method":"GET","upstream_url":"`+upstream.URL+`/graphql"}`))
	rec := httptest.NewRecorder()

	srv.RecordSandboxProxyRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp api.SandboxProxyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.UpstreamStatus != http.StatusNoContent {
		t.Errorf("upstream status = %d", resp.UpstreamStatus)
	}
	if resp.ContentType != nil {
		t.Errorf("content type = %q, want nil", *resp.ContentType)
	}
}

func TestRecordSandboxProxyRequest_CredentialFailuresRejectBeforeUpstream(t *testing.T) {
	const connectorFQN = "github://acme/aileron-connector-linear"
	tests := []struct {
		name        string
		credential  string
		resolver    credential.Resolver
		wantStatus  int
		wantReason  string
		wantMessage string
	}{
		{
			name:        "resolver nil",
			credential:  "api_key",
			wantStatus:  http.StatusForbidden,
			wantReason:  "binding_required",
			wantMessage: "sandbox proxy credential binding is required",
		},
		{
			name:        "resolver binding missing",
			credential:  "api_key",
			resolver:    sandboxProxyTestResolver{err: credential.ErrBindingMissing},
			wantStatus:  http.StatusForbidden,
			wantReason:  "binding_required",
			wantMessage: "sandbox proxy credential binding is required",
		},
		{
			name:        "resolver kind mismatch",
			credential:  "api_key",
			resolver:    sandboxProxyTestResolver{err: credential.ErrCredentialKindMismatch},
			wantStatus:  http.StatusForbidden,
			wantReason:  "credential_kind_mismatch",
			wantMessage: "sandbox proxy credential kind does not match connector spec",
		},
		{
			name:        "resolver generic error",
			credential:  "api_key",
			resolver:    sandboxProxyTestResolver{err: errors.New("vault locked")},
			wantStatus:  http.StatusInternalServerError,
			wantReason:  "credential_resolution_failed",
			wantMessage: "sandbox proxy credential resolution failed",
		},
		{
			name:        "resolved kind mismatch",
			credential:  "oauth2",
			resolver:    sandboxProxyTestResolver{cred: credential.Credential{Kind: "api_key", Value: []byte("secret")}},
			wantStatus:  http.StatusForbidden,
			wantReason:  "credential_kind_mismatch",
			wantMessage: "sandbox proxy credential kind does not match connector spec",
		},
		{
			name:        "unsupported credential kind",
			credential:  "custom",
			resolver:    sandboxProxyTestResolver{cred: credential.Credential{Kind: "custom", Value: []byte("secret")}},
			wantStatus:  http.StatusForbidden,
			wantReason:  "unsupported_credential_kind",
			wantMessage: "sandbox proxy credential kind is not supported",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamCalled := false
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalled = true
				w.WriteHeader(http.StatusTeapot)
			}))
			defer upstream.Close()
			srv, auditStore := newConnectorOperationTestServer([]connectorspec.Spec{
				{
					SchemaVersion: connectorspec.SchemaVersion,
					Connector:     connectorspec.Connector{FQN: connectorFQN},
					Tools: []connectorspec.Tool{
						{Name: "linear", Operations: []connectorspec.Operation{{
							Name: "issues.list", Method: "GET", Path: "/graphql", Hosts: []string{strings.TrimPrefix(upstream.URL, "https://")}, Credential: tt.credential,
						}}},
					},
				},
			})
			srv.sandboxProxyClient = upstream.Client()
			srv.bindings = sandboxProxyTestBindingStore{resolver: tt.resolver}
			req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/requests", strings.NewReader(`{"connector_fqn":"github://acme/aileron-connector-linear","tool":"linear","operation":"issues.list","method":"GET","upstream_url":"`+upstream.URL+`/graphql"}`))
			rec := httptest.NewRecorder()

			srv.RecordSandboxProxyRequest(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if upstreamCalled {
				t.Fatal("upstream was called before credential rejection")
			}
			var resp api.SandboxProxyRejectedResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
			}
			if resp.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", resp.Message, tt.wantMessage)
			}
			events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
			if err != nil {
				t.Fatalf("ListEvents: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if got := events[0].Payload["aileron.connector.reject_reason"]; got != tt.wantReason {
				t.Errorf("reject reason = %v, want %q", got, tt.wantReason)
			}
		})
	}
}

func TestRecordSandboxProxyRequest_UpstreamFailuresRejectWithAudit(t *testing.T) {
	const connectorFQN = "github://acme/aileron-connector-linear"
	tests := []struct {
		name        string
		transport   http.RoundTripper
		wantReason  string
		wantMessage string
	}{
		{
			name: "transport",
			transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed")
			}),
			wantReason:  "upstream_transport_failed",
			wantMessage: "sandbox proxy upstream request failed",
		},
		{
			name: "read",
			transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       errReadCloser{err: errors.New("read failed")},
					Request:    req,
				}, nil
			}),
			wantReason:  "upstream_read_failed",
			wantMessage: "sandbox proxy upstream response read failed",
		},
		{
			name: "too large",
			transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", sandboxProxyMaxResponseBytes+1))),
					Request:    req,
				}, nil
			}),
			wantReason:  "upstream_response_too_large",
			wantMessage: "sandbox proxy upstream response exceeded size limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, auditStore := newConnectorOperationTestServer([]connectorspec.Spec{
				{
					SchemaVersion: connectorspec.SchemaVersion,
					Connector:     connectorspec.Connector{FQN: connectorFQN},
					Tools: []connectorspec.Tool{
						{Name: "linear", Operations: []connectorspec.Operation{{
							Name: "issues.list", Method: "GET", Path: "/graphql", Hosts: []string{"api.linear.app"},
						}}},
					},
				},
			})
			srv.sandboxProxyClient = &http.Client{Transport: tt.transport}
			req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/requests", strings.NewReader(`{"connector_fqn":"github://acme/aileron-connector-linear","tool":"linear","operation":"issues.list","method":"GET","upstream_url":"https://api.linear.app/graphql"}`))
			rec := httptest.NewRecorder()

			srv.RecordSandboxProxyRequest(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
			}
			var resp api.SandboxProxyRejectedResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
			}
			if resp.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", resp.Message, tt.wantMessage)
			}
			events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
			if err != nil {
				t.Fatalf("ListEvents: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if got := events[0].Payload["aileron.connector.reject_reason"]; got != tt.wantReason {
				t.Errorf("reject reason = %v, want %q", got, tt.wantReason)
			}
		})
	}
}

func TestRecordSandboxProxyRequest_DoesNotFollowRedirects(t *testing.T) {
	redirectTargetCalled := false
	redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer redirectTarget.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/unallowed", http.StatusFound)
	}))
	defer upstream.Close()
	srv, _ := newConnectorOperationTestServer([]connectorspec.Spec{
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-linear"},
			Tools: []connectorspec.Tool{
				{Name: "linear", Operations: []connectorspec.Operation{{
					Name: "issues.list", Method: "GET", Path: "/graphql", Hosts: []string{strings.TrimPrefix(upstream.URL, "https://")},
				}}},
			},
		},
	})
	srv.sandboxProxyClient = upstream.Client()
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/requests", strings.NewReader(`{"connector_fqn":"github://acme/aileron-connector-linear","tool":"linear","operation":"issues.list","method":"GET","upstream_url":"`+upstream.URL+`/graphql"}`))
	rec := httptest.NewRecorder()

	srv.RecordSandboxProxyRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if redirectTargetCalled {
		t.Fatal("proxy followed redirect to an unvalidated host")
	}
	var resp api.SandboxProxyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if resp.UpstreamStatus != http.StatusFound {
		t.Errorf("upstream status = %d, want %d", resp.UpstreamStatus, http.StatusFound)
	}
}

type errReadCloser struct {
	err error
}

func (r errReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r errReadCloser) Close() error {
	return nil
}

func TestRecordSandboxProxyRequest_RejectsNonHTTPSWithoutAudit(t *testing.T) {
	srv, auditStore := newConnectorOperationTestServer([]connectorspec.Spec{
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-google"},
			Tools: []connectorspec.Tool{
				{Name: "google", Operations: []connectorspec.Operation{{Name: "gmail.messages.search", Method: "GET", Path: "/gmail/v1/users/me/messages", Hosts: []string{"gmail.googleapis.com"}}}},
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/requests", bytes.NewReader([]byte(`{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.search","method":"GET","upstream_url":"http://gmail.googleapis.com/gmail/v1/users/me/messages"}`)))
	rec := httptest.NewRecorder()

	srv.RecordSandboxProxyRequest(rec, req)

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

func TestRecordSandboxProxyRequest_MethodOrPathMismatchReturns404WithoutAudit(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{
			name: "method",
			body: `{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.search","method":"POST","upstream_url":"https://gmail.googleapis.com/gmail/v1/users/me/messages"}`,
		},
		{
			name: "path",
			body: `{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.search","method":"GET","upstream_url":"https://gmail.googleapis.com/gmail/v1/users/me/labels"}`,
		},
		{
			name: "host",
			body: `{"connector_fqn":"github://acme/aileron-connector-google","tool":"google","operation":"gmail.messages.search","method":"GET","upstream_url":"https://evil.example.test/gmail/v1/users/me/messages"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, auditStore := newConnectorOperationTestServer([]connectorspec.Spec{
				{
					SchemaVersion: connectorspec.SchemaVersion,
					Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-google"},
					Tools: []connectorspec.Tool{
						{Name: "google", Operations: []connectorspec.Operation{{Name: "gmail.messages.search", Method: "GET", Path: "/gmail/v1/users/me/messages", Hosts: []string{"gmail.googleapis.com"}}}},
					},
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/requests", bytes.NewReader([]byte(tt.body)))
			rec := httptest.NewRecorder()

			srv.RecordSandboxProxyRequest(rec, req)

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
		})
	}
}

func TestRecordSandboxProxyRequest_RootPathMatchesOperation(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("upstream path = %s, want /", r.URL.Path)
		}
		_, _ = w.Write([]byte("root"))
	}))
	defer upstream.Close()
	srv, auditStore := newConnectorOperationTestServer([]connectorspec.Spec{
		{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-root"},
			Tools: []connectorspec.Tool{
				{Name: "root", Operations: []connectorspec.Operation{{Name: "root.get", Method: "GET", Path: "/", Hosts: []string{strings.TrimPrefix(upstream.URL, "https://")}}}},
			},
		},
	})
	srv.sandboxProxyClient = upstream.Client()
	req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/requests", bytes.NewReader([]byte(`{"connector_fqn":"github://acme/aileron-connector-root","tool":"root","operation":"root.get","method":"GET","upstream_url":"`+upstream.URL+`"}`)))
	rec := httptest.NewRecorder()

	srv.RecordSandboxProxyRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeConnectorProxyProxied {
		t.Errorf("event type = %q, want %q", events[0].EventType, model.EventTypeConnectorProxyProxied)
	}
	payload := events[0].Payload
	if payload["aileron.connector.fqn"] != "github://acme/aileron-connector-root" {
		t.Errorf("connector payload = %v", payload["aileron.connector.fqn"])
	}
	if payload["aileron.connector.tool"] != "root" {
		t.Errorf("tool payload = %v", payload["aileron.connector.tool"])
	}
	if payload["aileron.connector.operation"] != "root.get" {
		t.Errorf("operation payload = %v", payload["aileron.connector.operation"])
	}
	if payload["aileron.proxy.upstream.path"] != "/" {
		t.Errorf("path payload = %v", payload["aileron.proxy.upstream.path"])
	}
	if _, ok := payload["upstream_url"]; ok {
		t.Errorf("raw upstream_url should not be audited: %v", payload["upstream_url"])
	}
}

func TestSandboxProxyHostAllowed_DefaultPortOnlyForBareHost(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		allowed []string
		want    bool
	}{
		{name: "bare host default", rawURL: "https://gmail.googleapis.com/path", allowed: []string{"gmail.googleapis.com"}, want: true},
		{name: "bare host explicit default", rawURL: "https://gmail.googleapis.com:443/path", allowed: []string{"gmail.googleapis.com"}, want: true},
		{name: "bare host non default", rawURL: "https://gmail.googleapis.com:444/path", allowed: []string{"gmail.googleapis.com"}, want: false},
		{name: "host port exact", rawURL: "https://gmail.googleapis.com:444/path", allowed: []string{"gmail.googleapis.com:444"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream, err := parseSandboxProxyUpstreamURL(tt.rawURL)
			if err != nil {
				t.Fatalf("parseSandboxProxyUpstreamURL: %v", err)
			}
			if got := sandboxProxyHostAllowed(upstream, tt.allowed); got != tt.want {
				t.Fatalf("sandboxProxyHostAllowed() = %v, want %v", got, tt.want)
			}
		})
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
