package sandbox

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/vault"
)

// TestInjectCredential_AWSSigV4PathARegionScopedBindings is the Path A
// (WASM connector-spec) end-to-end regression for #1663: an aws_sigv4
// binding holds the secret access key plus the non-secret region and
// access key id, the connector manifest declares only the service, and the
// host signs each outbound request with the binding selected by the
// request's target AWS region.
//
// It wires a real binding.VaultStore (the same path the runtime uses via
// ResolverFor) with two region-differing bindings for one connector
// install, then drives injectCredential for two requests whose hosts carry
// different regions. The assertion is region-scoped: each Authorization is
// an AWS4-HMAC-SHA256 signature whose credential scope and Credential=
// access key id match the binding for that request's region, the canonical
// X-Amz-Date is present, and the two signatures differ (proving the
// region-derived signing key, not a shared one, was used). The secret
// access keys never appear in any header.
func TestInjectCredential_AWSSigV4PathARegionScopedBindings(t *testing.T) {
	const (
		connFQN = "github://acme/athena"

		eastKeyID  = "AKIAEASTEXAMPLE"
		eastSecret = "wJalrEastSecretAccessKeyEXAMPLE000000000"
		eastHost   = "athena.us-east-1.amazonaws.com"

		westKeyID  = "AKIAWESTEXAMPLE"
		westSecret = "wJalrWestSecretAccessKeyEXAMPLE111111111"
		westHost   = "athena.eu-west-1.amazonaws.com"
	)

	store := &binding.VaultStore{Vault: vault.NewMemVault()}
	putRegionBinding(t, store, connFQN, "prod-east", "us-east-1", eastKeyID, eastSecret)
	putRegionBinding(t, store, connFQN, "prod-west", "eu-west-1", westKeyID, westSecret)

	resolver := store.ResolverFor(context.Background(), connFQN, cstore.CredentialKindAWSSigV4)
	if resolver == nil {
		t.Fatal("ResolverFor(aws_sigv4) returned nil; the connector has two bindings")
	}

	// Manifest declares only the service; region and access_key_id are
	// intentionally absent so the test proves binding-wins, not a manifest
	// fallback.
	newState := func() *hostState {
		return &hostState{
			connectorFQN:           connFQN,
			expectedCredentialKind: cstore.CredentialKindAWSSigV4,
			credentialService:      "athena",
			credentialResolver:     resolver,
		}
	}

	signFor := func(host string) string {
		st := newState()
		req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
		if err := injectCredential(context.Background(), st, req, cstore.CredentialKindAWSSigV4); err != nil {
			t.Fatalf("injectCredential(%s): %v", host, err)
		}
		if req.Header.Get("X-Amz-Date") == "" {
			t.Errorf("%s: X-Amz-Date not set", host)
		}
		if req.Header.Get("X-Amz-Content-Sha256") == "" {
			t.Errorf("%s: X-Amz-Content-Sha256 not set", host)
		}
		return req.Header.Get("Authorization")
	}

	eastAuth := signFor(eastHost)
	westAuth := signFor(westHost)

	assertScoped(t, "east", eastAuth, eastKeyID, "us-east-1", "athena")
	assertScoped(t, "west", westAuth, westKeyID, "eu-west-1", "athena")

	// The two signatures must differ: a region-scoped signing key (derived
	// from region + the per-binding secret) is what makes them distinct. If
	// they matched, region selection silently collapsed to one binding.
	eastSig := signatureField(eastAuth)
	westSig := signatureField(westAuth)
	if eastSig == "" || westSig == "" {
		t.Fatalf("missing Signature= field: east=%q west=%q", eastAuth, westAuth)
	}
	if eastSig == westSig {
		t.Errorf("east and west signatures are identical (%q); region selection did not vary the signing key", eastSig)
	}

	// No secret access key may appear in any signed header.
	for _, secret := range []string{eastSecret, westSecret} {
		for _, auth := range []string{eastAuth, westAuth} {
			if strings.Contains(auth, secret) {
				t.Errorf("Authorization leaked a secret access key: %q", auth)
			}
		}
	}
}

func TestRegionFromHost(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"athena.us-east-1.amazonaws.com", "us-east-1"},
		{"athena.eu-west-1.amazonaws.com", "eu-west-1"},
		{"s3.ap-southeast-2.amazonaws.com", "ap-southeast-2"},
		{"athena.us-gov-west-1.amazonaws.com", "us-gov-west-1"}, // GovCloud partition (was missed by the old regex)
		{"s3.amazonaws.com", ""},     // legacy global endpoint: no region segment
		{"example.com", ""},          // not an AWS host
		{"athena.amazonaws.com", ""}, // service-only, no region
		{"", ""},                     // empty host
	}
	for _, tc := range cases {
		if got := regionFromHost(tc.host); got != tc.want {
			t.Errorf("regionFromHost(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func putRegionBinding(t *testing.T, s *binding.VaultStore, connFQN, identity, region, accessKeyID, secret string) {
	t.Helper()
	name, err := binding.MakeName(cstore.CredentialKindAWSSigV4, "athena", identity)
	if err != nil {
		t.Fatalf("MakeName: %v", err)
	}
	b := binding.Binding{
		Name:         name,
		Kind:         cstore.CredentialKindAWSSigV4,
		Service:      "athena",
		Identity:     identity,
		ConnectorFQN: connFQN,
		Region:       region,
		AccessKeyID:  accessKeyID,
		Status:       binding.StatusActive,
	}
	if err := s.Put(context.Background(), b, []byte(secret), binding.PutCreate); err != nil {
		t.Fatalf("Put(%s): %v", identity, err)
	}
}

// assertScoped checks an Authorization header is a SigV4 signature whose
// Credential= carries wantKeyID and whose scope ends with
// <region>/<service>/aws4_request.
func assertScoped(t *testing.T, label, auth, wantKeyID, region, service string) {
	t.Helper()
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("%s: Authorization = %q, want AWS4-HMAC-SHA256 prefix", label, auth)
	}
	if !strings.Contains(auth, "Credential="+wantKeyID+"/") {
		t.Errorf("%s: Authorization missing Credential=%s/...: %q", label, wantKeyID, auth)
	}
	wantScopeSuffix := region + "/" + service + "/aws4_request"
	if !strings.Contains(auth, wantScopeSuffix) {
		t.Errorf("%s: Authorization missing scope suffix %q: %q", label, wantScopeSuffix, auth)
	}
}

// signatureField returns the hex signature from a SigV4 Authorization
// header, or "" when absent.
func signatureField(auth string) string {
	for _, part := range strings.Split(auth, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "Signature=") {
			return strings.TrimPrefix(part, "Signature=")
		}
	}
	return ""
}
