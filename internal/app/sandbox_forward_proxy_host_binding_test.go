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
	return mustHostBindingTableOpts(t, host, ref, scheme)
}

// mustHostBindingTableOpts builds a single-entry binding table, threading
// scheme-specific construction options (e.g. binding.WithSigV4Resign) so a
// scheme that carries non-secret params can be exercised end to end.
func mustHostBindingTableOpts(t *testing.T, host, ref, scheme string, opts ...binding.HostBindingOption) binding.HostBindings {
	t.Helper()
	hb, err := binding.NewHostBinding(host, ref, scheme, opts...)
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

// TestSandboxForwardProxy_HostBindingSigV4ResignInjectsSignature is the
// sigv4-resign happy path: a request whose host matches a sigv4-resign
// binding is re-signed daemon-side with the resolved secret access key.
// The upstream sees a well-formed AWS4-HMAC-SHA256 Authorization header
// plus X-Amz-Date and X-Amz-Content-Sha256; the Credential= field carries
// the non-secret access-key-id and credential scope, but the secret access
// key never appears on any header, body, or audit surface, and a
// binding_injected audit event is emitted with scheme sigv4-resign.
func TestSandboxForwardProxy_HostBindingSigV4ResignInjectsSignature(t *testing.T) {
	auditStore := audit.NewMemStore()
	const secretAccessKey = "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"
	const accessKeyID = "AKIDEXAMPLE"
	var upstreamAuth, upstreamDate, upstreamContentHash, upstreamURL string
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-sigv4" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte(secretAccessKey)),
		hostBindings: mustHostBindingTableOpts(t, "s3.amazonaws.test", "user/aws", "sigv4-resign",
			binding.WithSigV4Resign(accessKeyID, "us-east-1", "s3")),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			upstreamDate = req.Header.Get("X-Amz-Date")
			upstreamContentHash = req.Header.Get("X-Amz-Content-Sha256")
			upstreamURL = req.URL.String()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://s3.amazonaws.test/bucket/key?q=1")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, string(body))
	}
	if !strings.HasPrefix(upstreamAuth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("upstream Authorization = %q, want AWS4-HMAC-SHA256 prefix", upstreamAuth)
	}
	if !strings.Contains(upstreamAuth, "Credential="+accessKeyID+"/") {
		t.Errorf("Authorization missing Credential=%s/...: %q", accessKeyID, upstreamAuth)
	}
	if !strings.Contains(upstreamAuth, "/us-east-1/s3/aws4_request") {
		t.Errorf("Authorization missing credential scope: %q", upstreamAuth)
	}
	if !strings.Contains(upstreamAuth, "SignedHeaders=") || !strings.Contains(upstreamAuth, "Signature=") {
		t.Errorf("Authorization missing SignedHeaders/Signature: %q", upstreamAuth)
	}
	if upstreamDate == "" {
		t.Error("upstream X-Amz-Date is empty")
	}
	if upstreamContentHash == "" {
		t.Error("upstream X-Amz-Content-Sha256 is empty")
	}
	if upstreamURL != "https://s3.amazonaws.test/bucket/key?q=1" {
		t.Errorf("upstream URL = %q", upstreamURL)
	}

	// The secret access key must never appear on any observable surface:
	// not in the signed Authorization header (it is only HMAC key material),
	// nor in any response header or body.
	if strings.Contains(upstreamAuth, secretAccessKey) {
		t.Error("Authorization header leaked the secret access key")
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			if strings.Contains(v, secretAccessKey) {
				t.Errorf("response header %s leaked credential: %q", k, v)
			}
		}
	}
	if strings.Contains(string(body), secretAccessKey) {
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
	if got := evt.Payload["aileron.proxy.binding.scheme"]; got != "sigv4-resign" {
		t.Errorf("binding.scheme = %v, want sigv4-resign", got)
	}
	if got := evt.Payload["aileron.proxy.binding.host"]; got != "s3.amazonaws.test" {
		t.Errorf("binding.host = %v, want s3.amazonaws.test", got)
	}
	sandboxProxyBindingInjectedShape.validate(t, evt.Payload)
	payloadJSON, _ := json.Marshal(evt.Payload)
	if strings.Contains(string(payloadJSON), secretAccessKey) {
		t.Errorf("audit payload leaked the secret access key: %s", payloadJSON)
	}
}

// TestSandboxForwardProxy_HostBindingSigV4MissingSecretFailsClosed asserts
// a sigv4-resign binding whose vault secret is absent fails closed: no
// upstream dial, a binding_credential_unavailable rejection, and not a
// passthrough. The missing-required-param fail-closed mode is covered at
// construction (NewHostBinding rejects it before the table is built).
func TestSandboxForwardProxy_HostBindingSigV4MissingSecretFailsClosed(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-sigv4-missing" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         vault.NewMemVault(), // empty: no entry at the ref path
		hostBindings: mustHostBindingTableOpts(t, "s3.amazonaws.test", "user/aws", "sigv4-resign",
			binding.WithSigV4Resign("AKIDEXAMPLE", "us-east-1", "s3")),
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("upstream must not be dialed when the secret is missing; got %s", req.URL)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://s3.amazonaws.test/bucket/key")
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
