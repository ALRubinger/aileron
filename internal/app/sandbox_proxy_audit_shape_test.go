package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/sandbox/discovery"
)

// sandboxProxyEventShape describes the documented field contract for a
// sandbox proxy audit event family. Required fields must always be
// present and non-empty; allowed fields may be present; forbidden
// substrings must never appear in any string value (defense against
// credential leaks).
type sandboxProxyEventShape struct {
	eventType        string
	requiredFields   []string
	allowedFields    []string
	forbiddenSubstrs []string
}

func (s sandboxProxyEventShape) validate(t *testing.T, payload map[string]any) {
	t.Helper()
	for _, field := range s.requiredFields {
		val, ok := payload[field]
		if !ok {
			t.Errorf("[%s] missing required field %q; payload=%v", s.eventType, field, payload)
			continue
		}
		if str, ok := val.(string); ok && strings.TrimSpace(str) == "" {
			t.Errorf("[%s] required field %q is empty", s.eventType, field)
		}
	}
	allowed := map[string]struct{}{}
	for _, f := range append(s.requiredFields, s.allowedFields...) {
		allowed[f] = struct{}{}
	}
	for field := range payload {
		if _, ok := allowed[field]; !ok {
			t.Errorf("[%s] payload contains undocumented field %q (add to allowed list or remove)", s.eventType, field)
		}
	}
	payloadJSON, _ := json.Marshal(payload)
	for _, forbidden := range s.forbiddenSubstrs {
		if strings.Contains(string(payloadJSON), forbidden) {
			t.Errorf("[%s] payload leaks forbidden substring %q: %s", s.eventType, forbidden, string(payloadJSON))
		}
	}
}

// connectorProxyProxiedShape, connectorProxyRejectedShape, and
// sandboxProxyRejectedShape describe the three sandbox HTTPS data
// plane event families. The plan's R29 reconciliation requirement
// asks for a "shape conformance" test that walks every emission site
// and verifies no field violations against the documented schema.
//
// These shapes mirror the field tables in
// docs/src/content/docs/guides/observability.md "Sandbox HTTPS data
// plane" section. When that documentation changes, this struct must
// change with it. The accompanying tests below stress every emission
// site by name.
//
// The sandbox.proxy.disabled shape lands in a follow-up once the
// new daemon endpoint from PR #970 (issue #896 Section B) is on main;
// the documentation already describes it.
var (
	connectorProxyProxiedShape = sandboxProxyEventShape{
		eventType: "connector.proxy.proxied",
		requiredFields: []string{
			"aileron.connector.fqn",
			"aileron.connector.tool",
			"aileron.connector.operation",
			"aileron.connector.boundary",
			"aileron.connector.mediation",
			"aileron.connector.decision",
			"aileron.proxy.source",
			"aileron.proxy.method",
			"aileron.proxy.upstream.scheme",
			"aileron.proxy.upstream.host",
			"aileron.proxy.upstream.path",
			"aileron.proxy.upstream.status",
		},
		allowedFields: []string{
			"aileron.connector.method",
			"aileron.connector.path",
			"aileron.connector.idempotency",
			"aileron.connector.approval",
			"aileron.connector.credential",
			"aileron.connector.credential_required",
			"aileron.session.id",
		},
		forbiddenSubstrs: []string{
			"lin_secret", "Bearer ", "Authorization",
		},
	}

	connectorProxyRejectedShape = sandboxProxyEventShape{
		eventType: "connector.proxy.rejected",
		requiredFields: []string{
			"aileron.connector.fqn",
			"aileron.connector.tool",
			"aileron.connector.operation",
			"aileron.connector.boundary",
			"aileron.connector.mediation",
			"aileron.connector.decision",
			"aileron.connector.reject_reason",
			"aileron.proxy.source",
			"aileron.proxy.method",
			"aileron.proxy.upstream.scheme",
			"aileron.proxy.upstream.host",
			"aileron.proxy.upstream.path",
		},
		allowedFields: []string{
			"aileron.connector.method",
			"aileron.connector.path",
			"aileron.connector.idempotency",
			"aileron.connector.approval",
			"aileron.connector.credential",
			"aileron.connector.credential_required",
			"aileron.session.id",
		},
		forbiddenSubstrs: []string{
			"lin_secret", "Bearer ", "Authorization",
		},
	}

	sandboxProxyRejectedShape = sandboxProxyEventShape{
		eventType: "sandbox.proxy.rejected",
		requiredFields: []string{
			"aileron.proxy.boundary",
			"aileron.proxy.mediation",
			"aileron.proxy.decision",
			"aileron.proxy.reject_reason",
			"aileron.proxy.source",
			"aileron.proxy.method",
			"aileron.proxy.upstream.scheme",
			"aileron.proxy.upstream.host",
			"aileron.proxy.upstream.path",
		},
		allowedFields: []string{
			"aileron.session.id",
		},
		forbiddenSubstrs: []string{
			"lin_secret", "Bearer ", "Authorization",
		},
	}

	sandboxProxyPassthroughShape = sandboxProxyEventShape{
		eventType: "sandbox.proxy.passthrough",
		requiredFields: []string{
			"aileron.proxy.boundary",
			"aileron.proxy.mediation",
			"aileron.proxy.source",
			"aileron.proxy.decision",
			"aileron.proxy.method",
			"aileron.proxy.upstream.scheme",
			"aileron.proxy.upstream.host",
			"aileron.proxy.upstream.path",
			"aileron.proxy.upstream.status",
		},
		allowedFields: []string{
			"aileron.session.id",
		},
		forbiddenSubstrs: []string{
			"lin_secret", "Bearer ", "Authorization",
		},
	}
)

