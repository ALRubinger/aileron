//go:build integration_sandbox

// Real-container end-to-end coverage for in-container step egress through the
// ADR-0019 daemon forward proxy (#1769 substrate, #1829 step scoping).
//
// Tool steps run as subprocesses INSIDE the single booted plan container
// (#1829) — there is no per-step sibling container — so what these tests
// exercise is exactly that model's egress path: a containerized process whose
// only network configuration is an HTTPS_PROXY pointing at the daemon forward
// proxy and a mounted session CA trusted via a CA-bundle env var.
//
// Two contracts are proven against a real `docker run`:
//
//  1. TestSandboxProxyToolContainerIntegration (#1769): the container's
//     egress is intercepted, MITM-terminated against the mounted CA, and
//     re-signed by a matched `sigv4-resign` host binding with the operator's
//     vault-bound secret, with NO credential bytes in the image, env, mounts,
//     or args.
//  2. TestSandboxProxyToolStepScopeContainerIntegration (#1829): a
//     containerized client egressing under a daemon-minted STEP-SCOPED proxy
//     credential reaches its declared (sealed) host and is refused 403 at
//     CONNECT time for an undeclared one — the containerized twin of the
//     in-process step-scope contract test.
//
// The containers reach the host-bound in-process proxy through
// host.docker.internal (Docker Desktop, or `--add-host
// host.docker.internal:host-gateway` on Linux). The scaffold mirrors the
// sigv4 test's (state dir + generated session CA + logical-host DialContext
// redirect + fake upstream) and reuses its same-package helpers.
//
// The tests are gated behind the `integration_sandbox` build tag so they do
// not run during the normal `task test:go` suite. Run with:
//
//	task test:integration:sandbox-proxy-tool-container
//
// `docker` availability: per repo policy (no skip-when-unsupported), the tests
// FAIL (t.Fatalf) if `docker` is not on PATH rather than skipping.
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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
)

