package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/sandbox/discovery"
)

// mustAuditShapeBinding constructs a minimal host binding for the audit
// shape tests, optionally threading trust-scope/identity construction
// options so a payload carrying the identity triple or effect can be
// exercised.
func mustAuditShapeBinding(t *testing.T, host, scheme string, opts ...binding.HostBindingOption) binding.HostBinding {
	t.Helper()
	hb, err := binding.NewHostBinding(host, "api_key/github/octocat", scheme, opts...)
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	return hb
}

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

	sandboxProxyUpgradeShape = sandboxProxyEventShape{
		eventType: "sandbox.proxy.upgrade",
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

	sandboxProxyBindingInjectedShape = sandboxProxyEventShape{
		eventType: "sandbox.proxy.binding_injected",
		requiredFields: []string{
			"aileron.proxy.boundary",
			"aileron.proxy.mediation",
			"aileron.proxy.source",
			"aileron.proxy.decision",
			"aileron.proxy.method",
			"aileron.proxy.binding.host",
			"aileron.proxy.binding.scheme",
			"aileron.proxy.upstream.scheme",
			"aileron.proxy.upstream.host",
			"aileron.proxy.upstream.path",
			"aileron.proxy.upstream.status",
		},
		allowedFields: []string{
			"aileron.session.id",
			"aileron.trust.effect",
			"aileron.plan.id",
			"aileron.step.id",
			"aileron.tool.name",
		},
		forbiddenSubstrs: []string{
			"lin_secret", "Bearer ", "Authorization",
		},
	}

	sandboxProxyTrustDeniedShape = sandboxProxyEventShape{
		eventType: "sandbox.proxy.trust_denied",
		requiredFields: []string{
			"aileron.proxy.boundary",
			"aileron.proxy.mediation",
			"aileron.proxy.source",
			"aileron.proxy.decision",
			"aileron.proxy.reject_reason",
			"aileron.proxy.method",
			"aileron.proxy.binding.host",
			"aileron.proxy.binding.scheme",
			"aileron.proxy.upstream.scheme",
			"aileron.proxy.upstream.host",
			"aileron.proxy.upstream.path",
		},
		allowedFields: []string{
			"aileron.session.id",
			"aileron.trust.effect",
			"aileron.plan.id",
			"aileron.step.id",
			"aileron.tool.name",
		},
		forbiddenSubstrs: []string{
			"lin_secret", "Bearer ", "Authorization",
		},
	}

	sandboxProxyForeignTokenNotSwappedShape = sandboxProxyEventShape{
		eventType: "sandbox.proxy.foreign_token_not_swapped",
		requiredFields: []string{
			"aileron.proxy.boundary",
			"aileron.proxy.mediation",
			"aileron.proxy.source",
			"aileron.proxy.decision",
			"aileron.proxy.method",
			"aileron.proxy.binding.host",
			"aileron.proxy.binding.scheme",
			"aileron.proxy.upstream.scheme",
			"aileron.proxy.upstream.host",
			"aileron.proxy.upstream.path",
			"aileron.proxy.upstream.status",
		},
		allowedFields: []string{
			"aileron.session.id",
			"aileron.trust.effect",
			"aileron.plan.id",
			"aileron.step.id",
			"aileron.tool.name",
		},
		forbiddenSubstrs: []string{
			"lin_secret", "Bearer ", "Authorization",
		},
	}

	sandboxProxyDisabledShape = sandboxProxyEventShape{
		eventType: "sandbox.proxy.disabled",
		requiredFields: []string{
			"aileron.proxy.source",
			"aileron.proxy.boundary",
			"aileron.proxy.decision",
			"aileron.proxy.disabled_reason",
			"aileron.sandbox.mode",
			"aileron.session.id",
		},
		allowedFields: []string{
			"aileron.sandbox.image",
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

func TestSandboxProxyAuditShape_SandboxProxyBindingInjectedConforms(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-shape-binding" }),
	}
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Header.Set("X-Aileron-Session-Id", "session-shape-test")
	req.Method = http.MethodGet
	upstream, _ := url.Parse("https://api.example.test/v1/resource")
	srv.recordSandboxProxyBindingInjected(req, sandboxProxySourceTransparentConnectTLS, mustAuditShapeBinding(t, "*.example.test", "bearer"), upstream, 200)
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyBindingInjected {
		t.Fatalf("event type = %q", events[0].EventType)
	}
	sandboxProxyBindingInjectedShape.validate(t, events[0].Payload)
}

// TestSandboxProxyAuditShape_BindingInjectedCarriesTrustIdentity asserts a
// binding_injected payload surfaces the declared trust effect and the
// plan/step/tool identity triple when the matched binding carries them,
// without leaking the AllowedHosts values or the credential-ref.
func TestSandboxProxyAuditShape_BindingInjectedCarriesTrustIdentity(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-shape-identity" }),
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Aileron-Session-Id", "session-shape-test")
	upstream, _ := url.Parse("https://api.example.test/v1/resource")
	hb := mustAuditShapeBinding(t, "*.example.test", "bearer",
		binding.WithTrustContract("write", []string{"secret-host.example.test"}),
		binding.WithToolIdentity("plan-7", "step-3", "linear.create_issue"),
	)
	srv.recordSandboxProxyBindingInjected(req, sandboxProxySourceTransparentConnectTLS, hb, upstream, 200)
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	p := events[0].Payload
	sandboxProxyBindingInjectedShape.validate(t, p)
	if got := p["aileron.trust.effect"]; got != "write" {
		t.Errorf("trust.effect = %v, want write", got)
	}
	if got := p["aileron.plan.id"]; got != "plan-7" {
		t.Errorf("plan.id = %v, want plan-7", got)
	}
	if got := p["aileron.step.id"]; got != "step-3" {
		t.Errorf("step.id = %v, want step-3", got)
	}
	if got := p["aileron.tool.name"]; got != "linear.create_issue" {
		t.Errorf("tool.name = %v, want linear.create_issue", got)
	}
	payloadJSON, _ := json.Marshal(p)
	// The AllowedHosts values and the credential-ref must never appear.
	for _, forbidden := range []string{"secret-host.example.test", "api_key/github/octocat"} {
		if strings.Contains(string(payloadJSON), forbidden) {
			t.Errorf("payload leaked %q: %s", forbidden, payloadJSON)
		}
	}
}

// TestSandboxProxyAuditShape_TrustDeniedConforms pins the
// sandbox.proxy.trust_denied family: a denial at the trust gate emits the
// documented shape carrying the reject reason, the declared effect, and the
// identity triple, with no AllowedHosts value or credential-ref.
func TestSandboxProxyAuditShape_TrustDeniedConforms(t *testing.T) {
	for _, reason := range []string{trustRejectHostNotAllowed, trustRejectEffectNotAllowed} {
		t.Run(reason, func(t *testing.T) {
			auditStore := audit.NewMemStore()
			srv := &apiServer{
				auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-shape-denied" }),
			}
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Aileron-Session-Id", "session-shape-test")
			upstream, _ := url.Parse("https://api.example.test/v1/resource")
			hb := mustAuditShapeBinding(t, "*.example.test", "bearer",
				binding.WithTrustContract("read", []string{"allowed.example.test"}),
				binding.WithToolIdentity("plan-7", "step-3", "linear.create_issue"),
			)
			srv.recordSandboxProxyTrustDenied(req, sandboxProxySourceTransparentConnectTLS, hb, upstream, reason)
			events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if events[0].EventType != model.EventTypeSandboxProxyTrustDenied {
				t.Fatalf("event type = %q", events[0].EventType)
			}
			p := events[0].Payload
			sandboxProxyTrustDeniedShape.validate(t, p)
			if got := p["aileron.proxy.reject_reason"]; got != reason {
				t.Errorf("reject_reason = %v, want %q", got, reason)
			}
			payloadJSON, _ := json.Marshal(p)
			for _, forbidden := range []string{"allowed.example.test", "api_key/github/octocat"} {
				if strings.Contains(string(payloadJSON), forbidden) {
					t.Errorf("payload leaked %q: %s", forbidden, payloadJSON)
				}
			}
		})
	}
}

func TestSandboxProxyAuditShape_TrustDeniedNilRecorderIsIDSafe(t *testing.T) {
	upstream, _ := url.Parse("https://api.example.test/v1/resource")
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	hb := mustAuditShapeBinding(t, "*.example.test", "bearer")

	srv := &apiServer{newID: func() string { return "minted-id" }}
	if got := srv.recordSandboxProxyTrustDenied(req, sandboxProxySourceTransparentConnectTLS, hb, upstream, trustRejectHostNotAllowed); got != "minted-id" {
		t.Errorf("audit id = %q, want minted-id", got)
	}

	srvNoID := &apiServer{}
	if got := srvNoID.recordSandboxProxyTrustDenied(req, sandboxProxySourceTransparentConnectTLS, hb, upstream, trustRejectHostNotAllowed); strings.TrimSpace(got) == "" {
		t.Error("audit id must be non-empty even with no recorder and no newID")
	}
}

func TestSandboxProxyAuditShape_ForeignTokenNotSwappedConforms(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-shape-foreign" }),
	}
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Header.Set("X-Aileron-Session-Id", "session-shape-test")
	req.Method = http.MethodGet
	upstream, _ := url.Parse("https://api.github.com/user")
	hb := mustAuditShapeBinding(t, "api.github.com", "bearer",
		binding.WithTrustContract("read", []string{"secret-host.example.test"}),
		binding.WithToolIdentity("plan-7", "step-3", "linear.create_issue"),
	)
	srv.recordSandboxProxyForeignTokenNotSwapped(req, sandboxProxySourceTransparentConnectTLS, hb, upstream, 200)
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyForeignTokenNotSwapped {
		t.Fatalf("event type = %q", events[0].EventType)
	}
	p := events[0].Payload
	sandboxProxyForeignTokenNotSwappedShape.validate(t, p)
	if got := p["aileron.trust.effect"]; got != "read" {
		t.Errorf("trust.effect = %v, want read", got)
	}
	if got := p["aileron.plan.id"]; got != "plan-7" {
		t.Errorf("plan.id = %v, want plan-7", got)
	}
	if got := p["aileron.step.id"]; got != "step-3" {
		t.Errorf("step.id = %v, want step-3", got)
	}
	if got := p["aileron.tool.name"]; got != "linear.create_issue" {
		t.Errorf("tool.name = %v, want linear.create_issue", got)
	}
	payloadJSON, _ := json.Marshal(p)
	// The AllowedHosts values and the credential-ref must never appear.
	for _, forbidden := range []string{"secret-host.example.test", "api_key/github/octocat"} {
		if strings.Contains(string(payloadJSON), forbidden) {
			t.Errorf("payload leaked %q: %s", forbidden, payloadJSON)
		}
	}
}

func TestSandboxProxyAuditShape_ForeignTokenNotSwappedNilRecorderIsIDSafe(t *testing.T) {
	upstream, _ := url.Parse("https://api.github.com/user")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	hb := mustAuditShapeBinding(t, "api.github.com", "bearer")

	srv := &apiServer{newID: func() string { return "minted-id" }}
	if got := srv.recordSandboxProxyForeignTokenNotSwapped(req, sandboxProxySourceTransparentConnectTLS, hb, upstream, 200); got != "minted-id" {
		t.Errorf("audit id = %q, want minted-id", got)
	}

	srvNoID := &apiServer{}
	if got := srvNoID.recordSandboxProxyForeignTokenNotSwapped(req, sandboxProxySourceTransparentConnectTLS, hb, upstream, 200); strings.TrimSpace(got) == "" {
		t.Error("audit id must be non-empty even with no recorder and no newID")
	}
}

func TestSandboxProxyAuditShape_BindingInjectedNilRecorderIsIDSafe(t *testing.T) {
	upstream, _ := url.Parse("https://api.example.test/v1/resource")
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	hb := mustAuditShapeBinding(t, "*.example.test", "bearer")

	// With newID wired, the recorder mints from it.
	srv := &apiServer{newID: func() string { return "minted-id" }}
	if got := srv.recordSandboxProxyBindingInjected(req, sandboxProxySourceTransparentConnectTLS, hb, upstream, 200); got != "minted-id" {
		t.Errorf("audit id = %q, want minted-id", got)
	}

	// With no newID and no recorder, it falls back to the default ID fn.
	srvNoID := &apiServer{}
	if got := srvNoID.recordSandboxProxyBindingInjected(req, sandboxProxySourceTransparentConnectTLS, hb, upstream, 200); strings.TrimSpace(got) == "" {
		t.Error("audit id must be non-empty even with no recorder and no newID")
	}
}

func TestSandboxProxyAuditShape_SandboxProxyUpgradeConforms(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-shape-upgrade" }),
	}
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Header.Set("X-Aileron-Session-Id", "session-shape-test")
	upstream, _ := url.Parse("https://api.unknown.test/backend-api/codex/responses")
	srv.recordSandboxProxyUpgrade(req, sandboxProxySourceTransparentConnectTLS, "GET", upstream, http.StatusSwitchingProtocols)
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyUpgrade {
		t.Fatalf("event type = %q", events[0].EventType)
	}
	sandboxProxyUpgradeShape.validate(t, events[0].Payload)
	if got := events[0].Payload["aileron.proxy.upstream.status"]; got != http.StatusSwitchingProtocols {
		t.Errorf("upstream status = %v, want 101", got)
	}
}