func TestSandboxProxyAuditShape_ConnectorProxyProxiedConforms(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-shape-proxied" }),
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sandbox-proxy/requests", nil)
	req.Header.Set("X-Aileron-Session-Id", "session-shape-test")
	upstream, _ := url.Parse("https://api.example.test/graphql")
	srv.recordSandboxProxyProxied(req, sandboxProxySourceTransparentConnectTLS, "github://acme/aileron-connector-linear", "linear",
		"GET", upstream, discovery.SpecOperationHelp{
			Name: "viewer.get", Method: "GET", Path: "/graphql", Credential: "api_key",
		}, 200)
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeConnectorProxyProxied {
		t.Fatalf("event type = %q", events[0].EventType)
	}
	connectorProxyProxiedShape.validate(t, events[0].Payload)
}

func TestSandboxProxyAuditShape_ConnectorProxyRejectedConforms(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-shape-rejected" }),
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sandbox-proxy/requests", nil)
	req.Header.Set("X-Aileron-Session-Id", "session-shape-test")
	upstream, _ := url.Parse("https://api.example.test/graphql")
	srv.recordSandboxProxyRejected(req, sandboxProxySourceTransparentConnectTLS, "github://acme/aileron-connector-linear", "linear",
		"GET", upstream, discovery.SpecOperationHelp{
			Name: "viewer.get", Method: "GET", Path: "/graphql", Credential: "api_key",
		}, "request_body_too_large")
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeConnectorProxyRejected {
		t.Fatalf("event type = %q", events[0].EventType)
	}
	connectorProxyRejectedShape.validate(t, events[0].Payload)
}

func TestSandboxProxyAuditShape_SandboxProxyRejectedConforms(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-shape-unresolved" }),
	}
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Header.Set("X-Aileron-Session-Id", "session-shape-test")
	upstream, _ := url.Parse("https://api.example.test/v1/resource")
	srv.recordSandboxProxyProtocolRejected(req, sandboxProxySourceTransparentConnectTLS, "GET", upstream, "non_connect_proxy_request_unsupported")
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyRejected {
		t.Fatalf("event type = %q", events[0].EventType)
	}
	sandboxProxyRejectedShape.validate(t, events[0].Payload)
}

func TestSandboxProxyAuditShape_SandboxProxyPassthroughConforms(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-shape-passthrough" }),
	}
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Header.Set("X-Aileron-Session-Id", "session-shape-test")
	upstream, _ := url.Parse("https://api.unknown.test/v1/resource")
	srv.recordSandboxProxyPassthrough(req, sandboxProxySourceTransparentConnectTLS, "GET", upstream, 200)
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyPassthrough {
		t.Fatalf("event type = %q", events[0].EventType)
	}
	sandboxProxyPassthroughShape.validate(t, events[0].Payload)
}
