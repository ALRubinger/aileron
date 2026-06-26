package binding_test

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/vault"
)

const sigv4Conn = "github://acme/athena"

// putSigV4 creates an aws_sigv4 binding carrying the non-secret region and
// access key id plus the secret access key bytes.
func putSigV4(t *testing.T, s *binding.VaultStore, identity, region, accessKeyID, secret string) {
	t.Helper()
	name, err := binding.MakeName("aws_sigv4", "athena", identity)
	if err != nil {
		t.Fatalf("MakeName: %v", err)
	}
	b := binding.Binding{
		Name:         name,
		Kind:         "aws_sigv4",
		Service:      "athena",
		Identity:     identity,
		ConnectorFQN: sigv4Conn,
		Region:       region,
		AccessKeyID:  accessKeyID,
		Status:       binding.StatusActive,
	}
	if err := s.Put(context.Background(), b, []byte(secret), binding.PutCreate); err != nil {
		t.Fatalf("Put(%s): %v", identity, err)
	}
}

func TestBindingRegionAccessKeyRoundTrip(t *testing.T) {
	// Region and AccessKeyID are non-secret metadata; they must survive a
	// Put/Get round trip through the vault label encoding.
	s := &binding.VaultStore{Vault: vault.NewMemVault()}
	putSigV4(t, s, "prod-east", "us-east-1", "AKIAEAST", "secret-east")

	got, err := s.Get(context.Background(), mustName(t, "aws_sigv4", "athena", "prod-east"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", got.Region)
	}
	if got.AccessKeyID != "AKIAEAST" {
		t.Errorf("AccessKeyID = %q, want AKIAEAST", got.AccessKeyID)
	}
}

func mustName(t *testing.T, kind, service, identity string) binding.Name {
	t.Helper()
	n, err := binding.MakeName(kind, service, identity)
	if err != nil {
		t.Fatalf("MakeName: %v", err)
	}
	return n
}

func TestResolverForAWSSigV4_NilWhenUnbound(t *testing.T) {
	// No aws_sigv4 binding exists for the connector → no resolver, so the
	// host fails closed with binding_required rather than signing with an
	// empty key.
	s := &binding.VaultStore{Vault: vault.NewMemVault()}
	if r := s.ResolverFor(context.Background(), sigv4Conn, "aws_sigv4"); r != nil {
		t.Fatalf("ResolverFor returned %T, want nil for unbound connector", r)
	}
}

func TestResolverForAWSSigV4_SingleBindingFallback(t *testing.T) {
	// A single binding resolves regardless of the requested region (the
	// region parse may have failed for a legacy global endpoint), and the
	// resolved Credential carries the binding's region and access key id so
	// the host can prefer them over the manifest.
	s := &binding.VaultStore{Vault: vault.NewMemVault()}
	putSigV4(t, s, "prod", "us-east-1", "AKIAONLY", "secret-only")

	r := s.ResolverFor(context.Background(), sigv4Conn, "aws_sigv4")
	if r == nil {
		t.Fatal("ResolverFor returned nil, want a resolver")
	}
	rr, ok := r.(credential.RegionalResolver)
	if !ok {
		t.Fatalf("resolver %T does not implement RegionalResolver", r)
	}

	// Plain Resolve (no region hint) succeeds with the sole binding.
	for _, region := range []string{"", "eu-west-1"} {
		cred, err := rr.ResolveForRegion(context.Background(), region)
		if err != nil {
			t.Fatalf("ResolveForRegion(%q): %v", region, err)
		}
		if cred.Kind != "aws_sigv4" {
			t.Errorf("Kind = %q, want aws_sigv4", cred.Kind)
		}
		if string(cred.Value) != "secret-only" {
			t.Errorf("Value mismatch for region %q", region)
		}
		if cred.Region != "us-east-1" || cred.AccessKeyID != "AKIAONLY" {
			t.Errorf("Region/AccessKeyID = %q/%q, want us-east-1/AKIAONLY", cred.Region, cred.AccessKeyID)
		}
	}
}

func TestResolverForAWSSigV4_RegionSelectsAmongMany(t *testing.T) {
	// Two region-scoped bindings for one connector install. The resolver
	// selects by region and returns that region's access key id and secret;
	// a request with no derivable region is ambiguous and refused rather
	// than guessed (ADR-0006 no-silent-matching).
	s := &binding.VaultStore{Vault: vault.NewMemVault()}
	putSigV4(t, s, "prod-east", "us-east-1", "AKIAEAST", "secret-east")
	putSigV4(t, s, "prod-west", "eu-west-1", "AKIAWEST", "secret-west")

	rr := s.ResolverFor(context.Background(), sigv4Conn, "aws_sigv4").(credential.RegionalResolver)

	east, err := rr.ResolveForRegion(context.Background(), "us-east-1")
	if err != nil {
		t.Fatalf("ResolveForRegion(us-east-1): %v", err)
	}
	if east.AccessKeyID != "AKIAEAST" || string(east.Value) != "secret-east" {
		t.Errorf("east = %q/%q, want AKIAEAST/secret-east", east.AccessKeyID, string(east.Value))
	}

	west, err := rr.ResolveForRegion(context.Background(), "eu-west-1")
	if err != nil {
		t.Fatalf("ResolveForRegion(eu-west-1): %v", err)
	}
	if west.AccessKeyID != "AKIAWEST" || string(west.Value) != "secret-west" {
		t.Errorf("west = %q/%q, want AKIAWEST/secret-west", west.AccessKeyID, string(west.Value))
	}

	if _, err := rr.ResolveForRegion(context.Background(), ""); err == nil {
		t.Error("ResolveForRegion(\"\") with multiple bindings = nil error, want ambiguous")
	}
	if _, err := rr.ResolveForRegion(context.Background(), "ap-south-1"); err == nil {
		t.Error("ResolveForRegion(unknown region) = nil error, want ambiguous")
	}
}
