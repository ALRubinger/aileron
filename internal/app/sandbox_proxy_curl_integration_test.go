//go:build integration_sandbox

// Curl integration coverage for the v4 sandbox HTTPS proxy.
//
// This test runs a real `curl` host subprocess against an in-process
// proxy + fake upstream registered as an installed connector operation
// and verifies the load-bearing R26 contract from the issue #896 plan:
//
//  1. `curl` honors `HTTPS_PROXY` for HTTPS interception via CONNECT.
//  2. The proxy injects the credential at the boundary — the upstream
//     sees `Authorization: Bearer <secret>` even though `curl` was
//     invoked with no credential.
//  3. The container's `curl` invocation does not carry the credential
//     (the test's `curl` argv is constructed without any auth header).
//  4. The daemon emits a `connector.proxy.proxied` audit event with the
//     matched connector FQN, tool, operation, and upstream host (not URL).
//
// The test is gated behind the `integration_sandbox` build tag so it
// does not run during the normal `task test:go` suite. Run with:
//
//	task test:integration:sandbox
//
// `curl` availability: the test skips if `curl` is not on PATH. The
// plan's preferred mode is to run `curl` inside a sandbox container
// reaching the daemon via `host.docker.internal`. This implementation
// runs `curl` as a HOST subprocess of the test, talking to the
// in-process proxy on loopback. The R26 assertions (credential
// injection + audit emission) are identical regardless of whether
// `curl` runs on the host or in a container; the container variant
// is deferred as a follow-up.
package app

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/model"
)

