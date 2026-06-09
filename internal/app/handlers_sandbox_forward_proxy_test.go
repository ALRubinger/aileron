package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/vault"
)

func TestSandboxForwardProxy_CONNECTRequiresProxyAuthorization(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodConnect, "http://gmail.googleapis.com:443", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Proxy-Authenticate"); got != `Basic realm="aileron sandbox proxy"` {
		t.Fatalf("Proxy-Authenticate = %q", got)
	}
}

func TestSandboxForwardProxy_CONNECTRejectsInvalidToken(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodConnect, "http://gmail.googleapis.com:443", nil)
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "wrong-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSandboxForwardProxy_CONNECTAuthenticatesAndFailsClosedOnMissingSessionCA(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodConnect, "http://gmail.googleapis.com:443", nil)
	req.Host = "gmail.googleapis.com:443"
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Aileron-Session-Id"); got != "session-123" {
		t.Fatalf("X-Aileron-Session-Id = %q, want session-123", got)
	}
	if !strings.Contains(rec.Body.String(), "sandbox proxy session CA is unavailable") {
		t.Fatalf("body = %q, want session CA unavailable message", rec.Body.String())
	}
}

func TestSandboxForwardProxy_CONNECTTerminatesTLSAndFailsClosedAfterDecryptedRequest(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	handler := newSandboxForwardProxyTestHandlerWithStateDir(t, "daemon-token", stateDir)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := newSandboxForwardProxyClient(t, proxyURL, roots)
	resp, err := client.Get("https://api.example.test/v1/resource?secret=not-audited")
	if err != nil {
		t.Fatalf("GET through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Aileron-Session-Id"); got != "session-123" {
		t.Fatalf("X-Aileron-Session-Id = %q, want session-123", got)
	}
	if got := resp.Header.Get("X-Aileron-Proxy-Upstream-Host"); got != "api.example.test" {
		t.Fatalf("X-Aileron-Proxy-Upstream-Host = %q, want api.example.test", got)
	}
}

func TestSandboxForwardProxy_CONNECTRoutesMatchedDecryptedRequestThroughProxyBoundary(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	auditStore := audit.NewMemStore()
	var upstreamMethod, upstreamURL, upstreamAuth string
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-transparent-proxy" }),
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
		bindings: sandboxProxyTestBindingStore{resolver: sandboxProxyTestResolver{cred: credential.Credential{Kind: "api_key", Value: []byte("lin_secret")}}},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamMethod = req.Method
			upstreamURL = req.URL.String()
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	handler := srv.sandboxForwardProxyMiddleware(http.NotFoundHandler())
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := newSandboxForwardProxyClient(t, proxyURL, roots)
	resp, err := client.Get("https://api.example.test/graphql?query=viewer")
	if err != nil {
		t.Fatalf("GET through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", resp.StatusCode, string(body))
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", string(body))
	}
	if upstreamMethod != http.MethodGet {
		t.Fatalf("upstream method = %q, want GET", upstreamMethod)
	}
	if upstreamURL != "https://api.example.test/graphql?query=viewer" {
		t.Fatalf("upstream URL = %q", upstreamURL)
	}
	if upstreamAuth != "Bearer lin_secret" {
		t.Fatalf("upstream Authorization = %q, want bearer token", upstreamAuth)
	}
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeConnectorProxyProxied {
		t.Fatalf("event type = %q, want %q", events[0].EventType, model.EventTypeConnectorProxyProxied)
	}
	payload := events[0].Payload
	if payload["aileron.proxy.source"] != sandboxProxySourceTransparentConnectTLS {
		t.Errorf("proxy source = %v", payload["aileron.proxy.source"])
	}
	if payload["aileron.session.id"] != "session-123" {
		t.Errorf("session id = %v", payload["aileron.session.id"])
	}
	payloadJSON, _ := json.Marshal(payload)
	if strings.Contains(string(payloadJSON), "lin_secret") || strings.Contains(string(payloadJSON), "query=viewer") {
		t.Fatalf("audit payload leaked credential or query string: %s", payloadJSON)
	}
}

func TestSandboxForwardProxy_CONNECTAuthenticatesWithProxyURLUserinfo(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	var upstreamAuth string
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
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
		bindings: sandboxProxyTestBindingStore{resolver: sandboxProxyTestResolver{cred: credential.Credential{Kind: "api_key", Value: []byte("lin_secret")}}},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	defer proxy.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	proxyURL.User = url.UserPassword("session-123", "daemon-token")
	client := newSandboxForwardProxyClientFromProxyURL(t, proxyURL, roots)
	resp, err := client.Get("https://api.example.test/graphql")
	if err != nil {
		t.Fatalf("GET through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if upstreamAuth != "Bearer lin_secret" {
		t.Fatalf("upstream Authorization = %q, want bearer token", upstreamAuth)
	}
}

func TestSandboxForwardProxy_CONNECTNoMatchAuditsUnresolvedRejection(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-transparent-rejected" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			return nil, nil
		},
	}
	handler := srv.sandboxForwardProxyMiddleware(http.NotFoundHandler())
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := newSandboxForwardProxyClient(t, proxyURL, roots)
	resp, err := client.Get("https://api.example.test/v1/resource?secret=not-audited")
	if err != nil {
		t.Fatalf("GET through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyRejected {
		t.Fatalf("event type = %q, want %q", events[0].EventType, model.EventTypeSandboxProxyRejected)
	}
	payload := events[0].Payload
	if payload["aileron.proxy.source"] != sandboxProxySourceTransparentConnectTLS {
		t.Errorf("proxy source = %v", payload["aileron.proxy.source"])
	}
	if payload["aileron.proxy.reject_reason"] != "operation_not_matched" {
		t.Errorf("reject reason = %v", payload["aileron.proxy.reject_reason"])
	}
	if payload["aileron.proxy.upstream.path"] != "/v1/resource" {
		t.Errorf("upstream path = %v", payload["aileron.proxy.upstream.path"])
	}
	payloadJSON, _ := json.Marshal(payload)
	if strings.Contains(string(payloadJSON), "not-audited") {
		t.Fatalf("audit payload leaked query string: %s", payloadJSON)
	}
}

func TestSandboxForwardProxy_CONNECTMatchedRequestReturnsCapturedProxyRejection(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
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
	}
	handler := srv.sandboxForwardProxyMiddleware(http.NotFoundHandler())
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := newSandboxForwardProxyClient(t, proxyURL, roots)
	resp, err := client.Get("https://api.example.test/graphql")
	if err != nil {
		t.Fatalf("GET through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, string(body))
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if !strings.Contains(string(body), "binding store is unavailable") {
		t.Fatalf("body = %s, want binding-store rejection", string(body))
	}
}

func TestSandboxForwardProxy_CONNECTRejectsAmbiguousOperationMatch(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
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
					}, {
						Name:       "issues.search",
						Method:     http.MethodGet,
						Path:       "/graphql",
						Hosts:      []string{"api.example.test"},
						Credential: "api_key",
					}},
				}},
			}}, nil
		},
	}
	handler := srv.sandboxForwardProxyMiddleware(http.NotFoundHandler())
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := newSandboxForwardProxyClient(t, proxyURL, roots)
	resp, err := client.Get("https://api.example.test/graphql")
	if err != nil {
		t.Fatalf("GET through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "matched multiple connector operations") {
		t.Fatalf("body = %s, want ambiguous-match rejection", string(body))
	}
}

