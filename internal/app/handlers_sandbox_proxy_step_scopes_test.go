package app

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
)

// mintStepScope drives the real POST handler and returns the minted scope.
func mintStepScope(t *testing.T, srv *apiServer, sessionID, stepID string, hosts []string) api.SandboxProxyStepScopeResponse {
	t.Helper()
	body, _ := json.Marshal(api.SandboxProxyStepScopeRequest{
		SessionId: sessionID,
		StepId:    stepID,
		Hosts:     hosts,
	})
	rec := httptest.NewRecorder()
	srv.CreateSandboxProxyStepScope(rec, httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/step-scopes", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var out api.SandboxProxyStepScopeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	return out
}

// TestCreateSandboxProxyStepScope_MintsFreshEphemeralCredential proves the
// mint contract: a valid request yields a scope id, a random token, and a
// future expiry, and two mints yield distinct credentials (not idempotent by
// design — each mint is a fresh ephemeral credential).
func TestCreateSandboxProxyStepScope_MintsFreshEphemeralCredential(t *testing.T) {
	srv := &apiServer{}
	a := mintStepScope(t, srv, "session-123", "extract", []string{"api.example.com"})
	b := mintStepScope(t, srv, "session-123", "extract", []string{"api.example.com"})

	if a.ScopeId == "" || a.Token == "" {
		t.Fatalf("mint returned empty scope id or token: %+v", a)
	}
	if !a.ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at = %v, want a future instant", a.ExpiresAt)
	}
	if a.ScopeId == b.ScopeId || a.Token == b.Token {
		t.Errorf("two mints must yield distinct credentials: %+v vs %+v", a, b)
	}
}

