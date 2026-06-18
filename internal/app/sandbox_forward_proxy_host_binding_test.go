package app

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/vault"
)

// hostBindingProxySetup wires an apiServer fronted by a CONNECT/TLS
// intercepting proxy, with a configurable host-binding table, vault,
// connector specs, and a stub upstream transport. Each scenario sets
// only what it needs.
type hostBindingProxySetup struct {
	srv    *apiServer
	client *http.Client
}

func newHostBindingProxySetup(t *testing.T, srv *apiServer) *hostBindingProxySetup {
	t.Helper()
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	srv.localDaemonToken = "daemon-token"
	srv.sandboxProxyStateDir = stateDir

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
	return &hostBindingProxySetup{
		srv:    srv,
		client: newSandboxForwardProxyClient(t, proxyURL, roots),
	}
}

func mustVaultWith(t *testing.T, name, kind string, value []byte) vault.Vault {
	t.Helper()
	v := vault.NewMemVault()
	if err := v.Put(context.Background(), name, value, vault.Metadata{Type: kind}); err != nil {
		t.Fatalf("vault put: %v", err)
	}
	return v
}

func mustHostBindingTable(t *testing.T, host, ref, scheme string) binding.HostBindings {
	t.Helper()
	hb, err := binding.NewHostBinding(host, ref, scheme)
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	return binding.HostBindings{hb}
}

// TestSandboxForwardProxy_HostBindingInjectsAndSealsCredential is the
// happy path: a request whose host matches a binding gets the bound
// credential injected daemon-side; the upstream sees the bearer token
// but the in-container client never does, and a binding_injected audit
// event is emitted with no secret bytes.
func TestSandboxForwardProxy_HostBindingInjectsAndSealsCredential(t *testing.T) {
	auditStore := audit.NewMemStore()
	var upstreamAuth, upstreamURL string
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-host-binding" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "api_key/github/octocat", "api_key", []byte("hb_secret")),
		hostBindings:  mustHostBindingTable(t, "api.example.test", "api_key/github/octocat", "bearer"),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			upstreamURL = req.URL.String()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.example.test/v1/resource?q=1")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, string(body))
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want upstream body verbatim", string(body))
	}
	if upstreamAuth != "Bearer hb_secret" {
		t.Fatalf("upstream Authorization = %q, want Bearer hb_secret", upstreamAuth)
	}
	if upstreamURL != "https://api.example.test/v1/resource?q=1" {
		t.Errorf("upstream URL = %q", upstreamURL)
	}
	// The in-container client must never see the raw token in any
	// response header or body.
	for k, vs := range resp.Header {
		for _, v := range vs {
			if strings.Contains(v, "hb_secret") {
				t.Errorf("response header %s leaked credential: %q", k, v)
			}
		}
	}
	if strings.Contains(string(body), "hb_secret") {
		t.Error("response body leaked credential")
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	evt := events[0]
	if evt.EventType != model.EventTypeSandboxProxyBindingInjected {
		t.Fatalf("event type = %q, want %q", evt.EventType, model.EventTypeSandboxProxyBindingInjected)
	}
	if got := evt.Payload["aileron.proxy.binding.host"]; got != "api.example.test" {
		t.Errorf("binding.host = %v, want api.example.test", got)
	}
	if got := evt.Payload["aileron.proxy.binding.scheme"]; got != "bearer" {
		t.Errorf("binding.scheme = %v, want bearer", got)
	}
	if got := evt.Payload["aileron.proxy.upstream.status"]; got != http.StatusOK {
		t.Errorf("upstream.status = %v, want 200", got)
	}
	sandboxProxyBindingInjectedShape.validate(t, evt.Payload)
	payloadJSON, _ := json.Marshal(evt.Payload)
	for _, forbidden := range []string{"hb_secret", "api_key/github/octocat", "q=1"} {
		if strings.Contains(string(payloadJSON), forbidden) {
			t.Errorf("audit payload leaked %q: %s", forbidden, payloadJSON)
		}
	}
}