func TestSandboxForwardProxy_CONNECTRoutesPOSTRequestBodyToUpstream(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	auditStore := audit.NewMemStore()
	var upstreamMethod, upstreamContentType, upstreamAuth string
	var upstreamBody []byte
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-transparent-proxy-post" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			return []connectorspec.Spec{{
				SchemaVersion: connectorspec.SchemaVersion,
				Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-linear", Version: "1.0.0"},
				Tools: []connectorspec.Tool{{
					Name: "linear",
					Operations: []connectorspec.Operation{{
						Name:       "issues.create",
						Method:     http.MethodPost,
						Path:       "/graphql",
						Hosts:      []string{"api.example.test"},
						Credential: "api_key",
					}},
				}},
			}}, nil
		},
		bindings: sandboxProxyTestBindingStore{resolver: sandboxProxyTestResolver{cred: credential.Credential{Kind: "api_key", Value: []byte("lin_secret")}}},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamMethod = req.Method
			upstreamContentType = req.Header.Get("Content-Type")
			upstreamAuth = req.Header.Get("Authorization")
			if req.Body != nil {
				b, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				upstreamBody = b
			}
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"iss_42"}`)),
			}, nil
		})},
	}
	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	defer proxy.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := newSandboxForwardProxyClient(t, proxyURL, roots)
	requestBody := []byte(`{"title":"investigate flaky test","priority":"high"}`)
	req, err := http.NewRequest(http.MethodPost, "https://api.example.test/graphql", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, string(body))
	}
	if string(body) != `{"id":"iss_42"}` {
		t.Fatalf("body = %q, want upstream response verbatim", string(body))
	}
	if upstreamMethod != http.MethodPost {
		t.Fatalf("upstream method = %q, want POST", upstreamMethod)
	}
	if !bytes.Equal(upstreamBody, requestBody) {
		t.Fatalf("upstream body = %q, want %q", string(upstreamBody), string(requestBody))
	}
	if upstreamContentType != "application/json" {
		t.Fatalf("upstream Content-Type = %q, want application/json", upstreamContentType)
	}
	if upstreamAuth != "Bearer lin_secret" {
		t.Fatalf("upstream Authorization = %q, want credential-injected bearer", upstreamAuth)
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeConnectorProxyProxied {
		t.Fatalf("event type = %q, want %q", events[0].EventType, model.EventTypeConnectorProxyProxied)
	}
	payloadJSON, _ := json.Marshal(events[0].Payload)
	if strings.Contains(string(payloadJSON), "investigate flaky test") || strings.Contains(string(payloadJSON), "lin_secret") {
		t.Fatalf("audit payload leaked body or credential: %s", payloadJSON)
	}
}

func TestSandboxForwardProxy_CONNECTRejectsOversizedDecryptedRequestBody(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	auditStore := audit.NewMemStore()
	var upstreamCalled bool
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-transparent-proxy-oversized" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			return []connectorspec.Spec{{
				SchemaVersion: connectorspec.SchemaVersion,
				Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-linear", Version: "1.0.0"},
				Tools: []connectorspec.Tool{{
					Name: "linear",
					Operations: []connectorspec.Operation{{
						Name:       "issues.create",
						Method:     http.MethodPost,
						Path:       "/graphql",
						Hosts:      []string{"api.example.test"},
						Credential: "api_key",
					}},
				}},
			}}, nil
		},
		bindings: sandboxProxyTestBindingStore{resolver: sandboxProxyTestResolver{cred: credential.Credential{Kind: "api_key", Value: []byte("lin_secret")}}},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamCalled = true
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	defer proxy.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := newSandboxForwardProxyClient(t, proxyURL, roots)
	oversized := bytes.Repeat([]byte("x"), sandboxForwardProxyMaxRequestBytes+1)
	req, err := http.NewRequest(http.MethodPost, "https://api.example.test/graphql", bytes.NewReader(oversized))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 413; body=%s", resp.StatusCode, string(body))
	}
	if upstreamCalled {
		t.Fatal("upstream must not be called when body exceeds limit")
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeConnectorProxyRejected {
		t.Fatalf("event type = %q, want %q (matched operation -> connector-context rejection)", events[0].EventType, model.EventTypeConnectorProxyRejected)
	}
	if got := events[0].Payload["aileron.connector.reject_reason"]; got != "request_body_too_large" {
		t.Fatalf("reject reason = %v, want request_body_too_large", got)
	}
	if got := events[0].Payload["aileron.proxy.source"]; got != sandboxProxySourceTransparentConnectTLS {
		t.Errorf("proxy source = %v, want transparent CONNECT TLS", got)
	}
	if got := events[0].Payload["aileron.connector.operation"]; got != "issues.create" {
		t.Errorf("connector operation = %v, want issues.create (matched before rejection)", got)
	}
}

func TestSandboxForwardProxy_CONNECTPropagatesPATCHContentTypeToUpstream(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	var upstreamMethod, upstreamContentType string
	var upstreamBody []byte
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		specLoader: func() ([]connectorspec.Spec, error) {
			return []connectorspec.Spec{{
				SchemaVersion: connectorspec.SchemaVersion,
				Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-linear", Version: "1.0.0"},
				Tools: []connectorspec.Tool{{
					Name: "linear",
					Operations: []connectorspec.Operation{{
						Name:       "issues.update",
						Method:     http.MethodPatch,
						Path:       "/graphql",
						Hosts:      []string{"api.example.test"},
						Credential: "api_key",
					}},
				}},
			}}, nil
		},
		bindings: sandboxProxyTestBindingStore{resolver: sandboxProxyTestResolver{cred: credential.Credential{Kind: "api_key", Value: []byte("lin_secret")}}},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamMethod = req.Method
			upstreamContentType = req.Header.Get("Content-Type")
			if req.Body != nil {
				b, _ := io.ReadAll(req.Body)
				upstreamBody = b
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	defer proxy.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := newSandboxForwardProxyClient(t, proxyURL, roots)
	requestBody := []byte(`{"state":"resolved"}`)
	req, err := http.NewRequest(http.MethodPatch, "https://api.example.test/graphql", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PATCH through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if upstreamMethod != http.MethodPatch {
		t.Fatalf("upstream method = %q, want PATCH", upstreamMethod)
	}
	if upstreamContentType != "application/json" {
		t.Fatalf("upstream Content-Type = %q, want application/json (passthrough)", upstreamContentType)
	}
	if !bytes.Equal(upstreamBody, requestBody) {
		t.Fatalf("upstream body = %q, want %q", string(upstreamBody), string(requestBody))
	}
}

func TestSandboxForwardProxy_CONNECTRoutesEmptyPUTRequestBody(t *testing.T) {
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")
	var upstreamMethod string
	var upstreamBodyLen int
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		specLoader: func() ([]connectorspec.Spec, error) {
			return []connectorspec.Spec{{
				SchemaVersion: connectorspec.SchemaVersion,
				Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-linear", Version: "1.0.0"},
				Tools: []connectorspec.Tool{{
					Name: "linear",
					Operations: []connectorspec.Operation{{
						Name:       "issues.touch",
						Method:     http.MethodPut,
						Path:       "/graphql",
						Hosts:      []string{"api.example.test"},
						Credential: "api_key",
					}},
				}},
			}}, nil
		},
		bindings: sandboxProxyTestBindingStore{resolver: sandboxProxyTestResolver{cred: credential.Credential{Kind: "api_key", Value: []byte("lin_secret")}}},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamMethod = req.Method
			if req.Body != nil {
				b, _ := io.ReadAll(req.Body)
				upstreamBodyLen = len(b)
			}
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})},
	}
	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	defer proxy.Close()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to append CA cert")
	}
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := newSandboxForwardProxyClient(t, proxyURL, roots)
	req, err := http.NewRequest(http.MethodPut, "https://api.example.test/graphql", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT through sandbox forward proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if upstreamMethod != http.MethodPut {
		t.Fatalf("upstream method = %q, want PUT", upstreamMethod)
	}
	if upstreamBodyLen != 0 {
		t.Fatalf("upstream body length = %d, want 0 (empty body boundary)", upstreamBodyLen)
	}
}

func TestWriteSandboxForwardProxyResponseSanitizesHeaderValues(t *testing.T) {
	var out bytes.Buffer

	writeSandboxForwardProxyResponse(&out, "session\r\nbad", "api.example.test\r\nbad", http.StatusOK, "text/plain\r\nbad", []byte("ok"))

	raw := out.String()
	if strings.Contains(raw, "\r\nbad") {
		t.Fatalf("response contains unsanitized header injection: %q", raw)
	}
	if !strings.Contains(raw, "Content-Type: text/plain  bad\r\n") {
		t.Fatalf("response = %q, want sanitized Content-Type", raw)
	}
	if !strings.Contains(raw, "X-Aileron-Session-Id: session  bad\r\n") {
		t.Fatalf("response = %q, want sanitized session id", raw)
	}
	if !strings.Contains(raw, "X-Aileron-Proxy-Upstream-Host: api.example.test  bad\r\n") {
		t.Fatalf("response = %q, want sanitized upstream host", raw)
	}
}

func TestWriteSandboxForwardProxyResponseDefaults(t *testing.T) {
	var out bytes.Buffer

	writeSandboxForwardProxyResponse(&out, "session-123", "api.example.test", 0, "", nil)

	raw := out.String()
	if !strings.HasPrefix(raw, "HTTP/1.1 500 Internal Server Error\r\n") {
		t.Fatalf("response = %q, want default 500 status", raw)
	}
	if !strings.Contains(raw, "Content-Type: application/octet-stream\r\n") {
		t.Fatalf("response = %q, want default content type", raw)
	}
	if !strings.HasSuffix(raw, "Content-Length: 0\r\n\r\n") {
		t.Fatalf("response = %q, want empty body framing", raw)
	}
}

func TestSandboxForwardProxyUpstreamURL(t *testing.T) {
	req := &http.Request{URL: &url.URL{}}
	upstream, err := sandboxForwardProxyUpstreamURL("api.example.test", req)
	if err != nil {
		t.Fatalf("sandboxForwardProxyUpstreamURL: %v", err)
	}
	if got := upstream.String(); got != "https://api.example.test/" {
		t.Fatalf("upstream = %q, want default root path", got)
	}

	if _, err := sandboxForwardProxyUpstreamURL("%zz", req); err == nil {
		t.Fatal("expected invalid target host to fail URL parsing")
	}
}

func TestReadSandboxForwardProxyRequestBody(t *testing.T) {
	body, contentType, err := readSandboxForwardProxyRequestBody(&http.Request{})
	if err != nil {
		t.Fatalf("nil body read failed: %v", err)
	}
	if body != nil || contentType != "" {
		t.Fatalf("nil body = %q contentType = %q, want empty", body, contentType)
	}

	req := httptest.NewRequest(http.MethodPost, "https://api.example.test/graphql", strings.NewReader(`{"ok":true}`))
	req.Header.Set("Content-Type", " application/json ")
	body, contentType, err = readSandboxForwardProxyRequestBody(req)
	if err != nil {
		t.Fatalf("body read failed: %v", err)
	}
	if string(body) != `{"ok":true}` || contentType != "application/json" {
		t.Fatalf("body = %q contentType = %q", string(body), contentType)
	}

	req = httptest.NewRequest(http.MethodPost, "https://api.example.test/graphql", strings.NewReader(""))
	body, contentType, err = readSandboxForwardProxyRequestBody(req)
	if err != nil {
		t.Fatalf("empty body read failed: %v", err)
	}
	if body != nil || contentType != "" {
		t.Fatalf("empty body = %q contentType = %q, want empty", body, contentType)
	}

	req = httptest.NewRequest(http.MethodPost, "https://api.example.test/graphql", strings.NewReader(strings.Repeat("x", sandboxForwardProxyMaxRequestBytes+1)))
	if _, _, err := readSandboxForwardProxyRequestBody(req); err == nil {
		t.Fatal("expected oversized body to fail")
	}
}

func TestMatchSandboxForwardProxyOperationSpecErrors(t *testing.T) {
	upstream, err := parseSandboxProxyUpstreamURL("https://api.example.test/graphql")
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	srv := &apiServer{specLoader: func() ([]connectorspec.Spec, error) {
		return nil, errors.New("boom")
	}}
	if _, _, err := srv.matchSandboxForwardProxyOperation(http.MethodGet, upstream); err == nil || !strings.Contains(err.Error(), "connector specs unavailable") {
		t.Fatalf("load error = %v, want unavailable", err)
	}

	srv = &apiServer{specLoader: func() ([]connectorspec.Spec, error) {
		return []connectorspec.Spec{{
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: "github://acme/one"},
			Tools:         []connectorspec.Tool{{Name: "linear", Operations: []connectorspec.Operation{{Name: "one"}}}},
		}, {
			SchemaVersion: connectorspec.SchemaVersion,
			Connector:     connectorspec.Connector{FQN: "github://acme/two"},
			Tools:         []connectorspec.Tool{{Name: "linear", Operations: []connectorspec.Operation{{Name: "two"}}}},
		}}, nil
	}}
	if _, _, err := srv.matchSandboxForwardProxyOperation(http.MethodGet, upstream); err == nil || !strings.Contains(err.Error(), "connector specs invalid") {
		t.Fatalf("invalid specs error = %v, want invalid", err)
	}
}

func TestSandboxForwardProxyRejectReasonHelpers(t *testing.T) {
	if got := sandboxForwardProxyMatchErrorReason(nil); got != "" {
		t.Fatalf("nil match error reason = %q, want empty", got)
	}
	if got := sandboxForwardProxyMatchErrorReason(errors.New("connector specs invalid")); got != "connector_specs_invalid" {
		t.Fatalf("invalid match error reason = %q", got)
	}
	if got := sandboxForwardProxyMatchErrorReason(errors.New("connector specs unavailable")); got != "connector_specs_unavailable" {
		t.Fatalf("unavailable match error reason = %q", got)
	}
	if got := sandboxForwardProxyBodyRejectReason(errors.New("sandbox proxy decrypted request body read failed")); got != "request_body_read_failed" {
		t.Fatalf("read failure reason = %q", got)
	}
	if got := sandboxForwardProxyBodyRejectReason(errors.New("sandbox proxy decrypted request body exceeded size limit")); got != "request_body_too_large" {
		t.Fatalf("size failure reason = %q", got)
	}
}

func TestSandboxForwardProxy_CONNECTRejectsInvalidTarget(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Host = "api.example.test:8443"
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSandboxForwardProxy_CONNECTRequiresHijackerAfterSessionCA(t *testing.T) {
	stateDir := t.TempDir()
	writeSandboxProxyTestCA(t, stateDir, "session-123")
	srv := &apiServer{localDaemonToken: "daemon-token", sandboxProxyStateDir: stateDir}
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Host = "api.example.test:443"
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := newNoHijackResponseWriter()

	srv.HandleSandboxForwardProxy(rec, req)

	if rec.code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.code, rec.body.String())
	}
}

func TestSandboxForwardProxyConnectHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Host = "api.example.test:443"
	got, err := sandboxForwardProxyConnectHost(req)
	if err != nil {
		t.Fatalf("sandboxForwardProxyConnectHost: %v", err)
	}
	if got != "api.example.test" {
		t.Fatalf("host = %q, want api.example.test", got)
	}

	req = httptest.NewRequest(http.MethodConnect, "/", nil)
	req.Host = "api.example.test:8443"
	if _, err := sandboxForwardProxyConnectHost(req); err == nil {
		t.Fatal("expected non-443 CONNECT target to be rejected")
	}
}

func TestSandboxForwardProxyCertificateErrors(t *testing.T) {
	srv := &apiServer{}
	if _, err := srv.sandboxForwardProxyCertificate("session-123", "api.example.test"); err == nil {
		t.Fatal("expected missing state dir error")
	}

	srv.sandboxProxyStateDir = t.TempDir()
	if _, err := srv.sandboxForwardProxyCertificate("session-123", "api.example.test"); err == nil {
		t.Fatal("expected missing CA file error")
	}

	root := filepath.Join(srv.sandboxProxyStateDir, "sessions", "session-123", "sandbox-proxy")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create CA dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ca.pem"), []byte("not a cert"), 0o644); err != nil {
		t.Fatalf("write invalid CA cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ca.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write invalid CA key: %v", err)
	}
	if _, err := srv.sandboxForwardProxyCertificate("session-123", "api.example.test"); err == nil {
		t.Fatal("expected invalid CA/key error")
	}
}

func TestSignSandboxForwardProxyLeaf_IncludesIPSAN(t *testing.T) {
	ca, key := sandboxProxyTestCA(t)
	cert, err := signSandboxForwardProxyLeaf(ca, key, "127.0.0.1")
	if err != nil {
		t.Fatalf("signSandboxForwardProxyLeaf: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "127.0.0.1" {
		t.Fatalf("IP SANs = %v, want 127.0.0.1", leaf.IPAddresses)
	}
}

func TestSandboxForwardProxy_NonCONNECTAuditsUnresolvedRejection(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		localDaemonToken: "daemon-token",
		auditRecorder:    audit.NewRecorder(auditStore, nil, func() string { return "audit-non-connect" }),
	}
	handler := srv.sandboxForwardProxyMiddleware(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "https://api.example.test/v1/resource", nil)
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyRejected {
		t.Fatalf("event type = %q, want %q", events[0].EventType, model.EventTypeSandboxProxyRejected)
	}
	if got := events[0].Payload["aileron.proxy.reject_reason"]; got != "non_connect_proxy_request_unsupported" {
		t.Fatalf("reject reason = %v, want non_connect_proxy_request_unsupported", got)
	}
	if got := events[0].Payload["aileron.proxy.upstream.host"]; got != "api.example.test" {
		t.Errorf("upstream host = %v, want api.example.test", got)
	}
}

func TestSandboxForwardProxy_CONNECTSessionCAMissingAuditsUnresolvedRejection(t *testing.T) {
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		localDaemonToken: "daemon-token",
		auditRecorder:    audit.NewRecorder(auditStore, nil, func() string { return "audit-ca-missing" }),
	}
	handler := srv.sandboxForwardProxyMiddleware(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodConnect, "http://gmail.googleapis.com:443", nil)
	req.Host = "gmail.googleapis.com:443"
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EventType != model.EventTypeSandboxProxyRejected {
		t.Fatalf("event type = %q", events[0].EventType)
	}
	if got := events[0].Payload["aileron.proxy.reject_reason"]; got != "session_ca_unavailable" {
		t.Fatalf("reject reason = %v, want session_ca_unavailable", got)
	}
	if got := events[0].Payload["aileron.proxy.upstream.host"]; got != "gmail.googleapis.com" {
		t.Errorf("upstream host = %v, want gmail.googleapis.com", got)
	}
}

func TestSandboxForwardProxy_AbsoluteFormV1PathRejectsWithStructuredError(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodGet, "https://api.example.test/v1/resource", nil)
	req.Header.Set("Proxy-Authorization", basicProxyAuth("session-123", "daemon-token"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Aileron-Session-Id"); got != "session-123" {
		t.Fatalf("X-Aileron-Session-Id = %q, want session-123", got)
	}
	if !strings.Contains(rec.Body.String(), "requires CONNECT for HTTPS interception") {
		t.Fatalf("body = %q, want CONNECT-required message", rec.Body.String())
	}
}

func TestSandboxForwardProxy_DoesNotInterceptNormalV1Routes(t *testing.T) {
	handler := newSandboxForwardProxyTestHandler(t, "daemon-token")
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestParseSandboxProxyAuthorization(t *testing.T) {
	got, err := parseSandboxProxyAuthorization(basicProxyAuth("session-123", "daemon-token"))
	if err != nil {
		t.Fatalf("parseSandboxProxyAuthorization: %v", err)
	}
	if got.SessionID != "session-123" || got.Token != "daemon-token" {
		t.Fatalf("auth = %+v", got)
	}
}

func TestParseSandboxProxyAuthorization_RejectsUnsafeSessionID(t *testing.T) {
	for _, sessionID := range []string{"nested/session", `nested\session`, " session-123", ".", ".."} {
		t.Run(sessionID, func(t *testing.T) {
			if _, err := parseSandboxProxyAuthorization(basicProxyAuth(sessionID, "daemon-token")); err == nil {
				t.Fatal("expected unsafe session id error")
			}
		})
	}
}

func TestParseSandboxProxyAuthorization_RejectsMalformedHeader(t *testing.T) {
	tests := map[string]string{
		"missing":     "",
		"not basic":   "Bearer token",
		"bad base64":  "Basic !!!",
		"missing sep": "Basic " + base64.StdEncoding.EncodeToString([]byte("session-123")),
		"empty user":  "Basic " + base64.StdEncoding.EncodeToString([]byte(":daemon-token")),
	}
	for name, header := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSandboxProxyAuthorization(header); err == nil {
				t.Fatal("expected malformed header error")
			}
		})
	}
}

func newSandboxForwardProxyTestHandler(t *testing.T, token string) http.Handler {
	t.Helper()
	return newSandboxForwardProxyTestHandlerWithStateDir(t, token, "")
}

func newSandboxForwardProxyTestHandlerWithStateDir(t *testing.T, token, stateDir string) http.Handler {
	t.Helper()
	handler, err := NewHandlerWithConfig(slog.New(slog.NewJSONHandler(io.Discard, nil)), Config{
		Vault:                vault.NewMemVault(),
		LocalDaemonToken:     token,
		SandboxProxyStateDir: stateDir,
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	return handler
}

func basicProxyAuth(sessionID, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(sessionID+":"+token))
}

func newSandboxForwardProxyClient(t *testing.T, proxyURL *url.URL, roots *x509.CertPool) *http.Client {
	t.Helper()
	client := newSandboxForwardProxyClientFromProxyURL(t, proxyURL, roots)
	client.Transport.(*http.Transport).ProxyConnectHeader = http.Header{
		"Proxy-Authorization": []string{basicProxyAuth("session-123", "daemon-token")},
	}
	return client
}

func newSandboxForwardProxyClientFromProxyURL(t *testing.T, proxyURL *url.URL, roots *x509.CertPool) *http.Client {
	t.Helper()
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			MinVersion: tls.VersionTLS12,
		},
	}}
}

type noHijackResponseWriter struct {
	header http.Header
	body   strings.Builder
	code   int
}

func newNoHijackResponseWriter() *noHijackResponseWriter {
	return &noHijackResponseWriter{header: http.Header{}, code: http.StatusOK}
}

func (w *noHijackResponseWriter) Header() http.Header {
	return w.header
}

func (w *noHijackResponseWriter) WriteHeader(code int) {
	w.code = code
}

func (w *noHijackResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

func writeSandboxProxyTestCA(t *testing.T, stateDir, sessionID string) []byte {
	t.Helper()
	ca, key := sandboxProxyTestCA(t)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	root := filepath.Join(stateDir, "sessions", sessionID, "sandbox-proxy")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create CA dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ca.pem"), caPEM, 0o644); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(root, "ca.key"), keyPEM, 0o600); err != nil {
		t.Fatalf("write CA key: %v", err)
	}
	return caPEM
}

func sandboxProxyTestCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		t.Fatalf("generate CA serial: %v", err)
	}
	now := time.Now().UTC()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Aileron sandbox session CA",
			Organization: []string{"Aileron"},
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(12 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return ca, key
}