// TestCreateSandboxProxyStepScope_ValidatesShapes proves the mint refuses
// malformed session ids, step ids, and hosts with 400 (fail-closed input
// validation; nothing is registered on a refusal).
func TestCreateSandboxProxyStepScope_ValidatesShapes(t *testing.T) {
	cases := []struct {
		name string
		req  api.SandboxProxyStepScopeRequest
	}{
		{"missing session id", api.SandboxProxyStepScopeRequest{StepId: "s1", Hosts: []string{"api.example.com"}}},
		{"path-escaping session id", api.SandboxProxyStepScopeRequest{SessionId: "../etc", StepId: "s1", Hosts: []string{"api.example.com"}}},
		{"missing step id", api.SandboxProxyStepScopeRequest{SessionId: "session-123", Hosts: []string{"api.example.com"}}},
		{"malformed step id", api.SandboxProxyStepScopeRequest{SessionId: "session-123", StepId: "bad step", Hosts: []string{"api.example.com"}}},
		{"empty hosts", api.SandboxProxyStepScopeRequest{SessionId: "session-123", StepId: "s1"}},
		{"scheme-carrying host", api.SandboxProxyStepScopeRequest{SessionId: "session-123", StepId: "s1", Hosts: []string{"https://api.example.com"}}},
		{"path-carrying host", api.SandboxProxyStepScopeRequest{SessionId: "session-123", StepId: "s1", Hosts: []string{"api.example.com/path"}}},
		{"empty host entry", api.SandboxProxyStepScopeRequest{SessionId: "session-123", StepId: "s1", Hosts: []string{" "}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &apiServer{}
			body, _ := json.Marshal(tc.req)
			rec := httptest.NewRecorder()
			srv.CreateSandboxProxyStepScope(rec, httptest.NewRequest(http.MethodPost, "/v1/sandbox-proxy/step-scopes", bytes.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if len(srv.stepScopes) != 0 {
				t.Errorf("a refused mint must register nothing, got %d scopes", len(srv.stepScopes))
			}
		})
	}
}

// TestDeleteSandboxProxyStepScope_IdempotentRelease proves DELETE removes a
// live scope and stays 204 for an unknown/already-released scope id.
func TestDeleteSandboxProxyStepScope_IdempotentRelease(t *testing.T) {
	srv := &apiServer{}
	minted := mintStepScope(t, srv, "session-123", "extract", []string{"api.example.com"})

	rec := httptest.NewRecorder()
	srv.DeleteSandboxProxyStepScope(rec, httptest.NewRequest(http.MethodDelete, "/v1/sandbox-proxy/step-scopes/"+minted.ScopeId, nil), minted.ScopeId)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	if _, ok := srv.lookupSandboxProxyStepScope("session-123", minted.Token); ok {
		t.Error("a released scope must no longer authenticate")
	}

	rec = httptest.NewRecorder()
	srv.DeleteSandboxProxyStepScope(rec, httptest.NewRequest(http.MethodDelete, "/v1/sandbox-proxy/step-scopes/"+minted.ScopeId, nil), minted.ScopeId)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("repeat delete status = %d, want 204 (idempotent)", rec.Code)
	}
}

// TestLookupSandboxProxyStepScope_Semantics proves the auth-time lookup
// contract: a live token binds only under its own session, an expired scope
// never matches (and is reaped), and a wrong token never matches.
func TestLookupSandboxProxyStepScope_Semantics(t *testing.T) {
	srv := &apiServer{}
	minted := mintStepScope(t, srv, "session-123", "extract", []string{"api.example.com"})

	if scope, ok := srv.lookupSandboxProxyStepScope("session-123", minted.Token); !ok {
		t.Fatal("a live token under its own session must authenticate")
	} else if scope.StepID != "extract" || strings.Join(scope.Hosts, ",") != "api.example.com" {
		t.Errorf("scope = %+v, want the registered step id and hosts", scope)
	}
	if _, ok := srv.lookupSandboxProxyStepScope("other-session", minted.Token); ok {
		t.Error("a scope token must never authenticate a foreign session")
	}
	if _, ok := srv.lookupSandboxProxyStepScope("session-123", "not-the-token"); ok {
		t.Error("a wrong token must not authenticate")
	}

	// Expire the scope in place: the lookup must refuse AND reap it.
	srv.stepScopesMu.Lock()
	for id, scope := range srv.stepScopes {
		scope.ExpiresAt = time.Now().Add(-time.Second)
		srv.stepScopes[id] = scope
	}
	srv.stepScopesMu.Unlock()
	if _, ok := srv.lookupSandboxProxyStepScope("session-123", minted.Token); ok {
		t.Error("an expired scope must not authenticate")
	}
	srv.stepScopesMu.Lock()
	remaining := len(srv.stepScopes)
	srv.stepScopesMu.Unlock()
	if remaining != 0 {
		t.Errorf("expired scopes must be reaped lazily, %d remain", remaining)
	}
}

// TestAuthenticateSandboxForwardProxy_ExpiredStepScope407 proves an expired
// step-scope credential is a 407 at the proxy auth boundary when a daemon
// token is configured (never a silent unscoped pass).
func TestAuthenticateSandboxForwardProxy_ExpiredStepScope407(t *testing.T) {
	srv := &apiServer{localDaemonToken: "daemon-token"}
	minted := mintStepScope(t, srv, "session-123", "extract", []string{"api.example.com"})
	srv.stepScopesMu.Lock()
	for id, scope := range srv.stepScopes {
		scope.ExpiresAt = time.Now().Add(-time.Second)
		srv.stepScopes[id] = scope
	}
	srv.stepScopesMu.Unlock()

	req := httptest.NewRequest(http.MethodConnect, "https://api.example.com:443", nil)
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", minted.Token))
	rec := httptest.NewRecorder()
	if _, ok := srv.authenticateSandboxForwardProxy(rec, req); ok {
		t.Fatal("an expired step scope must not authenticate")
	}
	if rec.Code != http.StatusProxyAuthRequired {
		t.Errorf("status = %d, want 407", rec.Code)
	}
}

// stepScopeProxySetup wires a fake TLS upstream, an apiServer with the
// step-scope registry, and the real CONNECT/TLS forward proxy, mirroring
// newPassthroughIntegrationSetup but exposing the apiServer so the test can
// mint step scopes against it. sessionID names the boot session whose CA is
// written to the state dir.
type stepScopeProxySetup struct {
	srv         *apiServer
	proxyURL    *url.URL
	auditStore  *audit.MemStore
	roots       *x509.CertPool
	logicalHost string
}

func newStepScopeProxySetup(t *testing.T, sessionID string, upstreamHandler http.Handler) *stepScopeProxySetup {
	t.Helper()

	upstream := httptest.NewTLSServer(upstreamHandler)
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	const logicalHost = "api.step-scope.test"
	const logicalHostPort = logicalHost + ":443"

	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, sessionID)

	outboundRoots := x509.NewCertPool()
	outboundRoots.AddCert(upstream.Certificate())
	outboundTransport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == logicalHostPort {
				addr = upstreamURL.Host
			}
			d := net.Dialer{}
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig: upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
	}
	outboundTransport.TLSClientConfig.RootCAs = outboundRoots
	outboundTransport.TLSClientConfig.InsecureSkipVerify = true

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-step-scope" }),
		specLoader:           func() ([]connectorspec.Spec, error) { return nil, nil },
		sandboxProxyClient:   &http.Client{Transport: outboundTransport},
	}

	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	t.Cleanup(proxy.Close)

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	return &stepScopeProxySetup{
		srv:         srv,
		proxyURL:    proxyURL,
		auditStore:  auditStore,
		roots:       roots,
		logicalHost: logicalHost,
	}
}

// clientWithScope builds an HTTPS client that CONNECTs through the proxy
// authenticated as <sessionID>:<scopeToken>.
func (s *stepScopeProxySetup) clientWithScope(t *testing.T, sessionID, scopeToken string) *http.Client {
	t.Helper()
	client := newSandboxForwardProxyClientFromProxyURL(t, s.proxyURL, s.roots)
	client.Transport.(*http.Transport).ProxyConnectHeader = http.Header{
		"Proxy-Authorization": []string{basicProxyAuth(sessionID, scopeToken)},
	}
	return client
}

