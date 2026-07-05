package app

// Proxy-boundary regression anchor for issue #1734: strip a client-supplied
// placeholder SigV4 signature and re-sign at the proxy boundary.
//
// The existing sigv4-resign happy-path test
// (TestSandboxForwardProxy_HostBindingSigV4ResignInjectsSignature) sends no
// inbound Authorization, so it never exercises the load-bearing path this
// issue names: a self-signing client (e.g. `aws`/botocore configured with
// placeholder static credentials) that already carries a placeholder
// AWS4-HMAC-SHA256 Authorization header, which the proxy must strip before
// re-signing with the operator's vault-bound secret. The signer-unit test
// TestInjectSigV4ResignDropsClientAuthorization covers inject.Inject in
// isolation; this file drives the full decrypted-request -> host-binding ->
// re-sign flow through the real CONNECT + TLS-intercept + decrypt path.
//
// The upstream is a capturing mock transport with an in-test SigV4 validator
// (recomputed from scratch, independent of the production signer's internals),
// so the happy path is exercised fully and a signer bug cannot mask itself.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/vault"
)

// A syntactically plausible placeholder AWS4-HMAC-SHA256 Authorization header,
// as a self-signing client configured with placeholder static credentials
// would emit. The proxy must drop this entirely before re-signing.
const (
	boundaryPlaceholderAccessKeyID = "AKIAPLACEHOLDER0000000"
	boundaryPlaceholderSignature   = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	boundaryPlaceholderAuth        = "AWS4-HMAC-SHA256 " +
		"Credential=" + boundaryPlaceholderAccessKeyID + "/20250101/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=" + boundaryPlaceholderSignature
)