func TestSandboxProxyToolContainerIntegration(t *testing.T) {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		// Fail-fast, not skip: repo policy forbids skip-when-unsupported. The
		// task target documents `docker` as a prerequisite.
		t.Fatalf("docker not on PATH; install Docker to run this integration test: %v", err)
	}

	// The secret access key the daemon vault holds. The container is configured
	// with a DIFFERENT placeholder secret; the test asserts this real secret
	// never appears in the container env/argv or the upstream surface, yet the
	// upstream validates a signature computed with it.
	const secretAccessKey = "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"
	const accessKeyID = "AKIDEXAMPLE"
	const region = "us-east-1"
	const service = "s3"

	// Placeholder static credentials handed to the container so botocore signs
	// the request locally before it reaches the proxy. NOT the real secret.
	const placeholderAccessKeyID = "AKIAIOSFODNN7PLACEHLDR"
	const placeholderSecretKey = "placeholderAileronInjectsRealSecretXXXXXX"

	const sessionID = "flightplan-tool-integration"

	type upstreamCapture struct {
		auth      string
		sigValid  bool
		credScope string
		surfaces  []string
	}
	// cap is written from the httptest server goroutine and read from the test
	// goroutine after the docker run completes. There is no Go memory-model
	// happens-before edge between the server goroutine's write and the later
	// read (the subprocess exit synchronizes the subprocess, not the in-process
	// handler goroutine), so guard cap with a mutex to keep the test race-free
	// under -race.
	var capMu sync.Mutex
	var cap upstreamCapture
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capMu.Lock()
		cap.auth = r.Header.Get("Authorization")
		for k, vs := range r.Header {
			for _, v := range vs {
				cap.surfaces = append(cap.surfaces, k+": "+v)
			}
		}
		cap.surfaces = append(cap.surfaces, "query: "+r.URL.RawQuery)
		body, _ := io.ReadAll(r.Body)
		cap.surfaces = append(cap.surfaces, "body: "+string(body))

		valid, scope := validateSigV4(r, body, []byte(secretAccessKey), accessKeyID, region, service)
		cap.sigValid = valid
		cap.credScope = scope
		capMu.Unlock()

		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<ListAllMyBucketsResult><Buckets></Buckets>` +
			`<Owner><ID>aileron-test-owner</ID></Owner></ListAllMyBucketsResult>`))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	upstreamLoopbackAddr := upstreamURL.Host

	const logicalHost = "s3.amazonaws.test"
	const logicalHostPort = logicalHost + ":443"

	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, sessionID)

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
	outboundTransport.TLSClientConfig.InsecureSkipVerify = true // upstream cert is for 127.0.0.1; reached under the logical hostname.

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditStore:           auditStore,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-tool-container-integration" }),
		specLoader:           func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:                mustVaultWith(t, "user/aws", "user", []byte(secretAccessKey)),
		hostBindings: mustHostBindingTableOpts(t, logicalHost, "user/aws", "sigv4-resign",
			binding.WithSigV4Resign(accessKeyID, region, service)),
		sandboxProxyClient: &http.Client{Transport: outboundTransport},
	}

	// Bind the proxy on ALL interfaces (not httptest's loopback-only listener)
	// so the container can reach it via host.docker.internal, which on a Linux
	// Docker host resolves to the bridge gateway address rather than the host
	// loopback. The container connects to host.docker.internal:<proxyPort>.
	proxyLn, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	proxyServer := &http.Server{Handler: srv.sandboxForwardProxyMiddleware(http.NotFoundHandler())}
	go func() { _ = proxyServer.Serve(proxyLn) }()
	defer proxyServer.Close()
	proxyPort := proxyLn.Addr().(*net.TCPAddr).Port

	// Write the session CA to a host tempfile to bind-mount into the container.
	caBundleDir := t.TempDir()
	caBundleHost := filepath.Join(caBundleDir, "ca.pem")
	if err := os.WriteFile(caBundleHost, caPEM, 0o644); err != nil {
		t.Fatalf("write ca bundle: %v", err)
	}

	const caContainerPath = "/etc/aileron/proxy/ca.pem"
	proxyURL := (&url.URL{
		Scheme: "http",
		User:   url.UserPassword(sessionID, "daemon-token"),
		Host:   net.JoinHostPort("host.docker.internal", strconv.Itoa(proxyPort)),
	}).String()

	endpoint := "https://" + logicalHostPort

	dockerArgs := []string{
		"run", "--rm",
		"--add-host", "host.docker.internal:host-gateway",
		"-e", "HTTPS_PROXY=" + proxyURL,
		"-e", "https_proxy=" + proxyURL,
		"-e", "AWS_CA_BUNDLE=" + caContainerPath,
		"-e", "AWS_ACCESS_KEY_ID=" + placeholderAccessKeyID,
		"-e", "AWS_SECRET_ACCESS_KEY=" + placeholderSecretKey,
		"-e", "AWS_DEFAULT_REGION=" + region,
		"-e", "AWS_EC2_METADATA_DISABLED=true",
		"-v", caBundleHost + ":" + caContainerPath + ":ro",
		"amazon/aws-cli",
		"s3api", "list-buckets",
		"--endpoint-url", endpoint,
		"--region", region,
		"--output", "json",
	}
	// No-leak precondition: the real secret must never appear on the argv.
	for _, a := range dockerArgs {
		if strings.Contains(a, secretAccessKey) {
			t.Fatalf("docker argv leaked the real secret access key: %v", dockerArgs)
		}
	}

	// A hard deadline bounds a stuck image pull, proxy call, or aws CLI hang so
	// the test fails rather than wedging CI.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, dockerPath, dockerArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker run amazon/aws-cli s3api list-buckets: %v\noutput:\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "aileron-test-owner") {
		t.Fatalf("aws output did not contain the expected upstream owner id; output:\n%s", string(out))
	}

	// Snapshot the upstream capture under the mutex so all assertions read a
	// stable, race-free copy.
	capMu.Lock()
	snap := cap
	capMu.Unlock()

	// The seal: the upstream received a cryptographically valid AWS4-HMAC-SHA256
	// signature computed with the daemon's vault secret, proving the proxy
	// re-signed on the way out.
	if !strings.HasPrefix(snap.auth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("upstream Authorization = %q, want AWS4-HMAC-SHA256 prefix", snap.auth)
	}
	if !snap.sigValid {
		t.Fatalf("upstream SigV4 signature did not validate against the daemon's secret; Authorization=%q", snap.auth)
	}
	wantScope := region + "/" + service + "/aws4_request"
	if !strings.HasSuffix(snap.credScope, wantScope) {
		t.Errorf("credential scope = %q, want suffix %q", snap.credScope, wantScope)
	}
	if !strings.Contains(snap.auth, "Credential="+accessKeyID+"/") {
		t.Errorf("Authorization missing Credential=%s/...: %q", accessKeyID, snap.auth)
	}

	// No-leak: the real secret appears in NO upstream request surface, and the
	// placeholder access key id never reaches upstream (proxy re-signed).
	for _, s := range snap.surfaces {
		if strings.Contains(s, secretAccessKey) {
			t.Fatalf("upstream request surface leaked the real secret: %q", s)
		}
	}
	if strings.Contains(snap.auth, placeholderAccessKeyID) {
		t.Errorf("upstream Authorization carried the placeholder access key id (proxy did not re-sign): %q", snap.auth)
	}

	// Audit: a single sandbox.proxy.binding_injected event, scheme
	// sigv4-resign, host = the logical host, with no secret bytes in the payload.
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1; events=%+v", len(events), events)
	}
	evt := events[0]
	if evt.EventType != model.EventTypeSandboxProxyBindingInjected {
		t.Fatalf("event type = %q, want %q", evt.EventType, model.EventTypeSandboxProxyBindingInjected)
	}
	if got := evt.Payload["aileron.proxy.binding.scheme"]; got != "sigv4-resign" {
		t.Errorf("binding.scheme = %v, want sigv4-resign", got)
	}
	if got := evt.Payload["aileron.proxy.binding.host"]; got != logicalHost {
		t.Errorf("binding.host = %v, want %s", got, logicalHost)
	}
	sandboxProxyBindingInjectedShape.validate(t, evt.Payload)
	payloadJSON, _ := json.Marshal(evt.Payload)
	for _, forbidden := range []string{secretAccessKey, placeholderSecretKey, "user/aws"} {
		if strings.Contains(string(payloadJSON), forbidden) {
			t.Errorf("audit payload leaked %q: %s", forbidden, payloadJSON)
		}
	}
}

// TestSandboxProxyToolStepScopeContainerIntegration is the containerized twin
// of the in-process step-scope contract test (#1829): a real `docker run
// curlimages/curl` egressing under a daemon-minted STEP-SCOPED proxy
// credential (the boot session id as the CONNECT username, the scope token as
// the password, so CA continuity holds against the mounted boot-session CA)
//
//  1. reaches its declared (sealed) host through the MITM + passthrough
//     path, and
//  2. is refused 403 at CONNECT time for an undeclared host, with a
//     sandbox.proxy.trust_denied audit event (reason step_scope_host_denied)
//     and no upstream dial.
func TestSandboxProxyToolStepScopeContainerIntegration(t *testing.T) {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		// Fail-fast, not skip: repo policy forbids skip-when-unsupported.
		t.Fatalf("docker not on PATH; install Docker to run this integration test: %v", err)
	}

	const sessionID = "flightplan-boot-stepscope"
	const logicalHost = "api.step-scope.test"
	const logicalHostPort = logicalHost + ":443"

	var upstreamHits int
	var hitsMu sync.Mutex
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsMu.Lock()
		upstreamHits++
		hitsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scoped":"container-ok"}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, sessionID)

	outboundTransport := &http.Transport{
		TLSClientConfig: upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == logicalHostPort {
				addr = upstreamURL.Host
			}
			d := net.Dialer{}
			return d.DialContext(ctx, network, addr)
		},
	}
	outboundTransport.TLSClientConfig.InsecureSkipVerify = true // upstream cert is for 127.0.0.1; reached under the logical hostname.

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditStore:           auditStore,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-step-scope-container" }),
		specLoader:           func() ([]connectorspec.Spec, error) { return nil, nil },
		sandboxProxyClient:   &http.Client{Transport: outboundTransport},
	}

	// Mint the step scope through the real handler: sealed reach = the
	// declared logical host only.
	minted := mintStepScope(t, srv, sessionID, "extract", []string{logicalHost})

	// Bind the proxy on ALL interfaces so the container reaches it via
	// host.docker.internal.
	proxyLn, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	proxyServer := &http.Server{Handler: srv.sandboxForwardProxyMiddleware(http.NotFoundHandler())}
	go func() { _ = proxyServer.Serve(proxyLn) }()
	defer proxyServer.Close()
	proxyPort := proxyLn.Addr().(*net.TCPAddr).Port

	caBundleDir := t.TempDir()
	caBundleHost := filepath.Join(caBundleDir, "ca.pem")
	if err := os.WriteFile(caBundleHost, caPEM, 0o644); err != nil {
		t.Fatalf("write ca bundle: %v", err)
	}
	const caContainerPath = "/etc/aileron/proxy/ca.pem"

	// The STEP-SCOPED proxy URL: boot session as the username (CA
	// continuity), the minted scope token as the password.
	scopedProxyURL := (&url.URL{
		Scheme: "http",
		User:   url.UserPassword(sessionID, minted.Token),
		Host:   net.JoinHostPort("host.docker.internal", strconv.Itoa(proxyPort)),
	}).String()

	runCurl := func(target string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, dockerPath,
			"run", "--rm",
			"--add-host", "host.docker.internal:host-gateway",
			"-e", "HTTPS_PROXY="+scopedProxyURL,
			"-e", "https_proxy="+scopedProxyURL,
			"-e", "CURL_CA_BUNDLE="+caContainerPath,
			"-v", caBundleHost+":"+caContainerPath+":ro",
			"curlimages/curl",
			"-sS", "--fail-with-body", target,
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// (1) Declared host: the scoped containerized client completes the MITM +
	// passthrough round trip.
	out, err := runCurl("https://" + logicalHost + "/data")
	if err != nil {
		t.Fatalf("scoped curl to the declared host: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, `"scoped":"container-ok"`) {
		t.Fatalf("declared-host output = %q, want the upstream body", out)
	}
	hitsMu.Lock()
	hits := upstreamHits
	hitsMu.Unlock()
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits)
	}

	// (2) Undeclared host: refused 403 at CONNECT time; the upstream is never
	// dialed.
	out, err = runCurl("https://api.undeclared.test/data")
	if err == nil {
		t.Fatalf("scoped curl to an undeclared host must fail; output:\n%s", out)
	}
	if !strings.Contains(out, "403") {
		t.Errorf("undeclared-host output = %q, want a 403 CONNECT refusal", out)
	}
	hitsMu.Lock()
	hits = upstreamHits
	hitsMu.Unlock()
	if hits != 1 {
		t.Errorf("upstream hits = %d after the denied CONNECT, want still 1", hits)
	}

	// The denial is audited with the stable step-scope reason and never the
	// scope token.
	events, err := auditStore.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var denied int
	for _, evt := range events {
		if evt.EventType != model.EventTypeSandboxProxyTrustDenied {
			continue
		}
		denied++
		if got := evt.Payload["aileron.proxy.reject_reason"]; got != "step_scope_host_denied" {
			t.Errorf("reject_reason = %v, want step_scope_host_denied", got)
		}
		if got := evt.Payload["aileron.proxy.upstream.host"]; got != "api.undeclared.test" {
			t.Errorf("upstream.host = %v, want api.undeclared.test", got)
		}
		payloadJSON, _ := json.Marshal(evt.Payload)
		if strings.Contains(string(payloadJSON), minted.Token) {
			t.Errorf("audit payload leaked the scope token: %s", payloadJSON)
		}
	}
	if denied != 1 {
		t.Fatalf("trust_denied events = %d, want exactly 1; events=%+v", denied, events)
	}
}