// TestSandboxForwardProxy_StepScopedCredentialEnforcesSealedReach is contract
// regression test 1 for #1829 (in-process, no Docker): a client dialing the
// real forward proxy with a daemon-minted step-scoped credential
//
//  1. reaches its declared host (the passthrough upstream observes the
//     request, and CA continuity holds — the MITM leaf chains to the boot
//     session CA the client trusts), and
//  2. is refused 403 at the proxy for an undeclared host BEFORE any TLS
//     handshake, with a sandbox.proxy.trust_denied audit event carrying
//     reason step_scope_host_denied.
//
// On origin/main this fails: no step-scope credential exists, so the scoped
// CONNECT is refused 407 at auth and nothing scoped ever reaches upstream.
func TestSandboxForwardProxy_StepScopedCredentialEnforcesSealedReach(t *testing.T) {
	const sessionID = "flightplan-boot-scope"
	var upstreamHits int
	setup := newStepScopeProxySetup(t, sessionID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scoped":"ok"}`))
	}))

	minted := mintStepScope(t, setup.srv, sessionID, "extract", []string{setup.logicalHost})
	client := setup.clientWithScope(t, sessionID, minted.Token)

	// (1) Declared host: the scoped CONNECT succeeds end-to-end through the
	// MITM + passthrough path.
	resp, err := client.Get("https://" + setup.logicalHost + "/data")
	if err != nil {
		t.Fatalf("GET through step-scoped proxy to the declared host: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"scoped":"ok"}` {
		t.Fatalf("declared-host response = %d %q, want 200 with the upstream body", resp.StatusCode, string(body))
	}
	if upstreamHits != 1 {
		t.Fatalf("upstream hits = %d, want 1", upstreamHits)
	}

	// (2) Undeclared host: refused 403 at CONNECT time, before any TLS
	// handshake — the upstream is never dialed.
	_, err = client.Get("https://api.undeclared.test/data")
	if err == nil {
		t.Fatal("a scoped CONNECT to an undeclared host must be refused")
	}
	// Go's transport surfaces a refused CONNECT as the proxy's status text.
	if !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("undeclared-host error = %v, want a 403 Forbidden CONNECT refusal", err)
	}
	if upstreamHits != 1 {
		t.Errorf("upstream hits = %d after the denied CONNECT, want still 1", upstreamHits)
	}

	// The denial is audited: sandbox.proxy.trust_denied with the stable
	// step-scope reason, the step id, and the target host — never the scope
	// token or the sealed host list.
	events, err := setup.auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var denied *audit.Event
	for i := range events {
		if events[i].EventType == model.EventTypeSandboxProxyTrustDenied {
			if denied != nil {
				t.Fatal("more than one trust_denied event emitted")
			}
			denied = &events[i]
		}
	}
	if denied == nil {
		t.Fatalf("no sandbox.proxy.trust_denied event emitted; events=%+v", events)
	}
	if got := denied.Payload["aileron.proxy.reject_reason"]; got != sandboxProxyStepScopeReasonHostDenied {
		t.Errorf("reject_reason = %v, want %s", got, sandboxProxyStepScopeReasonHostDenied)
	}
	if got := denied.Payload["aileron.proxy.upstream.host"]; got != "api.undeclared.test" {
		t.Errorf("upstream.host = %v, want api.undeclared.test", got)
	}
	if got := denied.Payload["aileron.step.id"]; got != "extract" {
		t.Errorf("step.id = %v, want extract", got)
	}
	payloadJSON, _ := json.Marshal(denied.Payload)
	if strings.Contains(string(payloadJSON), minted.Token) {
		t.Errorf("audit payload leaked the scope token: %s", payloadJSON)
	}
}

// TestSandboxForwardProxy_ReleasedScopeNoLongerConnects proves the
// release-after-step contract: once the runtime DELETEs the scope, the same
// credential is refused 407 even for the previously declared host.
func TestSandboxForwardProxy_ReleasedScopeNoLongerConnects(t *testing.T) {
	const sessionID = "flightplan-boot-release"
	setup := newStepScopeProxySetup(t, sessionID, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	minted := mintStepScope(t, setup.srv, sessionID, "extract", []string{setup.logicalHost})

	rec := httptest.NewRecorder()
	setup.srv.DeleteSandboxProxyStepScope(rec, httptest.NewRequest(http.MethodDelete, "/v1/sandbox-proxy/step-scopes/"+minted.ScopeId, nil), minted.ScopeId)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("release status = %d, want 204", rec.Code)
	}

	client := setup.clientWithScope(t, sessionID, minted.Token)
	if _, err := client.Get("https://" + setup.logicalHost + "/data"); err == nil {
		t.Fatal("a released scope must no longer authenticate the CONNECT")
	} else if !strings.Contains(err.Error(), "Proxy Authentication Required") {
		t.Errorf("error = %v, want a 407 Proxy Authentication Required refusal", err)
	}
}
