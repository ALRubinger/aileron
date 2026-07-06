package app

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/vault"
)

const identityTargetHost = "athena.us-east-1.amazonaws.test"

// setupIdentityProxyServer wires the server's proxy state (session CA,
// daemon token) and returns a proxy client whose CONNECT presents the given
// step-scope token, so the request authenticates as a scoped CONNECT and
// carries the scope's credential identity to egress.
func setupIdentityProxyServer(t *testing.T, srv *apiServer, sessionID, scopeToken string) *http.Client {
	t.Helper()
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, sessionID)
	srv.localDaemonToken = "daemon-token"
	srv.sandboxProxyStateDir = stateDir

	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA cert")
	}
	client := newSandboxForwardProxyClientFromProxyURL(t, proxyURL, roots)
	client.Transport.(*http.Transport).ProxyConnectHeader = http.Header{
		"Proxy-Authorization": []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(sessionID+":"+scopeToken))},
	}
	return client
}

// registerStepScope stores a live step scope directly on the server registry
// (bypassing the mint) so a test can drive a scoped CONNECT, including the
// malformed half-identity a well-behaved mint would reject — the proxy must
// still fail closed defensively. It returns the scope's CONNECT token.
func registerStepScope(srv *apiServer, sessionID, stepID string, hosts []string, kind, label string) string {
	token := "scope-token-" + stepID
	srv.stepScopesMu.Lock()
	if srv.stepScopes == nil {
		srv.stepScopes = map[string]sandboxProxyStepScope{}
	}
	srv.stepScopes["scope-"+stepID] = sandboxProxyStepScope{
		SessionID:      sessionID,
		StepID:         stepID,
		Hosts:          hosts,
		Token:          token,
		ExpiresAt:      time.Now().Add(15 * time.Minute),
		CredentialKind: kind,
		IdentityLabel:  label,
	}
	srv.stepScopesMu.Unlock()
	return token
}

// credentialFromAuthHeader extracts the access key id from a SigV4
// Authorization header's Credential= field: it is the first slash-delimited
// segment. Empty when the header is not a SigV4 signature.
func credentialFromAuthHeader(auth string) string {
	for _, part := range strings.Split(auth, ", ") {
		part = strings.TrimPrefix(part, "AWS4-HMAC-SHA256 ")
		if cred := strings.TrimPrefix(part, "Credential="); cred != part {
			if i := strings.IndexByte(cred, '/'); i >= 0 {
				return cred[:i]
			}
			return cred
		}
	}
	return ""
}

// assertProxyReason asserts the single audit event has the given type and
// reject_reason (reason "" skips the reject_reason check).
func assertProxyReason(t *testing.T, store *audit.MemStore, wantType model.EventType, wantReason string) {
	t.Helper()
	events, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	evt := events[0]
	if evt.EventType != wantType {
		t.Fatalf("event type = %q, want %q", evt.EventType, wantType)
	}
	if wantReason != "" {
		if got := evt.Payload["aileron.proxy.reject_reason"]; got != wantReason {
			t.Errorf("reject_reason = %v, want %q", got, wantReason)
		}
	}
}

func mustSigV4IdentityBinding(t *testing.T, ref, label, accessKeyID string) binding.HostBinding {
	t.Helper()
	hb, err := binding.NewHostBinding("", ref, "sigv4-resign",
		binding.WithIdentity("aws-sigv4", label),
		binding.WithSigV4Resign(accessKeyID, "", ""))
	if err != nil {
		t.Fatalf("NewHostBinding identity %q: %v", label, err)
	}
	return hb
}