// TestSandboxForwardProxy_HostBindingNonMatchPassesThrough is the
// regression guard: a host that matches no binding passes through
// unchanged, injects nothing, and emits sandbox.proxy.passthrough.
func TestSandboxForwardProxy_HostBindingNonMatchPassesThrough(t *testing.T) {
	auditStore := audit.NewMemStore()
	var upstreamAuth string
	var upstreamBody []byte
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-passthrough" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "api_key/github/octocat", "api_key", []byte("hb_secret")),
		hostBindings:  mustHostBindingTable(t, "bound.example.test", "api_key/github/octocat", "bearer"),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			upstreamBody, _ = io.ReadAll(req.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader("verbatim")),
			}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Post("https://other.example.test/echo", "application/json", strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("POST through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "verbatim" {
		t.Errorf("body = %q, want verbatim", string(body))
	}
	if upstreamAuth != "" {
		t.Errorf("upstream Authorization = %q, want empty (no binding match)", upstreamAuth)
	}
	if string(upstreamBody) != `{"k":"v"}` {
		t.Errorf("upstream body = %q, want byte-for-byte", string(upstreamBody))
	}

	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyPassthrough {
		t.Fatalf("event type = %q, want passthrough", events[0].EventType)
	}
}

// TestSandboxForwardProxy_HostBindingLockedVaultFailsClosed asserts a
// bound host whose vault is locked fails closed: no upstream dial, no
// secret bytes anywhere, a binding_locked_vault rejection, and NOT a
// passthrough.
func TestSandboxForwardProxy_HostBindingLockedVaultFailsClosed(t *testing.T) {
	auditStore := audit.NewMemStore()
	dialed := false
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-locked" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		// LockableVault constructed empty is locked: Get returns
		// vault.ErrCredentialUnavailable.
		vault:        vault.NewLockableVault(),
		hostBindings: mustHostBindingTable(t, "api.example.test", "api_key/github/octocat", "bearer"),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			dialed = true
			t.Errorf("upstream must not be dialed when vault is locked; got %s", req.URL)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.example.test/v1/resource")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 500 {
		t.Fatalf("status = %d, want fail-closed 5xx", resp.StatusCode)
	}
	if dialed {
		t.Error("upstream was dialed despite locked vault")
	}
	if strings.Contains(string(body), "hb_secret") {
		t.Error("response body leaked credential")
	}

	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	evt := events[0]
	if evt.EventType != model.EventTypeSandboxProxyRejected {
		t.Fatalf("event type = %q, want rejected", evt.EventType)
	}
	if got := evt.Payload["aileron.proxy.reject_reason"]; got != hostBindingRejectLockedVault {
		t.Errorf("reject_reason = %v, want %q", got, hostBindingRejectLockedVault)
	}
	payloadJSON, _ := json.Marshal(evt.Payload)
	if strings.Contains(string(payloadJSON), "hb_secret") {
		t.Errorf("audit payload leaked credential: %s", payloadJSON)
	}
}

// TestSandboxForwardProxy_HostBindingMissingCredentialFailsClosed
// asserts a bound host whose credential is absent from the vault fails
// closed without passthrough.
func TestSandboxForwardProxy_HostBindingMissingCredentialFailsClosed(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-missing" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         vault.NewMemVault(), // empty: no entry at the ref path
		hostBindings:  mustHostBindingTable(t, "api.example.test", "api_key/github/octocat", "bearer"),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("upstream must not be dialed when credential is missing; got %s", req.URL)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.example.test/v1/resource")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 500 {
		t.Fatalf("status = %d, want fail-closed 5xx", resp.StatusCode)
	}
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if got := events[0].Payload["aileron.proxy.reject_reason"]; got != hostBindingRejectCredentialUnavailable {
		t.Errorf("reject_reason = %v, want %q", got, hostBindingRejectCredentialUnavailable)
	}
}