// TestSandboxProxyAuditShape_SandboxProxyDisabledConforms pins the
// sandbox.proxy.disabled family (gate A). RecordSandboxProxyDisabled is
// the daemon endpoint the launcher posts to once per launch session
// when the v4 HTTPS proxy is not in force. Each disabled_reason enum is
// exercised as a subtest; every emission must conform to the documented
// field contract.
func TestSandboxProxyAuditShape_SandboxProxyDisabledConforms(t *testing.T) {
	reasons := []api.SandboxProxyDisabledRequestReason{
		api.UserOptOut,
		api.PreflightFailed,
		api.UnsupportedSandboxMode,
	}
	for _, reason := range reasons {
		t.Run(string(reason), func(t *testing.T) {
			auditStore := audit.NewMemStore()
			srv := &apiServer{
				auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-shape-disabled" }),
			}
			mode := "docker"
			image := "ghcr.io/acme/sandbox:latest"
			body, err := json.Marshal(api.SandboxProxyDisabledRequest{
				Reason:       reason,
				SessionId:    "session-shape-test",
				SandboxMode:  &mode,
				SandboxImage: &image,
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/disabled", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			srv.RecordSandboxProxyDisabled(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rec.Code)
			}
			events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
			if len(events) != 1 {
				t.Fatalf("events = %d, want 1", len(events))
			}
			if events[0].EventType != model.EventTypeSandboxProxyDisabled {
				t.Fatalf("event type = %q", events[0].EventType)
			}
			sandboxProxyDisabledShape.validate(t, events[0].Payload)
			if got := events[0].Payload["aileron.proxy.disabled_reason"]; got != string(reason) {
				t.Errorf("disabled_reason = %v, want %q", got, reason)
			}
		})
	}
}

// TestSandboxProxyAuditShape_NoCredentialLeakAcrossFamilies is the
// cross-family no-credential-leak sweep (gate E): the single most
// load-bearing invariant for the credential-sealing claim. It drives
// every sandbox/data-plane audit emitter with inputs that carry
// credential-shaped substrings — an Authorization: Bearer header, a
// secret-shaped header, and a query string on the upstream URL — and
// asserts no emitter's serialized payload echoes any of them. Host and
// path are surfaced; query strings, headers, and credential bytes are
// not.
func TestSandboxProxyAuditShape_NoCredentialLeakAcrossFamilies(t *testing.T) {
	const (
		bearer    = "Bearer test-token"
		linSecret = "lin_secret_xyz"
		query     = "token=value"
	)
	forbidden := []string{bearer, linSecret, query, "?" + query, "test-token"}

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-leak-sweep" }),
	}

	poison := func(req *http.Request) *http.Request {
		req.Header.Set("X-Aileron-Session-Id", "session-leak-sweep")
		req.Header.Set("Authorization", bearer)
		req.Header.Set("X-Aileron-Upstream-Secret", linSecret)
		return req
	}

	// Upstream URL carries credential-shaped query parameters; the
	// recorders must surface host + path only, never the raw query.
	upstream, err := url.Parse("https://api.example.test/v1/resource?" + query + "&secret=" + linSecret)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	op := discovery.SpecOperationHelp{
		Name: "viewer.get", Method: "GET", Path: "/v1/resource", Credential: "api_key",
		Hosts: []string{"api.example.test"},
	}
	const fqn = "github://acme/aileron-connector-linear"

	// A binding whose non-secret trust scope carries credential-shaped
	// substrings: the AllowedHosts and credential-ref must never echo into a
	// payload.
	trustHB := mustAuditShapeBinding(t, "*.example.test", "bearer",
		binding.WithTrustContract("read", []string{"secret." + linSecret + ".example.test"}),
		binding.WithToolIdentity("plan-leak", "step-leak", "tool-leak"),
	)

	proxyReq := poison(httptest.NewRequest(http.MethodConnect, "/", nil))
	srv.recordSandboxProxyProxied(proxyReq, sandboxProxySourceTransparentConnectTLS, fqn, "linear", "GET", upstream, op, 200)
	srv.recordSandboxProxyRejected(proxyReq, sandboxProxySourceTransparentConnectTLS, fqn, "linear", "GET", upstream, op, "upstream_transport_failed")
	srv.recordSandboxProxyProtocolRejected(proxyReq, sandboxProxySourceTransparentConnectTLS, "GET", upstream, "session_ca_unavailable")
	srv.recordSandboxProxyPassthrough(proxyReq, sandboxProxySourceTransparentConnectTLS, "GET", upstream, 200)
	srv.recordSandboxProxyUpgrade(proxyReq, sandboxProxySourceTransparentConnectTLS, "GET", upstream, http.StatusSwitchingProtocols)
	srv.recordConnectorOperationRejected(proxyReq, fqn, "linear", op)
	srv.recordSandboxProxyBindingInjected(proxyReq, sandboxProxySourceTransparentConnectTLS, trustHB, upstream, 200)
	srv.recordSandboxProxyTrustDenied(proxyReq, sandboxProxySourceTransparentConnectTLS, trustHB, upstream, trustRejectHostNotAllowed)

	mode := "docker"
	disabledBody, err := json.Marshal(api.SandboxProxyDisabledRequest{
		Reason:      api.UserOptOut,
		SessionId:   "session-leak-sweep",
		SandboxMode: &mode,
	})
	if err != nil {
		t.Fatalf("marshal disabled request: %v", err)
	}
	disabledReq := poison(httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/disabled", bytes.NewReader(disabledBody)))
	srv.RecordSandboxProxyDisabled(httptest.NewRecorder(), disabledReq)

	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 9 {
		t.Fatalf("events = %d, want 9 (one per emitter)", len(events))
	}
	for _, ev := range events {
		payloadJSON, _ := json.Marshal(ev.Payload)
		for _, f := range forbidden {
			if strings.Contains(string(payloadJSON), f) {
				t.Errorf("[%s] payload leaks credential-shaped substring %q: %s", ev.EventType, f, string(payloadJSON))
			}
		}
	}
}
