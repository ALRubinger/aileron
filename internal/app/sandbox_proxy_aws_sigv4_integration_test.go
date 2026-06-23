//go:build integration_sandbox

// AWS-CLI SigV4 end-to-end coverage for the v4 sandbox HTTPS proxy.
//
// This test runs a real `aws` host subprocess against an in-process
// proxy + a SigV4-validating fake upstream registered behind a
// `sigv4-resign` host binding. It proves the load-bearing end-to-end
// contract for issue #1505: a genuine `aws` CLI invocation through the
// sealed proxy produces a valid AWS Signature Version 4 that the upstream
// cryptographically accepts, while the secret access key never enters the
// CLI's environment, argv, or any observable upstream surface.
//
// Unlike the in-process binding-layer proof
// (TestSandboxForwardProxy_HostBindingSigV4ResignInjectsSignature, which
// drives a mock transport), this test drives the real `aws` client as a
// host subprocess so the env-driven configuration path (HTTPS_PROXY +
// AWS_CA_BUNDLE) is exercised exactly as it is inside the sandbox
// container. It mirrors the curl integration test's scaffold (state dir +
// generated session CA + logical-host DialContext redirect + fake HTTPS
// upstream) and adds a SigV4-validating upstream: the upstream recomputes
// the canonical request and signature from what it received, using the
// same secret access key the daemon vault holds, and asserts the recomputed
// signature matches the one the proxy emitted. A matching signature proves
// a genuine valid SigV4 reached upstream, not merely a 200.
//
// The test is gated behind the `integration_sandbox` build tag so it does
// not run during the normal `task test:go` suite. Run with:
//
//	task test:integration:sandbox-proxy-clients
//
// `aws` availability: per repo policy (no skip-when-unsupported), the test
// FAILS (t.Fatalf) if `aws` is not on PATH rather than skipping. The
// dedicated task target documents the prerequisite.
package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
)