// TestSandboxForwardProxy_HostBindingUnsupportedSchemeFailsClosed
// asserts a binding declaring a scheme in the closed set but not yet
// injected by the proxy (e.g. `sigv4-resign`, which the #1194 injector
// enumerates but defers) fails closed rather than dialing upstream with
// an un-injected request.
func TestSandboxForwardProxy_HostBindingUnsupportedSchemeFailsClosed(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-scheme" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "api_key/github/octocat", "api_key", []byte("hb_secret")),
		hostBindings:  mustHostBindingTable(t, "api.example.test", "api_key/github/octocat", "sigv4-resign"),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("upstream must not be dialed for an unsupported scheme; got %s", req.URL)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.example.test/v1/resource")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if got := events[0].Payload["aileron.proxy.reject_reason"]; got != hostBindingRejectUnsupportedScheme {
		t.Errorf("reject_reason = %v, want %q", got, hostBindingRejectUnsupportedScheme)
	}
}

// TestSandboxForwardProxy_HostBindingUpstreamUnreachableFailsClosed
// asserts a transport error after successful injection fails closed
// with a 502 and a binding_upstream_unreachable rejection.
func TestSandboxForwardProxy_HostBindingUpstreamUnreachableFailsClosed(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-unreachable" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "api_key/github/octocat", "api_key", []byte("hb_secret")),
		hostBindings:  mustHostBindingTable(t, "api.example.test", "api_key/github/octocat", "bearer"),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, io.ErrUnexpectedEOF
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.example.test/v1/resource")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if got := events[0].Payload["aileron.proxy.reject_reason"]; got != "binding_upstream_unreachable" {
		t.Errorf("reject_reason = %v, want binding_upstream_unreachable", got)
	}
}

// TestSandboxForwardProxy_HostBindingResponseTooLargeFailsClosed
// asserts the binding path caps the upstream response at the same 4 MiB
// limit as the matched/credentialed connector path and the passthrough
// path, failing closed with a 502.
func TestSandboxForwardProxy_HostBindingResponseTooLargeFailsClosed(t *testing.T) {
	auditStore := audit.NewMemStore()
	big := strings.Repeat("a", (4<<20)+1024)
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-too-large" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "api_key/github/octocat", "api_key", []byte("hb_secret")),
		hostBindings:  mustHostBindingTable(t, "api.example.test", "api_key/github/octocat", "bearer"),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
				Body:       io.NopCloser(strings.NewReader(big)),
			}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.example.test/large")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if got := events[0].Payload["aileron.proxy.reject_reason"]; got != "binding_upstream_response_too_large" {
		t.Errorf("reject_reason = %v, want binding_upstream_response_too_large", got)
	}
}

// TestSandboxForwardProxy_HostBindingRefusesPrivateIPLiteral asserts a
// bound host that is a private IP literal is refused before any dial,
// closing the SSRF vector on the binding path exactly as on passthrough.
func TestSandboxForwardProxy_HostBindingRefusesPrivateIPLiteral(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-ssrf" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "api_key/github/octocat", "api_key", []byte("hb_secret")),
		hostBindings:  mustHostBindingTable(t, "169.254.169.254", "api_key/github/octocat", "bearer"),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("must not dial private IP literal; got %s", req.URL)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://169.254.169.254/latest/meta-data/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if got := events[0].Payload["aileron.proxy.reject_reason"]; got != "passthrough_target_not_allowed" {
		t.Errorf("reject_reason = %v, want passthrough_target_not_allowed", got)
	}
}

// TestSandboxForwardProxy_ConnectorSpecWinsOverHostBinding asserts
// precedence: a request matching BOTH a connector-spec operation and a
// host binding resolves via the connector spec (connector.proxy.proxied,
// not sandbox.proxy.binding_injected).
func TestSandboxForwardProxy_ConnectorSpecWinsOverHostBinding(t *testing.T) {
	auditStore := audit.NewMemStore()
	var upstreamAuth string
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-precedence" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			return []connectorspec.Spec{{
				SchemaVersion: connectorspec.SchemaVersion,
				Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-linear", Version: "1.0.0"},
				Tools: []connectorspec.Tool{{
					Name: "linear",
					Operations: []connectorspec.Operation{{
						Name:       "issues.list",
						Method:     http.MethodGet,
						Path:       "/graphql",
						Hosts:      []string{"api.example.test"},
						Credential: "api_key",
					}},
				}},
			}}, nil
		},
		// Connector-spec credential path uses the bindings store; the
		// host-binding table also matches the same host. Spec must win.
		bindings:     sandboxProxyTestBindingStore{resolver: sandboxProxyTestResolver{cred: credential.Credential{Kind: "api_key", Value: []byte("spec_token")}}},
		vault:        mustVaultWith(t, "api_key/github/octocat", "api_key", []byte("hb_token")),
		hostBindings: mustHostBindingTable(t, "api.example.test", "api_key/github/octocat", "bearer"),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.example.test/graphql?query=viewer")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if upstreamAuth != "Bearer spec_token" {
		t.Fatalf("upstream Authorization = %q, want Bearer spec_token (connector spec wins)", upstreamAuth)
	}
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeConnectorProxyProxied {
		t.Fatalf("event type = %q, want connector.proxy.proxied (spec wins)", events[0].EventType)
	}
}