// TestSandboxForwardProxy_SigV4ResignStripsPlaceholderAtBoundary is the
// #1734 regression anchor. It is table-driven against a capturing fake
// upstream and pins three acceptance rows in one place:
//
//  1. A sigv4-resign request that ALREADY carries a placeholder
//     AWS4-HMAC-SHA256 Authorization header is re-signed at the boundary:
//     the placeholder access-key-id and placeholder Signature= never reach
//     the upstream, the emitted Authorization carries the real access-key-id
//     and a signature that validates against the AWS SigV4 signing contract,
//     and the vault secret never appears on any observable surface. This is
//     the strip-of-placeholder proof the existing tests lack.
//  2. A bearer host binding still injects Authorization: Bearer <secret>
//     unchanged (no regression from the sigv4 path).
//  3. A sigv4-resign binding whose vault secret is absent fails closed with a
//     binding_credential_unavailable rejection, not an unsigned passthrough.
func TestSandboxForwardProxy_SigV4ResignStripsPlaceholderAtBoundary(t *testing.T) {
	const secretAccessKey = "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"
	const realAccessKeyID = "AKIDEXAMPLE"
	const region = "us-east-1"
	const service = "s3"

	tests := []struct {
		name string
		// scheme + binding wiring
		bindingHost   string
		bindingRef    string
		bindingScheme string
		bindingOpts   []binding.HostBindingOption
		// vault contents (nil kind means: do not store, exercise fail-closed)
		vaultName  string
		vaultKind  string
		vaultValue []byte
		// request
		reqURL string
		// an inbound header the in-container client already set
		inboundAuthHeader string
		// expectations
		wantStatus  int
		assertProxy func(t *testing.T, cap capturedUpstream)
		wantReject  string // "" means expect binding_injected success
	}{
		{
			name:          "placeholder-signed sigv4 request is stripped and re-signed",
			bindingHost:   "s3.amazonaws.test",
			bindingRef:    "user/aws",
			bindingScheme: "sigv4-resign",
			bindingOpts:   []binding.HostBindingOption{binding.WithSigV4Resign(realAccessKeyID, region, service)},
			vaultName:     "user/aws",
			vaultKind:     "user",
			vaultValue:    []byte(secretAccessKey),
			reqURL:        "https://s3.amazonaws.test/bucket/key?q=1",
			// The load-bearing addition: the client already carries a
			// placeholder AWS4-HMAC-SHA256 signature.
			inboundAuthHeader: boundaryPlaceholderAuth,
			wantStatus:        http.StatusOK,
			assertProxy: func(t *testing.T, cap capturedUpstream) {
				t.Helper()
				if !strings.HasPrefix(cap.auth, "AWS4-HMAC-SHA256 ") {
					t.Fatalf("upstream Authorization = %q, want AWS4-HMAC-SHA256 prefix", cap.auth)
				}
				// The placeholder must be fully gone: neither the placeholder
				// access-key-id nor the placeholder Signature= survived.
				if strings.Contains(cap.auth, boundaryPlaceholderAccessKeyID) {
					t.Errorf("upstream Authorization carried the placeholder access-key-id (proxy did not strip+re-sign): %q", cap.auth)
				}
				if strings.Contains(cap.auth, boundaryPlaceholderSignature) {
					t.Errorf("upstream Authorization carried the placeholder Signature= (proxy did not strip+re-sign): %q", cap.auth)
				}
				// The real access key id must be present in Credential=.
				if !strings.Contains(cap.auth, "Credential="+realAccessKeyID+"/") {
					t.Errorf("Authorization missing Credential=%s/...: %q", realAccessKeyID, cap.auth)
				}
				// The emitted signature must validate against the AWS SigV4
				// signing contract, recomputed independently from the request
				// the upstream actually received.
				ok, scope := boundaryValidateSigV4(cap.req, cap.body, []byte(secretAccessKey), realAccessKeyID, region, service)
				if !ok {
					t.Errorf("re-signed request did not validate against SigV4 contract (scope=%q, auth=%q)", scope, cap.auth)
				}
				// The vault secret must never appear on any observable surface.
				if strings.Contains(cap.auth, secretAccessKey) {
					t.Error("upstream Authorization leaked the secret access key")
				}
			},
		},
		{
			// Acceptance (#1979): a sigv4-resign binding with EMPTY stored
			// region/service still signs, deriving the SigV4 scope from the
			// resolved upstream host (athena.us-east-1.amazonaws.test ->
			// athena/us-east-1). This is the incremental win: a descriptor no
			// longer needs to carry region/service at all.
			name:          "sigv4-resign derives scope from host when stored region/service are empty",
			bindingHost:   "athena.us-east-1.amazonaws.test",
			bindingRef:    "user/aws",
			bindingScheme: "sigv4-resign",
			bindingOpts:   []binding.HostBindingOption{binding.WithSigV4Resign(realAccessKeyID, "", "")},
			vaultName:     "user/aws",
			vaultKind:     "user",
			vaultValue:    []byte(secretAccessKey),
			reqURL:        "https://athena.us-east-1.amazonaws.test/",
			wantStatus:    http.StatusOK,
			assertProxy: func(t *testing.T, cap capturedUpstream) {
				t.Helper()
				if !strings.Contains(cap.auth, "/us-east-1/athena/aws4_request") {
					t.Errorf("Authorization scope not host-derived: %q", cap.auth)
				}
				ok, scope := boundaryValidateSigV4(cap.req, cap.body, []byte(secretAccessKey), realAccessKeyID, "us-east-1", "athena")
				if !ok {
					t.Errorf("host-derived re-sign did not validate (scope=%q, auth=%q)", scope, cap.auth)
				}
			},
		},
		{
			// Acceptance (#1979): a GovCloud endpoint (us-gov-west-1) signs
			// correctly. The earlier two-word region regex could not recognize
			// the us-gov-west-1 partition; the generalized parser can.
			name:          "sigv4-resign derives GovCloud region from host",
			bindingHost:   "athena.us-gov-west-1.amazonaws.test",
			bindingRef:    "user/aws",
			bindingScheme: "sigv4-resign",
			bindingOpts:   []binding.HostBindingOption{binding.WithSigV4Resign(realAccessKeyID, "", "")},
			vaultName:     "user/aws",
			vaultKind:     "user",
			vaultValue:    []byte(secretAccessKey),
			reqURL:        "https://athena.us-gov-west-1.amazonaws.test/",
			wantStatus:    http.StatusOK,
			assertProxy: func(t *testing.T, cap capturedUpstream) {
				t.Helper()
				if !strings.Contains(cap.auth, "/us-gov-west-1/athena/aws4_request") {
					t.Errorf("Authorization scope not GovCloud host-derived: %q", cap.auth)
				}
				ok, scope := boundaryValidateSigV4(cap.req, cap.body, []byte(secretAccessKey), realAccessKeyID, "us-gov-west-1", "athena")
				if !ok {
					t.Errorf("GovCloud re-sign did not validate (scope=%q, auth=%q)", scope, cap.auth)
				}
			},
		},
		{
			// Acceptance (#1979): a stored region/service that DISAGREES with
			// the resolved host is overridden by the host-derived scope. This
			// is the UnrecognizedClientException regression: a stale stored
			// region can no longer sign the wrong scope for the host the proxy
			// actually dials.
			name:          "sigv4-resign host scope overrides disagreeing stored region/service",
			bindingHost:   "athena.us-east-1.amazonaws.test",
			bindingRef:    "user/aws",
			bindingScheme: "sigv4-resign",
			bindingOpts:   []binding.HostBindingOption{binding.WithSigV4Resign(realAccessKeyID, "eu-west-1", "s3")},
			vaultName:     "user/aws",
			vaultKind:     "user",
			vaultValue:    []byte(secretAccessKey),
			reqURL:        "https://athena.us-east-1.amazonaws.test/",
			wantStatus:    http.StatusOK,
			assertProxy: func(t *testing.T, cap capturedUpstream) {
				t.Helper()
				if strings.Contains(cap.auth, "eu-west-1") || strings.Contains(cap.auth, "/s3/") {
					t.Errorf("Authorization signed against stale stored scope, not the host: %q", cap.auth)
				}
				ok, scope := boundaryValidateSigV4(cap.req, cap.body, []byte(secretAccessKey), realAccessKeyID, "us-east-1", "athena")
				if !ok {
					t.Errorf("host-overrides-stored re-sign did not validate (scope=%q, auth=%q)", scope, cap.auth)
				}
			},
		},
		{
			name:              "bearer binding injects unchanged (no regression)",
			bindingHost:       "api.example.test",
			bindingRef:        "api_key/github/octocat",
			bindingScheme:     "bearer",
			vaultName:         "api_key/github/octocat",
			vaultKind:         "api_key",
			vaultValue:        []byte("hb_secret"),
			reqURL:            "https://api.example.test/v1/resource",
			inboundAuthHeader: "", // bearer clients do not self-sign
			wantStatus:        http.StatusOK,
			assertProxy: func(t *testing.T, cap capturedUpstream) {
				t.Helper()
				if cap.auth != "Bearer hb_secret" {
					t.Fatalf("upstream Authorization = %q, want Bearer hb_secret", cap.auth)
				}
			},
		},
		{
			name:          "sigv4-resign binding with absent vault secret fails closed",
			bindingHost:   "s3.amazonaws.test",
			bindingRef:    "user/aws",
			bindingScheme: "sigv4-resign",
			bindingOpts:   []binding.HostBindingOption{binding.WithSigV4Resign(realAccessKeyID, region, service)},
			// No vault entry: fail closed, do not pass through unsigned.
			vaultName:         "",
			reqURL:            "https://s3.amazonaws.test/bucket/key",
			inboundAuthHeader: boundaryPlaceholderAuth,
			wantReject:        hostBindingRejectCredentialUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auditStore := audit.NewMemStore()
			var cap capturedUpstream
			dialed := false

			var v vault.Vault
			if tc.vaultName == "" {
				v = vault.NewMemVault() // empty: no entry at the ref path
			} else {
				v = mustVaultWith(t, tc.vaultName, tc.vaultKind, tc.vaultValue)
			}

			srv := &apiServer{
				auditStore:    auditStore,
				auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-1734" }),
				specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
				vault:         v,
				hostBindings:  mustHostBindingTableOpts(t, tc.bindingHost, tc.bindingRef, tc.bindingScheme, tc.bindingOpts...),
				sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					dialed = true
					var body []byte
					if req.Body != nil {
						body, _ = io.ReadAll(req.Body)
					}
					cap = capturedUpstream{
						req:  req,
						body: body,
						auth: req.Header.Get("Authorization"),
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
					}, nil
				})},
			}
			setup := newHostBindingProxySetup(t, srv)

			req, err := http.NewRequest(http.MethodGet, tc.reqURL, nil)
			if err != nil {
				t.Fatalf("http.NewRequest: %v", err)
			}
			if tc.inboundAuthHeader != "" {
				req.Header.Set("Authorization", tc.inboundAuthHeader)
			}

			resp, err := setup.client.Do(req)
			if err != nil {
				t.Fatalf("request through proxy: %v", err)
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)

			if tc.wantReject != "" {
				// Fail-closed row: 5xx to the in-container client, a rejection
				// audit with the expected reason, and no unsigned passthrough.
				if resp.StatusCode < 500 {
					t.Fatalf("status = %d, want fail-closed 5xx", resp.StatusCode)
				}
				if dialed {
					t.Error("upstream was dialed despite absent vault secret (unsigned passthrough)")
				}
				events, _ := auditStore.ListEvents(context.Background(), audit.EventFilter{})
				if len(events) != 1 {
					t.Fatalf("events = %d, want 1", len(events))
				}
				if got := events[0].Payload["aileron.proxy.reject_reason"]; got != tc.wantReject {
					t.Errorf("reject_reason = %v, want %q", got, tc.wantReject)
				}
				// The vault secret and placeholder must not leak into audit or body.
				payloadJSON, _ := json.Marshal(events[0].Payload)
				if strings.Contains(string(payloadJSON), secretAccessKey) {
					t.Errorf("audit payload leaked the secret access key: %s", payloadJSON)
				}
				return
			}

			// Success row.
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tc.wantStatus, string(respBody))
			}
			if !dialed {
				t.Fatal("upstream was never dialed on the success path")
			}
			if tc.assertProxy != nil {
				tc.assertProxy(t, cap)
			}

			// The vault secret must never surface in the response.
			if strings.Contains(string(respBody), secretAccessKey) {
				t.Error("response body leaked the secret access key")
			}
			for k, vs := range resp.Header {
				for _, vv := range vs {
					if strings.Contains(vv, secretAccessKey) {
						t.Errorf("response header %s leaked credential: %q", k, vv)
					}
				}
			}

			// A binding_injected audit event with the right scheme and host,
			// carrying no secret bytes.
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
			if got := evt.Payload["aileron.proxy.binding.scheme"]; got != tc.bindingScheme {
				t.Errorf("binding.scheme = %v, want %s", got, tc.bindingScheme)
			}
			if got := evt.Payload["aileron.proxy.binding.host"]; got != tc.bindingHost {
				t.Errorf("binding.host = %v, want %s", got, tc.bindingHost)
			}
			sandboxProxyBindingInjectedShape.validate(t, evt.Payload)
			payloadJSON, _ := json.Marshal(evt.Payload)
			if strings.Contains(string(payloadJSON), secretAccessKey) {
				t.Errorf("audit payload leaked the secret access key: %s", payloadJSON)
			}
		})
	}
}