func TestSandboxProxyAWSSigV4Integration_RealClientProducesValidSignatureAndSeals(t *testing.T) {
	awsPath, err := exec.LookPath("aws")
	if err != nil {
		// Fail-fast, not skip: repo policy forbids skip-when-unsupported.
		// The task target test:integration:sandbox-proxy-clients documents
		// `aws` as a prerequisite.
		t.Fatalf("aws not on PATH; install the AWS CLI to run this integration test: %v", err)
	}

	// The secret access key the daemon vault holds. The `aws` subprocess
	// is configured with a DIFFERENT placeholder secret; the test asserts
	// this real secret never appears in the subprocess env or argv, and
	// that the upstream nonetheless validates a signature computed with it.
	const secretAccessKey = "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"
	const accessKeyID = "AKIDEXAMPLE"
	const region = "us-east-1"
	const service = "s3"

	// Placeholder static credentials handed to the `aws` subprocess. These
	// are NOT the real secret; the daemon re-signs at egress with the vault
	// secret above. The placeholder must be a syntactically plausible value
	// so botocore signs the request locally before it reaches the proxy.
	const placeholderAccessKeyID = "AKIAIOSFODNN7PLACEHLDR"
	const placeholderSecretKey = "placeholderAileronInjectsRealSecretXXXXXX"

	// SigV4-validating upstream. It recomputes the canonical request and
	// signature from what it received using the real secret access key and
	// asserts a match. It also records every request surface so the test
	// can assert the secret never appears anywhere.
	type upstreamCapture struct {
		auth        string
		amzDate     string
		contentHash string
		sigValid    bool
		credScope   string
		surfaces    []string // headers, query, body — every byte the upstream saw
	}
	var cap upstreamCapture
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.auth = r.Header.Get("Authorization")
		cap.amzDate = r.Header.Get(headerAmzDate)
		cap.contentHash = r.Header.Get(headerAmzContentSha256)

		// Record every observable surface for the no-leak assertion.
		for k, vs := range r.Header {
			for _, v := range vs {
				cap.surfaces = append(cap.surfaces, k+": "+v)
			}
		}
		cap.surfaces = append(cap.surfaces, "query: "+r.URL.RawQuery)
		body, _ := io.ReadAll(r.Body)
		cap.surfaces = append(cap.surfaces, "body: "+string(body))

		// Validate the SigV4 signature: recompute from the received request
		// using the real secret and compare to the emitted Signature=.
		valid, scope := validateSigV4(r, body, []byte(secretAccessKey), accessKeyID, region, service)
		cap.sigValid = valid
		cap.credScope = scope

		// Return a minimal, valid s3api list-buckets XML body so the aws
		// client treats the call as a success.
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

	// The logical host the aws client targets. The proxy intercepts the
	// CONNECT to logicalHost:443 and dials the loopback upstream via the
	// outbound transport's DialContext redirect.
	const logicalHost = "s3.amazonaws.test"
	const logicalHostPort = logicalHost + ":443"

	stateDir := t.TempDir()
	caPEM := writeSandboxProxyTestCA(t, stateDir, "session-123")

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

	// Build the apiServer with the secret in the vault under user/aws and a
	// sigv4-resign host binding carrying only the non-secret access-key-id,
	// region, and service. This mirrors the in-process happy-path test's
	// wiring exactly.
	auditStore := audit.NewMemStore()
	srv := &apiServer{
		localDaemonToken:     "daemon-token",
		sandboxProxyStateDir: stateDir,
		auditStore:           auditStore,
		auditRecorder:        audit.NewRecorder(auditStore, nil, func() string { return "audit-aws-sigv4-integration" }),
		specLoader:           func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:                mustVaultWith(t, "user/aws", "user", []byte(secretAccessKey)),
		hostBindings: mustHostBindingTableOpts(t, logicalHost, "user/aws", "sigv4-resign",
			binding.WithSigV4Resign(accessKeyID, region, service)),
		sandboxProxyClient: &http.Client{Transport: outboundTransport},
	}

	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	defer proxy.Close()

	// Write the session CA to a tempfile for AWS_CA_BUNDLE.
	caBundleDir := t.TempDir()
	caBundle := filepath.Join(caBundleDir, "aileron-session-ca.pem")
	if err := os.WriteFile(caBundle, caPEM, 0o600); err != nil {
		t.Fatalf("write aws ca bundle: %v", err)
	}

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	// The proxy URL must carry the session/daemon-token basic auth the
	// CONNECT handshake requires. botocore reads HTTPS_PROXY including
	// userinfo and sends it as Proxy-Authorization.
	proxyWithAuth := *proxyURL
	proxyWithAuth.User = url.UserPassword("session-123", "daemon-token")

	// Resolve the logical host to the proxy's loopback address via a hosts
	// override so botocore's CONNECT target reaches the in-process proxy
	// without a real DNS entry. botocore honors HTTPS_PROXY for the CONNECT
	// regardless, but the endpoint URL host must resolve for the client to
	// proceed; we point it at the proxy host's loopback IP.
	endpoint := "https://" + logicalHostPort

	// Build aws argv. s3api list-buckets is a deterministic GET with no
	// body. --endpoint-url targets the logical host; --region pins the
	// signing region. No real secret on argv.
	args := []string{
		"s3api", "list-buckets",
		"--endpoint-url", endpoint,
		"--region", region,
		"--output", "json",
	}
	for _, arg := range args {
		if strings.Contains(arg, secretAccessKey) {
			t.Fatalf("aws argv leaked the real secret access key: %v", args)
		}
	}

	// Build the subprocess env: ONLY HTTPS_PROXY, AWS_CA_BUNDLE, region,
	// and the placeholder static creds. Never the real secret. A throwaway
	// HOME/config dir keeps the run hermetic (no ambient ~/.aws profile).
	awsHome := t.TempDir()
	env := []string{
		"HTTPS_PROXY=" + proxyWithAuth.String(),
		"https_proxy=" + proxyWithAuth.String(),
		"AWS_CA_BUNDLE=" + caBundle,
		"AWS_ACCESS_KEY_ID=" + placeholderAccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + placeholderSecretKey,
		"AWS_DEFAULT_REGION=" + region,
		"AWS_REGION=" + region,
		"HOME=" + awsHome,
		"AWS_CONFIG_FILE=" + filepath.Join(awsHome, "config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(awsHome, "credentials"),
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_PAGER=",
		// PATH so aws can find its bundled python interpreter where needed.
		"PATH=" + os.Getenv("PATH"),
	}
	for _, e := range env {
		if strings.Contains(e, secretAccessKey) {
			t.Fatalf("aws env leaked the real secret access key: %v", e)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, awsPath, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("aws s3api list-buckets: %v\noutput:\n%s", err, string(out))
	}

	// The aws client received and parsed the upstream's XML body into JSON.
	if !strings.Contains(string(out), "aileron-test-owner") {
		t.Fatalf("aws output did not contain expected upstream owner id; output:\n%s", string(out))
	}

	// Signature assertion (the seal): the upstream received a syntactically
	// AND cryptographically valid AWS4-HMAC-SHA256 signature, recomputed
	// with the same secret the daemon holds. This proves a genuine valid
	// SigV4 reached upstream, not merely a 200.
	if !strings.HasPrefix(cap.auth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("upstream Authorization = %q, want AWS4-HMAC-SHA256 prefix", cap.auth)
	}
	if !cap.sigValid {
		t.Fatalf("upstream SigV4 signature did not validate against the daemon's secret; Authorization=%q", cap.auth)
	}
	wantScope := region + "/" + service + "/aws4_request"
	if !strings.HasSuffix(cap.credScope, wantScope) {
		t.Errorf("credential scope = %q, want suffix %q", cap.credScope, wantScope)
	}
	if !strings.Contains(cap.auth, "Credential="+accessKeyID+"/") {
		t.Errorf("Authorization missing Credential=%s/...: %q", accessKeyID, cap.auth)
	}
	if cap.amzDate == "" {
		t.Error("upstream X-Amz-Date is empty")
	}
	if cap.contentHash == "" {
		t.Error("upstream X-Amz-Content-Sha256 is empty")
	}

	// No-leak assertion: the real secret access key appears in NONE of the
	// aws subprocess env, the aws argv, or any upstream request surface
	// (header, query, body). The argv/env were checked above before launch;
	// re-assert the upstream surfaces here.
	for _, s := range cap.surfaces {
		if strings.Contains(s, secretAccessKey) {
			t.Fatalf("upstream request surface leaked the real secret access key: %q", s)
		}
	}
	// The placeholder secret must also never reach the upstream: the proxy
	// strips the client's signature and re-signs, so no placeholder-derived
	// signature survives. (The placeholder is only the secret material for
	// the client-side signature the proxy discards.)
	if strings.Contains(cap.auth, placeholderAccessKeyID) {
		t.Errorf("upstream Authorization carried the placeholder access key id (proxy did not re-sign): %q", cap.auth)
	}

	// Audit assertion: a single sandbox.proxy.binding_injected event with
	// scheme sigv4-resign and the logical host, and no secret bytes in the
	// payload JSON.
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

// SigV4 header names, duplicated here so the test is independent of the
// (unexported) signer's constants. The upstream validator below is a
// from-scratch reimplementation of the SigV4 verification path; it shares
// no code with internal/credential/inject so a bug in the signer cannot
// mask itself in the validator.
const (
	headerAmzDate          = "X-Amz-Date"
	headerAmzContentSha256 = "X-Amz-Content-Sha256"
)

// validateSigV4 recomputes the AWS4-HMAC-SHA256 signature from the request
// the upstream received and compares it to the Signature= field of the
// Authorization header. It returns whether the recomputed signature matches
// and the credential scope string parsed from the header. The recomputation
// uses only the headers listed in SignedHeaders, the request method, the
// canonical URI/query, and the X-Amz-Content-Sha256 payload hash, per the
// SigV4 spec. A match proves the proxy emitted a genuine valid signature
// over exactly what reached the upstream.
func validateSigV4(r *http.Request, body []byte, secretAccessKey []byte, wantAccessKeyID, region, service string) (bool, string) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return false, ""
	}
	rest := strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 ")

	var credential, signedHeaders, signature string
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "Credential="):
			credential = strings.TrimPrefix(part, "Credential=")
		case strings.HasPrefix(part, "SignedHeaders="):
			signedHeaders = strings.TrimPrefix(part, "SignedHeaders=")
		case strings.HasPrefix(part, "Signature="):
			signature = strings.TrimPrefix(part, "Signature=")
		}
	}
	if credential == "" || signedHeaders == "" || signature == "" {
		return false, ""
	}

	// Credential = <accessKeyID>/<date>/<region>/<service>/aws4_request
	credParts := strings.SplitN(credential, "/", 2)
	if len(credParts) != 2 || credParts[0] != wantAccessKeyID {
		return false, credential
	}
	scope := credParts[1] // <date>/<region>/<service>/aws4_request
	scopeParts := strings.Split(scope, "/")
	if len(scopeParts) != 4 {
		return false, scope
	}
	scopeDate := scopeParts[0]
	if scopeParts[1] != region || scopeParts[2] != service || scopeParts[3] != "aws4_request" {
		return false, scope
	}

	xAmzDate := r.Header.Get(headerAmzDate)
	if xAmzDate == "" {
		return false, scope
	}

	// Payload hash: prefer the X-Amz-Content-Sha256 header (what the signer
	// used), falling back to a recompute over the received body.
	payloadHash := r.Header.Get(headerAmzContentSha256)
	if payloadHash == "" {
		sum := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(sum[:])
	}

	// Canonical headers from the SignedHeaders list, reading the request's
	// actual header values. host is sourced from r.Host.
	signedNames := strings.Split(signedHeaders, ";")
	sort.Strings(signedNames)
	var cb strings.Builder
	for _, name := range signedNames {
		cb.WriteString(name)
		cb.WriteByte(':')
		if name == "host" {
			host := r.Host
			if host == "" {
				host = r.URL.Host
			}
			cb.WriteString(strings.TrimSpace(host))
		} else {
			cb.WriteString(strings.TrimSpace(r.Header.Get(name)))
		}
		cb.WriteByte('\n')
	}

	canonicalURI := r.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := canonicalizeQuery(r.URL.RawQuery)

	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI,
		canonicalQuery,
		cb.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		xAmzDate,
		scope,
		hexSHA256Test([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKeyTest(secretAccessKey, scopeDate, region, service)
	want := hex.EncodeToString(hmacSHA256Test(signingKey, []byte(stringToSign)))
	return hmac.Equal([]byte(want), []byte(signature)), scope
}

// canonicalizeQuery returns the SigV4 canonical query string: parameters
// sorted by name then value, each name/value RFC 3986 percent-encoded.
func canonicalizeQuery(raw string) string {
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		pairs = append(pairs, kv{k: uriEncodeTest(name), v: uriEncodeTest(value)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

func uriEncodeTest(s string) string {
	// The query values arrive already percent-encoded from the URL; decode
	// then re-encode for a stable canonical form.
	dec := s
	if u, err := url.QueryUnescape(s); err == nil {
		dec = u
	}
	var b strings.Builder
	for i := 0; i < len(dec); i++ {
		c := dec[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			const hexDigits = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

func deriveSigningKeyTest(secret []byte, date, region, service string) []byte {
	kDate := hmacSHA256Test(append([]byte("AWS4"), secret...), []byte(date))
	kRegion := hmacSHA256Test(kDate, []byte(region))
	kService := hmacSHA256Test(kRegion, []byte(service))
	return hmacSHA256Test(kService, []byte("aws4_request"))
}

func hmacSHA256Test(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256Test(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
