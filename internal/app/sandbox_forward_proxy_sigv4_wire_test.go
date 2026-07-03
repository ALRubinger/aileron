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
	"sort"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
)

// TestSandboxForwardProxy_SigV4ResignSignatureVerifiesOnTheWire is the
// AWS-side truth test the shape-only assertions above don't provide: the
// re-signed request travels the REAL upstream leg (a real http.Transport
// serializing to a real TLS server), and the upstream verifies the SigV4
// signature the way AWS does — reconstruct the canonical request from the
// RECEIVED headers named in SignedHeaders, derive the signing key from the
// known secret, and compare signatures. A proxy that mutates any signed
// header between signing and the wire (or signs a value the upstream never
// receives) fails here with the exact SignatureDoesNotMatch AWS returns in
// production. The request is botocore-shaped (POST body, content-length,
// accept-encoding, long user-agent, amz-sdk-* headers, a stale client
// Authorization) because that is what the rung-3 AWS-CLI tool sends.
func TestSandboxForwardProxy_SigV4ResignSignatureVerifiesOnTheWire(t *testing.T) {
	const secretAccessKey = "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"
	const accessKeyID = "AKIDEXAMPLE"

	type verdict struct {
		ok       bool
		detail   string
		received bool
	}
	var got verdict

	// The wire-truth upstream: a real TLS server that verifies the signature
	// from what it actually received, exactly as AWS does.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.received = true
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")

		// Parse SignedHeaders and Signature out of the Authorization.
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
		if signedList == "" || gotSig == "" {
			got.detail = fmt.Sprintf("malformed Authorization: %q", auth)
			w.WriteHeader(http.StatusForbidden)
			return
		}

		// Canonicalize from RECEIVED values, per the SigV4 server-side rules.
		names := strings.Split(signedList, ";")
		sort.Strings(names)
		var ch strings.Builder
		for _, n := range names {
			var v string
			if n == "host" {
				v = r.Host
			} else {
				v = strings.TrimSpace(r.Header.Get(n))
			}
			ch.WriteString(n + ":" + v + "\n")
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

		// Scope: AKIDEXAMPLE/<date>/us-east-1/athena/aws4_request
		scopeParts := strings.Split(credScope, "/")
		if len(scopeParts) != 5 {
			got.detail = fmt.Sprintf("malformed Credential scope: %q", credScope)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		scopeDate := scopeParts[1]
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
		kSigning := hm(hm(hm(hm([]byte("AWS4"+secretAccessKey), scopeDate), "us-east-1"), "athena"), "aws4_request")
		wantSig := hex.EncodeToString(hm(kSigning, stringToSign))

		if wantSig == gotSig {
			got.ok = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		got.detail = fmt.Sprintf(
			"SignatureDoesNotMatch:\ncanonical (from received wire values):\n%s\nwant sig %s\ngot sig  %s\nauth %q",
			canonical, wantSig, gotSig, auth)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(upstream.Close)

	// Real upstream leg: a REAL http.Transport (real request serialization)
	// whose dial is pinned to the local TLS upstream regardless of the
	// target host, mirroring "the daemon dials AWS".
	upstreamAddr := upstream.Listener.Addr().String()
	realLeg := &http.Client{Transport: &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return tls.Dial("tcp", upstreamAddr, &tls.Config{InsecureSkipVerify: true})
		},
	}}

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-sigv4-wire" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/aws", "user", []byte(secretAccessKey)),
		hostBindings: mustHostBindingTableOpts(t, "athena.us-east-1.amazonaws.test", "user/aws", "sigv4-resign",
			binding.WithSigV4Resign(accessKeyID, "us-east-1", "athena")),
		sandboxProxyClient: realLeg,
	}
	setup := newHostBindingProxySetup(t, srv)

	// A botocore-shaped request: POST body, stale placeholder signature, the
	// full header set the AWS CLI sends.
	body := `{"QueryString":"SELECT 1","WorkGroup":"wg-x","QueryExecutionContext":{"Database":"db"}}`
	req, err := http.NewRequest(http.MethodPost, "https://athena.us-east-1.amazonaws.test/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AmazonAthena.StartQueryExecution")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "aws-cli/2.35.15 md/awscrt#0.35.0 ua/2.1 os/linux#6.12.76-linuxkit md/arch#aarch64 lang/python#3.14.5")
	req.Header.Set("Amz-Sdk-Invocation-Id", "45b9a48b-67da-4d67-b1b3-a0347fe6f6a7")
	req.Header.Set("Amz-Sdk-Request", "attempt=1")
	req.Header.Set("X-Amz-Date", "20260703T000000Z")
	req.Header.Set("X-Amz-Content-Sha256", "placeholder")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7PLACEHLDR/20260703/us-east-1/athena/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")

	resp, err := setup.client.Do(req)
	if err != nil {
		t.Fatalf("POST through proxy: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if !got.received {
		t.Fatalf("upstream never received the request; proxy status=%d body=%s", resp.StatusCode, respBody)
	}
	if !got.ok {
		t.Fatalf("upstream signature verification failed:\n%s", got.detail)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, respBody)
	}
}
