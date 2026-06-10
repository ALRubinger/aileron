package app

import (
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

	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
)

// passthroughIntegrationSetup wires a fake TLS upstream, a daemon
// apiServer with no matching connector spec, and a CONNECT/TLS
// intercepting proxy in front of the daemon. The returned client makes
// HTTPS requests through the proxy to the logical host
// `api.unknown.test:443`; the proxy forwards them to the fake upstream
// via a custom DialContext that maps the logical address to the
// upstream's loopback port.
//
// The shared scaffolding lets each passthrough scenario express only
// the assertions specific to that scenario.
type passthroughIntegrationSetup struct {
	proxyURL    *url.URL
	client      *http.Client
	auditStore  *audit.MemStore
	logicalHost string
}

func newPassthroughIntegrationSetup(t *testing.T, upstreamHandler http.Handler) *passthroughIntegrationSetup {
	t.Helper()

	upstream := httptest.NewTLSServer(upstreamHandler)
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	const logicalHost = "api.unknown.test"
	const logicalHostPort = logicalHost + ":443"

	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")

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
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-passthrough" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			// No specs installed — every request to the proxy goes through the
			// passthrough branch under the credential-injection-only model.
			return nil, nil
		},
		sandboxProxyClient: &http.Client{Transport: outboundTransport},
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

	return &passthroughIntegrationSetup{
		proxyURL:    proxyURL,
		client:      newSandboxForwardProxyClient(t, proxyURL, roots),
		auditStore:  auditStore,
		logicalHost: logicalHost,
	}
}

func TestSandboxForwardProxy_PassthroughGetEchoesUpstreamResponse(t *testing.T) {
	var upstreamMethod, upstreamPath string
	setup := newPassthroughIntegrationSetup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMethod = r.Method
		upstreamPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "passthrough-test")
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))

	resp, err := setup.client.Get("https://" + setup.logicalHost + "/hello")
	if err != nil {
		t.Fatalf("GET through passthrough: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, string(body))
	}
	if upstreamMethod != http.MethodGet {
		t.Errorf("upstream method = %q, want GET", upstreamMethod)
	}
	if upstreamPath != "/hello" {
		t.Errorf("upstream path = %q, want /hello", upstreamPath)
	}
	if string(body) != `{"hello":"world"}` {
		t.Errorf("body = %q, want upstream body verbatim", string(body))
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (upstream header preserved)", got)
	}
	if got := resp.Header.Get("X-Upstream-Marker"); got != "passthrough-test" {
		t.Errorf("X-Upstream-Marker = %q, want passthrough-test (upstream header preserved)", got)
	}

	events, _ := setup.auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1; events=%+v", len(events), events)
	}
	evt := events[0]
	if evt.EventType != model.EventTypeSandboxProxyPassthrough {
		t.Fatalf("event type = %q, want %q", evt.EventType, model.EventTypeSandboxProxyPassthrough)
	}
	if got := evt.Payload["aileron.proxy.upstream.host"]; got != setup.logicalHost {
		t.Errorf("upstream host = %v, want %s", got, setup.logicalHost)
	}
	if got := evt.Payload["aileron.proxy.upstream.path"]; got != "/hello" {
		t.Errorf("upstream path = %v, want /hello", got)
	}
	if got := evt.Payload["aileron.proxy.upstream.status"]; got != http.StatusOK {
		t.Errorf("upstream status = %v, want 200", got)
	}
	if got := evt.Payload["aileron.session.id"]; got != "session-123" {
		t.Errorf("session id = %v, want session-123", got)
	}
	sandboxProxyPassthroughShape.validate(t, evt.Payload)
}

func TestSandboxForwardProxy_PassthroughPostForwardsRequestBody(t *testing.T) {
	var upstreamBody []byte
	var upstreamMethod string
	setup := newPassthroughIntegrationSetup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMethod = r.Method
		upstreamBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	}))

	resp, err := setup.client.Post(
		"https://"+setup.logicalHost+"/echo",
		"application/json",
		strings.NewReader(`{"k":"v"}`),
	)
	if err != nil {
		t.Fatalf("POST through passthrough: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, string(body))
	}
	if upstreamMethod != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", upstreamMethod)
	}
	if string(upstreamBody) != `{"k":"v"}` {
		t.Errorf("upstream received body = %q, want %q", string(upstreamBody), `{"k":"v"}`)
	}
	if string(body) != `{"created":true}` {
		t.Errorf("response body = %q", string(body))
	}

	events, _ := setup.auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if got := events[0].Payload["aileron.proxy.upstream.status"]; got != http.StatusCreated {
		t.Errorf("upstream status = %v, want 201", got)
	}
}

func TestSandboxForwardProxy_PassthroughRecordsUpstreamErrorStatusVerbatim(t *testing.T) {
	setup := newPassthroughIntegrationSetup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`upstream unhappy`))
	}))

	resp, err := setup.client.Get("https://" + setup.logicalHost + "/whatever")
	if err != nil {
		t.Fatalf("GET through passthrough: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if string(body) != "upstream unhappy" {
		t.Errorf("body = %q, want upstream body verbatim", string(body))
	}

	events, _ := setup.auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	evt := events[0]
	if evt.EventType != model.EventTypeSandboxProxyPassthrough {
		t.Fatalf("event type = %q, want passthrough (5xx is still passthrough; the upstream answered)", evt.EventType)
	}
	if got := evt.Payload["aileron.proxy.upstream.status"]; got != http.StatusServiceUnavailable {
		t.Errorf("upstream status = %v, want 503", got)
	}
}

func TestSandboxForwardProxy_PassthroughDoesNotLeakCredentialShapedURL(t *testing.T) {
	setup := newPassthroughIntegrationSetup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))

	// The path itself contains a credential-shaped substring. The audit
	// event must not surface it (the recorder strips query strings and
	// the path is the path only). This is the defensive half of the
	// no-leak invariant — the proxy doesn't add an Authorization header
	// when there's no spec match, and it also doesn't echo the request's
	// own URL into the audit payload.
	resp, err := setup.client.Get("https://" + setup.logicalHost + "/api?token=Bearer%20lin_secret")
	if err != nil {
		t.Fatalf("GET through passthrough: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	events, _ := setup.auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	payloadJSON, _ := json.Marshal(events[0].Payload)
	for _, forbidden := range []string{"Bearer", "lin_secret", "token="} {
		if strings.Contains(string(payloadJSON), forbidden) {
			t.Errorf("audit payload contains forbidden substring %q: %s", forbidden, string(payloadJSON))
		}
	}
}