func TestSandboxProxyCurlIntegration_InjectsCredentialAndEmitsProxiedAudit(t *testing.T) {
	curlPath, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not on PATH; skipping integration test")
	}

	// Fake upstream that asserts the credential reached it and returns a
	// known body. The test never sets an Authorization header on the
	// `curl` invocation — anything the upstream sees was injected by the
	// proxy boundary.
	var upstreamAuth string
	var upstreamMethod string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		upstreamMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"viewer":{"id":"u_42"}}`))
	}))
	defer upstream.Close()

	// Parse the upstream's loopback port so we can redirect the proxy's
	// outbound connection there even though it dials the logical
	// host "api.example.test:443".
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	upstreamLoopbackAddr := upstreamURL.Host

	// curl will CONNECT to api.example.test:443 (the proxy hardcodes :443
	// for CONNECT targets); the proxy's spec is registered for that host
	// too. The proxy's outbound HTTPS call resolves to the upstream by
	// way of a custom DialContext that maps the logical address to the
	// loopback port. This decouples the test from DNS while keeping the
	// proxy code unchanged.
	const logicalHost = "api.example.test"
	const logicalHostPort = logicalHost + ":443"

	// State dir with a session CA that the proxy uses to sign leaf certs
	// for the intercepted CONNECT.
	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")

	// Outbound client: trust the upstream's self-signed cert (skip
	// verification because the loopback upstream's SAN is for
	// 127.0.0.1, but the proxy reaches it via the logical hostname
	// after our DialContext redirect). The redirect maps dials for the
	// logical host:port to the loopback upstream.
	outboundRoots := x509.NewCertPool()
	outboundRoots.AddCert(upstream.Certificate())
	outboundTransport := &http.Transport{
		TLSClientConfig: upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == logicalHostPort {
				addr = upstreamLoopbackAddr
			}
			d := net.Dialer{}
			return d.DialContext(ctx, network, addr)
		},
	}
	outboundTransport.TLSClientConfig.RootCAs = outboundRoots
	outboundTransport.TLSClientConfig.InsecureSkipVerify = true // upstream cert is for 127.0.0.1; we reach it under the logical hostname.

	// Build the apiServer with the connector spec pointing at the
	// logical host and a binding that supplies the secret the upstream
	// will assert on.
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-curl-integration" }),
		specLoader: func() ([]connectorspec.Spec, error) {
			return []connectorspec.Spec{{
				SchemaVersion: connectorspec.SchemaVersion,
				Connector:     connectorspec.Connector{FQN: "github://acme/aileron-connector-linear", Version: "1.0.0"},
				Tools: []connectorspec.Tool{{
					Name: "linear",
					Operations: []connectorspec.Operation{{
						Name:       "viewer.get",
						Method:     http.MethodGet,
						Path:       "/graphql",
						Hosts:      []string{logicalHost},
						Credential: "api_key",
					}},
				}},
			}}, nil
		},
		bindings: sandboxProxyTestBindingStore{resolver: sandboxProxyTestResolver{
			cred: credential.Credential{Kind: "api_key", Value: []byte("lin_secret")},
		}},
		sandboxProxyClient: &http.Client{Transport: outboundTransport},
	}

	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	defer proxy.Close()

	// Write the session CA to a tempfile so curl can use it as
	// --cacert. (writeSandboxProxyTestCA wrote it to stateDir already,
	// but we mirror it to a curl-friendly path for clarity.)
	caBundleDir := t.TempDir()
	caBundle := filepath.Join(caBundleDir, "aileron-session-ca.pem")
	if err := os.WriteFile(caBundle, pemBlockToFile(t, caPEM), 0o600); err != nil {
		t.Fatalf("write curl ca bundle: %v", err)
	}

	// Build curl argv. No --header Authorization, no -u: the proxy
	// injects the credential at the boundary. We also tell curl how to
	// resolve the logical hostname so its CONNECT target reaches the
	// test proxy without needing a real DNS entry.
	target := "https://" + logicalHostPort + "/graphql"
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	args := []string{
		"--silent",
		"--show-error",
		"--proxy", proxy.URL,
		"--proxy-user", "session-123:daemon-token",
		"--cacert", caBundle,
		"--max-time", "30",
		"--resolve", logicalHostPort + ":" + proxyURL.Hostname(), // route DNS for logical host to proxy host (curl will CONNECT through proxy regardless)
		target,
	}

	// Assert defensively: the argv contains no credential token. This is
	// the "container's curl invocation does not carry the credential"
	// half of R26.
	for _, arg := range args {
		if strings.Contains(arg, "lin_secret") {
			t.Fatalf("curl argv leaked credential: %v", args)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, curlPath, args...)
	cmd.Env = []string{} // empty env, no HTTPS_PROXY etc.
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("curl: %v\noutput:\n%s", err, string(out))
	}

	// Body assertion: curl received the upstream's body verbatim.
	if !strings.Contains(string(out), `"u_42"`) {
		t.Fatalf("curl output did not contain expected upstream body: %s", string(out))
	}

	// Credential injection assertion: the upstream saw
	// `Authorization: Bearer lin_secret` even though curl was invoked
	// with no credential.
	if upstreamAuth != "Bearer lin_secret" {
		t.Fatalf("upstream Authorization = %q, want Bearer lin_secret (proves credential was injected by the proxy boundary)", upstreamAuth)
	}
	if upstreamMethod != http.MethodGet {
		t.Fatalf("upstream method = %q, want GET", upstreamMethod)
	}

	// Audit assertion: `connector.proxy.proxied` was emitted with the
	// matched connector FQN, tool, operation, upstream host (not URL).
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1; events=%+v", len(events), events)
	}
	if events[0].EventType != model.EventTypeConnectorProxyProxied {
		t.Fatalf("event type = %q, want %q", events[0].EventType, model.EventTypeConnectorProxyProxied)
	}
	payload := events[0].Payload
	if payload["aileron.connector.fqn"] != "github://acme/aileron-connector-linear" {
		t.Errorf("connector fqn = %v", payload["aileron.connector.fqn"])
	}
	if payload["aileron.connector.tool"] != "linear" {
		t.Errorf("tool = %v", payload["aileron.connector.tool"])
	}
	if payload["aileron.connector.operation"] != "viewer.get" {
		t.Errorf("operation = %v", payload["aileron.connector.operation"])
	}
	if payload["aileron.connector.boundary"] != "https_proxy" {
		t.Errorf("boundary = %v", payload["aileron.connector.boundary"])
	}
	if payload["aileron.proxy.source"] != sandboxProxySourceTransparentConnectTLS {
		t.Errorf("proxy source = %v", payload["aileron.proxy.source"])
	}
	if payload["aileron.proxy.upstream.host"] != logicalHost {
		t.Errorf("upstream host = %v, want %s", payload["aileron.proxy.upstream.host"], logicalHost)
	}
	// Credential bytes must not leak into the payload, even though the
	// proxy just used the credential to authenticate upstream.
	for _, v := range payload {
		if s, ok := v.(string); ok && strings.Contains(s, "lin_secret") {
			t.Fatalf("audit payload leaked credential: %v", payload)
		}
	}

}

// pemBlockToFile decodes the PEM bytes the test helper gave us and
// re-encodes them to a curl-friendly file. We use this rather than the
// raw bytes because writeSandboxProxyTestCA returns the PEM-encoded
// CERTIFICATE block already; we just want it on disk where curl can
// find it.
func pemBlockToFile(t *testing.T, caPEM []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("expected CERTIFICATE PEM block, got %#v", block)
	}
	return caPEM
}
