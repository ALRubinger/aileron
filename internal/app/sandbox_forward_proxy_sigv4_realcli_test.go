package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
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
)

// verifySigV4Upstream is an AWS-faithful upstream: it reconstructs the SigV4
// canonical request from the RECEIVED wire values named in the request's
// SignedHeaders and compares signatures derived from the known secret. On
// mismatch it records the canonical it computed so the caller can diff.
type verifySigV4Upstream struct {
	secret  string
	region  string
	service string

	ok       bool
	received bool
	detail   string
}

func (v *verifySigV4Upstream) handler(okBody string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v.received = true
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")

		var signedList, gotSig, credScope string
		for _, part := range strings.Split(auth, ", ") {
			part = strings.TrimPrefix(part, "AWS4-HMAC-SHA256 ")
			switch {
			case strings.HasPrefix(part, "SignedHeaders="):
				signedList = strings.TrimPrefix(part, "SignedHeaders=")
			case strings.HasPrefix(part, "Signature="):
				gotSig = strings.TrimPrefix(part, "Signature=")
			case strings.HasPrefix(part, "Credential="):
				credScope = strings.TrimPrefix(part, "Credential=")
			}
		}
		if signedList == "" || gotSig == "" || credScope == "" {
			v.detail = fmt.Sprintf("malformed Authorization: %q", auth)
			w.WriteHeader(http.StatusForbidden)
			return
		}

		names := strings.Split(signedList, ";")
		sort.Strings(names)
		var ch strings.Builder
		for _, n := range names {
			var val string
			if n == "host" {
				val = r.Host
			} else {
				val = strings.Join(strings.Fields(strings.TrimSpace(r.Header.Get(n))), " ")
			}
			ch.WriteString(n + ":" + val + "\n")
		}
		payloadHash := sha256.Sum256(body)
		canonical := strings.Join([]string{
			r.Method,
			r.URL.EscapedPath(),
			r.URL.RawQuery,
			ch.String(),
			signedList,
			hex.EncodeToString(payloadHash[:]),
		}, "\n")

		scopeParts := strings.Split(credScope, "/")
		if len(scopeParts) != 5 {
			v.detail = fmt.Sprintf("malformed Credential scope: %q", credScope)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		canonicalHash := sha256.Sum256([]byte(canonical))
		stringToSign := strings.Join([]string{
			"AWS4-HMAC-SHA256",
			r.Header.Get("X-Amz-Date"),
			strings.Join(scopeParts[1:], "/"),
			hex.EncodeToString(canonicalHash[:]),
		}, "\n")

		hm := func(key []byte, msg string) []byte {
			m := hmac.New(sha256.New, key)
			m.Write([]byte(msg))
			return m.Sum(nil)
		}
		kSigning := hm(hm(hm(hm([]byte("AWS4"+v.secret), scopeParts[1]), v.region), v.service), "aws4_request")
		wantSig := hex.EncodeToString(hm(kSigning, stringToSign))

		if wantSig == gotSig {
			v.ok = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, okBody)
			return
		}
		v.detail = fmt.Sprintf(
			"SignatureDoesNotMatch\ncanonical (from received wire values):\n%s\nwant sig %s\ngot sig  %s",
			canonical, wantSig, gotSig)
		w.WriteHeader(http.StatusForbidden)
	}
}

// TestSandboxForwardProxy_SigV4RealCLIThroughProxy reproduces the production
// rung-3 signing path with the REAL AWS CLI as the in-container client: the
// pinned tool image sends a request through HTTPS_PROXY into the real proxy
// middleware (CONNECT + MITM with the session CA), the daemon leg re-signs and
// forwards over a real Transport, and an AWS-faithful upstream verifies the
// signature from received wire values. Gated on docker + the tool image ref in
// AILERON_SIGV4_CLI_IMAGE so CI without the image skips.
func TestSandboxForwardProxy_SigV4RealCLIThroughProxy(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	image := os.Getenv("AILERON_SIGV4_CLI_IMAGE")
	if image == "" {
		t.Skip("AILERON_SIGV4_CLI_IMAGE not set (an image with the aws CLI)")
	}

	const secretAccessKey = "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"
	const accessKeyID = "AKIDEXAMPLE"
	const targetHost = "athena.us-east-1.amazonaws.com"

	verifier := &verifySigV4Upstream{secret: secretAccessKey, region: "us-east-1", service: "athena"}
	upstream := httptest.NewTLSServer(verifier.handler(`{"WorkGroups":[]}`))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	realLeg := &http.Client{Transport: &http.Transport{
		// Pin every dial to the local verifier; TLS against its self-signed cert.
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return tls.Dial("tcp", upstreamURL.Host, &tls.Config{InsecureSkipVerify: true})
		},
	}}

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-sigv4-realcli" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte(secretAccessKey)),
		hostBindings: mustHostBindingTableOpts(t, targetHost, "user/aws", "sigv4-resign",
			binding.WithSigV4Resign(accessKeyID, "us-east-1", "athena")),
		sandboxProxyClient: realLeg,
	}
	// Inline the proxy setup (mirrors newHostBindingProxySetup) so the proxy
	// URL is in hand for the container's HTTPS_PROXY.
	stateDir := t.TempDir()
	writeSandboxProxyTestCA(t, stateDir, "session-123")
	srv.localDaemonToken = "daemon-token"
	srv.sandboxProxyStateDir = stateDir
	proxy := httptest.NewServer(srv.sandboxForwardProxyMiddleware(http.NotFoundHandler()))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(stateDir, "sessions", "session-123", "sandbox-proxy", "ca.pem")

	// The real CLI, exactly as the rung-3 runner launches it: HTTPS_PROXY with
	// the session credentials, the session CA as the trust bundle, placeholder
	// static creds. list-work-groups first; start-query-execution exercises a
	// larger body.
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"list-work-groups", []string{"athena", "list-work-groups", "--region", "us-east-1"}},
		{"start-query-execution", []string{"athena", "start-query-execution", "--region", "us-east-1",
			"--work-group", "wg-x", "--query-string", strings.Repeat("SELECT 'padding' AS c UNION ALL ", 300) + "SELECT 'end'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifier.ok, verifier.received, verifier.detail = false, false, ""
			args := []string{"run", "--rm",
				"--add-host", "host.docker.internal:host-gateway",
				"-v", caPath + ":/proxyca/ca.pem:ro",
				"-e", "HTTPS_PROXY=http://session-123:daemon-token@host.docker.internal:" + proxyURL.Port(),
				"-e", "AWS_CA_BUNDLE=/proxyca/ca.pem",
				"-e", "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7PLACEHLDR",
				"-e", "AWS_SECRET_ACCESS_KEY=placeholder-secret",
				"-e", "AWS_MAX_ATTEMPTS=1",
				"--entrypoint", "aws", image,
			}
			args = append(args, tc.args...)
			cmd := exec.Command("docker", args...)
			out, err := cmd.CombinedOutput()
			if !verifier.received {
				t.Fatalf("upstream never received the request; docker err=%v out:\n%s", err, out)
			}
			if !verifier.ok {
				t.Fatalf("signature verification failed for the real CLI request:\n%s\n\nCLI output:\n%s", verifier.detail, out)
			}
			if err != nil {
				t.Logf("CLI exited non-zero (fake upstream body may not satisfy it): %v\n%s", err, out)
			}
		})
	}
}