// TestSandboxForwardProxy_IdentitySelectsCredentialHappyPath is the happy
// path: a scoped CONNECT whose step scope carries {aws-sigv4, metrics-reader},
// a host-less identity binding for that pair (access key AKIDEXAMPLE, no host
// or region on the binding) → the request is injected with that credential and
// a host-derived SigV4 scope, and a binding_injected audit event is emitted.
func TestSandboxForwardProxy_IdentitySelectsCredentialHappyPath(t *testing.T) {
	auditStore := audit.NewMemStore()
	var upstreamAuth string
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-identity" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte("wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY")),
		hostBindings:  binding.HostBindings{mustSigV4IdentityBinding(t, "user/aws", "metrics-reader", "AKIDEXAMPLE")},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	token := registerStepScope(srv, "session-123", "extract", []string{identityTargetHost}, "aws-sigv4", "metrics-reader")
	client := setupIdentityProxyServer(t, srv, "session-123", token)

	resp, err := client.Get("https://" + identityTargetHost + "/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if got := credentialFromAuthHeader(upstreamAuth); got != "AKIDEXAMPLE" {
		t.Fatalf("injected access key id = %q (auth=%q), want AKIDEXAMPLE", got, upstreamAuth)
	}
	// The signing scope is host-derived (region us-east-1, service athena).
	if !strings.Contains(upstreamAuth, "/us-east-1/athena/aws4_request") {
		t.Errorf("Authorization = %q, want a host-derived us-east-1/athena scope", upstreamAuth)
	}
	assertProxyReason(t, auditStore, model.EventTypeSandboxProxyBindingInjected, "")
}

// TestSandboxForwardProxy_IdentityUnboundFailsClosed is the required
// fail-closed regression: a scoped request carries {aws-sigv4, X} but NO
// identity binding for X exists, while a host-match binding for the SAME host
// DOES exist. The request must fail closed (403), the host-match binding must
// NOT be used, and no upstream dial may occur.
func TestSandboxForwardProxy_IdentityUnboundFailsClosed(t *testing.T) {
	auditStore := audit.NewMemStore()
	// A host-match binding for the target host exists (a decoy). It must never
	// be selected for an identity-carrying scope.
	hostBinding, err := binding.NewHostBinding(identityTargetHost, "user/aws", "sigv4-resign",
		binding.WithSigV4Resign("AKIAHOSTMATCHDECOY00", "us-east-1", "athena"))
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-unbound" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte("wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY")),
		hostBindings:  binding.HostBindings{hostBinding},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("upstream must not be dialed for an unbound identity; got %s", req.URL)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	token := registerStepScope(srv, "session-123", "extract", []string{identityTargetHost}, "aws-sigv4", "unbound-label")
	client := setupIdentityProxyServer(t, srv, "session-123", token)

	resp, err := client.Get("https://" + identityTargetHost + "/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 fail-closed", resp.StatusCode)
	}
	assertProxyReason(t, auditStore, model.EventTypeSandboxProxyRejected, hostBindingRejectIdentityUnbound)
}

// TestSandboxForwardProxy_IdentityIncompleteFailsClosed defends the
// half-identity: a scope with a kind but an empty label (which a well-behaved
// mint rejects, but the proxy must still fail closed) never degrades to a host
// match. A host-match binding exists as a decoy; it must not be dialed.
func TestSandboxForwardProxy_IdentityIncompleteFailsClosed(t *testing.T) {
	auditStore := audit.NewMemStore()
	hostBinding, err := binding.NewHostBinding(identityTargetHost, "user/aws", "sigv4-resign",
		binding.WithSigV4Resign("AKIAHOSTMATCHDECOY00", "us-east-1", "athena"))
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-incomplete" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte("secret")),
		hostBindings:  binding.HostBindings{hostBinding},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("upstream must not be dialed for a half-identity; got %s", req.URL)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	token := registerStepScope(srv, "session-123", "extract", []string{identityTargetHost}, "aws-sigv4", "")
	client := setupIdentityProxyServer(t, srv, "session-123", token)

	resp, err := client.Get("https://" + identityTargetHost + "/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 fail-closed", resp.StatusCode)
	}
	assertProxyReason(t, auditStore, model.EventTypeSandboxProxyRejected, hostBindingRejectIdentityIncomplete)
}