// TestSandboxForwardProxy_AmbiguousSpecFallsToHostBinding asserts that
// an ambiguous connector-spec match (no unique spec) falls to the
// host-binding table before passthrough.
func TestSandboxForwardProxy_AmbiguousSpecFallsToHostBinding(t *testing.T) {
	auditStore := audit.NewMemStore()
	var upstreamAuth string
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-ambiguous" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			// Two connectors declare the same (method, host, path), so the
			// match is ambiguous and yields no unique spec.
			op := connectorspec.Operation{
				Name:   "issues.list",
				Method: http.MethodGet,
				Path:   "/graphql",
				Hosts:  []string{"api.example.test"},
			}
			return []connectorspec.Spec{
				{
					SchemaVersion: connectorspec.SchemaVersion,
					Connector:     connectorspec.Connector{FQN: "github://acme/connector-a", Version: "1.0.0"},
					Tools:         []connectorspec.Tool{{Name: "a", Operations: []connectorspec.Operation{op}}},
				},
				{
					SchemaVersion: connectorspec.SchemaVersion,
					Connector:     connectorspec.Connector{FQN: "github://acme/connector-b", Version: "1.0.0"},
					Tools:         []connectorspec.Tool{{Name: "b", Operations: []connectorspec.Operation{op}}},
				},
			}, nil
		},
		vault:        mustVaultWith(t, "api_key/github/octocat", "api_key", []byte("hb_token")),
		hostBindings: mustHostBindingTable(t, "api.example.test", "api_key/github/octocat", "bearer"),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.example.test/graphql")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if upstreamAuth != "Bearer hb_token" {
		t.Fatalf("upstream Authorization = %q, want Bearer hb_token (ambiguous spec falls to host binding)", upstreamAuth)
	}
	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyBindingInjected {
		t.Fatalf("event type = %q, want binding_injected", events[0].EventType)
	}
}

// TestSandboxForwardProxy_AmbiguousSpecNoBindingPassesThrough asserts
// that with an ambiguous spec and no host binding for the target, the
// request still falls through to passthrough (preserving prior
// behavior).
func TestSandboxForwardProxy_AmbiguousSpecNoBindingPassesThrough(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-ambiguous-passthrough" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			op := connectorspec.Operation{
				Name:   "issues.list",
				Method: http.MethodGet,
				Path:   "/graphql",
				Hosts:  []string{"api.example.test"},
			}
			return []connectorspec.Spec{
				{
					SchemaVersion: connectorspec.SchemaVersion,
					Connector:     connectorspec.Connector{FQN: "github://acme/connector-a", Version: "1.0.0"},
					Tools:         []connectorspec.Tool{{Name: "a", Operations: []connectorspec.Operation{op}}},
				},
				{
					SchemaVersion: connectorspec.SchemaVersion,
					Connector:     connectorspec.Connector{FQN: "github://acme/connector-b", Version: "1.0.0"},
					Tools:         []connectorspec.Tool{{Name: "b", Operations: []connectorspec.Operation{op}}},
				},
			}, nil
		},
		// Host binding table is empty for this host.
		hostBindings: nil,
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.example.test/graphql")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyPassthrough {
		t.Fatalf("event type = %q, want passthrough", events[0].EventType)
	}
}