// capturedUpstream records what the fake upstream transport received after
// the proxy applied the host binding.
type capturedUpstream struct {
	req  *http.Request
	body []byte
	auth string
}

// boundaryValidateSigV4 recomputes the AWS4-HMAC-SHA256 signature from the
// request the upstream received and compares it to the Signature= field of
// the Authorization header. It is a from-scratch reimplementation that shares
// no code with internal/credential/inject, so a bug in the production signer
// cannot mask itself here. It returns whether the recomputed signature matches
// and the credential scope parsed from the header.
func boundaryValidateSigV4(r *http.Request, body []byte, secretAccessKey []byte, wantAccessKeyID, region, service string) (bool, string) {
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
	scope := credParts[1]
	scopeParts := strings.Split(scope, "/")
	if len(scopeParts) != 4 {
		return false, scope
	}
	scopeDate := scopeParts[0]
	if scopeParts[1] != region || scopeParts[2] != service || scopeParts[3] != "aws4_request" {
		return false, scope
	}

	xAmzDate := r.Header.Get("X-Amz-Date")
	if xAmzDate == "" {
		return false, scope
	}

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		sum := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(sum[:])
	}

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
	canonicalQuery := boundaryCanonicalizeQuery(r.URL.RawQuery)

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
		boundaryHexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := boundaryDeriveSigningKey(secretAccessKey, scopeDate, region, service)
	want := hex.EncodeToString(boundaryHMACSHA256(signingKey, []byte(stringToSign)))
	return hmac.Equal([]byte(want), []byte(signature)), scope
}

func boundaryCanonicalizeQuery(raw string) string {
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
		pairs = append(pairs, kv{k: boundaryURIEncode(name), v: boundaryURIEncode(value)})
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

func boundaryURIEncode(s string) string {
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

func boundaryDeriveSigningKey(secret []byte, date, region, service string) []byte {
	kDate := boundaryHMACSHA256(append([]byte("AWS4"), secret...), []byte(date))
	kRegion := boundaryHMACSHA256(kDate, []byte(region))
	kService := boundaryHMACSHA256(kRegion, []byte(service))
	return boundaryHMACSHA256(kService, []byte("aws4_request"))
}

func boundaryHMACSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func boundaryHexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