// TestSandboxForwardProxy_NoIdentityScopePreservesHostMatch is the regression
// that the host path is untouched: a scoped request whose scope declares NO
// credential identity selects by host exactly as before and injects.
func TestSandboxForwardProxy_NoIdentityScopePreservesHostMatch(t *testing.T) {
	auditStore := audit.NewMemStore()
	var upstreamAuth string
	hostBinding, err := binding.NewHostBinding(identityTargetHost, "user/aws", "sigv4-resign",
		binding.WithSigV4Resign("AKIAHOSTPATHKEY00000", "us-east-1", "athena"))
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-hostpath" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte("wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY")),
		hostBindings:  binding.HostBindings{hostBinding},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	// A scope with an empty credential identity: the host path applies.
	token := registerStepScope(srv, "session-123", "extract", []string{identityTargetHost}, "", "")
	client := setupIdentityProxyServer(t, srv, "session-123", token)

	resp, err := client.Get("https://" + identityTargetHost + "/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := credentialFromAuthHeader(upstreamAuth); got != "AKIAHOSTPATHKEY00000" {
		t.Fatalf("injected access key id = %q, want the host-match binding's key", got)
	}
	assertProxyReason(t, auditStore, model.EventTypeSandboxProxyBindingInjected, "")
}

// TestSandboxForwardProxy_TwoIdentitiesDisambiguate proves selection is by the
// exact pair: with bindings for {aws-sigv4, metrics-reader} and
// {aws-sigv4, admin} loaded, a scope for admin selects admin's access key id,
// not metrics-reader's.
func TestSandboxForwardProxy_TwoIdentitiesDisambiguate(t *testing.T) {
	auditStore := audit.NewMemStore()
	var upstreamAuth string

	v := mustVaultWith(t, "user/aws-reader", "user", []byte("READERSECRETKEYAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	if err := v.Put(context.Background(), "user/aws-admin", []byte("ADMINSECRETKEYBBBBBBBBBBBBBBBBBBBBBBBBBBB"), vault.Metadata{Type: "user"}); err != nil {
		t.Fatalf("vault put admin: %v", err)
	}

	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-two" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         v,
		hostBindings: binding.HostBindings{
			mustSigV4IdentityBinding(t, "user/aws-reader", "metrics-reader", "AKIAREADERKEY0000000"),
			mustSigV4IdentityBinding(t, "user/aws-admin", "admin", "AKIAADMINKEY00000000"),
		},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	token := registerStepScope(srv, "session-123", "extract", []string{identityTargetHost}, "aws-sigv4", "admin")
	client := setupIdentityProxyServer(t, srv, "session-123", token)

	resp, err := client.Get("https://" + identityTargetHost + "/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := credentialFromAuthHeader(upstreamAuth); got != "AKIAADMINKEY00000000" {
		t.Fatalf("injected access key id = %q, want admin's AKIAADMINKEY00000000", got)
	}
}

// TestSandboxForwardProxy_IdentityInjectedAuditCarriesIdentity pins the
// success-path attribution fix: a host-less identity binding leaves
// aileron.proxy.binding.host blank, so the binding_injected audit event must
// carry aileron.proxy.binding.identity = "<kind>/<label>" for the injection to
// be attributable to an operator credential. The field is the non-secret
// (kind, label) pair, never the credential bytes or the credential-ref.
func TestSandboxForwardProxy_IdentityInjectedAuditCarriesIdentity(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-attr" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte("wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY")),
		hostBindings:  binding.HostBindings{mustSigV4IdentityBinding(t, "user/aws", "metrics-reader", "AKIDEXAMPLE")},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	token := registerStepScope(srv, "session-123", "extract", []string{identityTargetHost}, "aws-sigv4", "metrics-reader")
	client := setupIdentityProxyServer(t, srv, "session-123", token)

	resp, err := client.Get("https://" + identityTargetHost + "/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
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
		t.Fatalf("event type = %q, want binding_injected", evt.EventType)
	}
	// The host-less identity binding leaves binding.host blank; the identity
	// field is what makes the injection attributable.
	if got := evt.Payload["aileron.proxy.binding.host"]; got != "" {
		t.Errorf("binding.host = %v, want empty for a host-less identity binding", got)
	}
	if got := evt.Payload["aileron.proxy.binding.identity"]; got != "aws-sigv4/metrics-reader" {
		t.Errorf("binding.identity = %v, want %q", got, "aws-sigv4/metrics-reader")
	}
}

// TestSandboxForwardProxy_IdentityNonDerivableHostFailsClosed pins the
// mixed-reach forfeiture: a step scope that declares a sigv4-resign identity
// enters the identity path for EVERY in-reach host, so a request to a
// non-derivable (non-AWS) host — whose SigV4 scope cannot be derived and whose
// binding carries no Region/Service fallback — fails closed at injection
// rather than passing through un-injected. injected=false, a protocol_rejected
// audit event is emitted, and no upstream dial occurs.
func TestSandboxForwardProxy_IdentityNonDerivableHostFailsClosed(t *testing.T) {
	const nonDerivableHost = "metrics.example.com"
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-nonderivable" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte("wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY")),
		// A host-less sigv4 identity binding with no Region/Service fallback:
		// its scope is derivable only from an AWS host.
		hostBindings: binding.HostBindings{mustSigV4IdentityBinding(t, "user/aws", "metrics-reader", "AKIDEXAMPLE")},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("upstream must not be dialed when sigv4 injection fails closed; got %s", req.URL)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	// The scope's reach includes the non-derivable host; the identity is
	// matched by (kind, label), not by host, so selection succeeds and the
	// request is driven to injection on a host the scheme cannot serve.
	token := registerStepScope(srv, "session-123", "extract", []string{nonDerivableHost}, "aws-sigv4", "metrics-reader")
	client := setupIdentityProxyServer(t, srv, "session-123", token)

	resp, err := client.Get("https://" + nonDerivableHost + "/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want a 4xx/5xx fail-closed", resp.StatusCode)
	}
	assertProxyReason(t, auditStore, model.EventTypeSandboxProxyRejected, hostBindingRejectUnsupportedScheme)
}

// TestSandboxForwardProxy_IdentityBindingTrustGateUnchanged proves the
// per-step trust-contract gate runs identically on an identity-selected
// binding: a `read`-effect binding still denies a mutating method at
// enforceHostBindingTrust, before any credential is injected or upstream
// dialed. The selection change only alters WHICH binding is chosen; the gate
// after it is untouched.
func TestSandboxForwardProxy_IdentityBindingTrustGateUnchanged(t *testing.T) {
	auditStore := audit.NewMemStore()
	idBinding, err := binding.NewHostBinding("", "user/aws", "sigv4-resign",
		binding.WithIdentity("aws-sigv4", "metrics-reader"),
		binding.WithSigV4Resign("AKIDEXAMPLE", "", ""),
		binding.WithTrustContract(binding.EffectRead, nil))
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-gate" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte("wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY")),
		hostBindings:  binding.HostBindings{idBinding},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("upstream must not be dialed when the read-effect gate denies; got %s", req.URL)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	token := registerStepScope(srv, "session-123", "extract", []string{identityTargetHost}, "aws-sigv4", "metrics-reader")
	client := setupIdentityProxyServer(t, srv, "session-123", token)

	// A POST is a mutating method; the read-effect gate denies it.
	resp, err := client.Post("https://"+identityTargetHost+"/", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (read-effect denies POST)", resp.StatusCode)
	}
	assertProxyReason(t, auditStore, model.EventTypeSandboxProxyTrustDenied, trustRejectEffectNotAllowed)
}
