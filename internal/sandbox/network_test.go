package sandbox

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// All assertions in this file cite the ADR clause they enforce so
// refactors that preserve the contract leave them green.

func TestHostPolicy_DefaultDenyWhenNoNetworkBlock(t *testing.T) {
	// ADR-0002 §"Connectors declare their needs in a manifest":
	//   "Each capability section is enumerable and bounded. There is no
	//    `*` and no implicit grant. A connector that did not declare
	//    network access cannot dial out, period."
	policy := NewHostPolicy(&cstore.Manifest{})
	err := policy.CheckURL("https://slack.com/")
	if err == nil {
		t.Fatal("expected denial when no network block was declared")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Class != ClassCapabilityDenied || serr.Boundary != BoundarySandbox {
		t.Errorf("err = %v; want capability_denied/sandbox", err)
	}
}

func TestHostPolicy_AllowsDeclaredHostPort(t *testing.T) {
	policy := NewHostPolicy(&cstore.Manifest{
		Capabilities: cstore.ManifestCapabilities{
			Network: &cstore.ManifestNetwork{Hosts: []string{"slack.com:443"}},
		},
	})
	if err := policy.CheckURL("https://slack.com/api"); err != nil {
		t.Errorf("https default port should be allowed: %v", err)
	}
	if err := policy.CheckURL("https://slack.com:443/api"); err != nil {
		t.Errorf("explicit https:443 should be allowed: %v", err)
	}
}

func TestHostPolicy_DeniesUndeclaredHost(t *testing.T) {
	// ADR-0005 example: a binary whose manifest declared only
	// slack.com:443 attempts to dial evil.example.com:443 — the
	// runtime refuses the import call.
	policy := NewHostPolicy(&cstore.Manifest{
		Capabilities: cstore.ManifestCapabilities{
			Network: &cstore.ManifestNetwork{Hosts: []string{"slack.com:443"}},
		},
	})
	err := policy.CheckURL("https://evil.example.com/")
	if err == nil {
		t.Fatal("expected denial for undeclared host")
	}
	var serr *Error
	if !errors.As(err, &serr) || serr.Class != ClassCapabilityDenied {
		t.Fatalf("err class = %v, want capability_denied", err)
	}
	if serr.Boundary != BoundarySandbox {
		t.Errorf("boundary = %q, want sandbox", serr.Boundary)
	}
	if got, _ := serr.Details["requested"].(string); got != "network:evil.example.com:443" {
		t.Errorf("details.requested = %q", got)
	}
	granted, _ := serr.Details["granted"].([]string)
	sort.Strings(granted)
	if len(granted) != 1 || granted[0] != "network:slack.com:443" {
		t.Errorf("details.granted = %v", granted)
	}
}

func TestHostPolicy_DeniesUndeclaredPort(t *testing.T) {
	// ADR-0002: "Pinned to specific host:port pairs; no wildcards."
	// slack.com:443 grants only the HTTPS port; HTTP on the same host
	// must be refused even though the host matches.
	policy := NewHostPolicy(&cstore.Manifest{
		Capabilities: cstore.ManifestCapabilities{
			Network: &cstore.ManifestNetwork{Hosts: []string{"slack.com:443"}},
		},
	})
	if err := policy.CheckURL("http://slack.com/"); err == nil {
		t.Fatal("expected denial when port differs from grant")
	}
}

func TestHostPolicy_RejectsNonHTTPScheme(t *testing.T) {
	// v1's connector ABI exposes HTTP only; ftp:// is a misuse of the
	// host import and must be refused even when the host:port appears
	// in the grant.
	policy := NewHostPolicy(&cstore.Manifest{
		Capabilities: cstore.ManifestCapabilities{
			Network: &cstore.ManifestNetwork{Hosts: []string{"slack.com:443"}},
		},
	})
	if err := policy.CheckURL("ftp://slack.com/"); err == nil {
		t.Fatal("expected denial for non-HTTP scheme")
	}
}

func TestHostPolicy_RejectsMalformedURL(t *testing.T) {
	policy := NewHostPolicy(&cstore.Manifest{
		Capabilities: cstore.ManifestCapabilities{
			Network: &cstore.ManifestNetwork{Hosts: []string{"slack.com:443"}},
		},
	})
	for _, in := range []string{"", "not a url", "://broken", "https://"} {
		t.Run(in, func(t *testing.T) {
			if err := policy.CheckURL(in); err == nil {
				t.Errorf("expected denial for %q", in)
			}
		})
	}
}

func TestHostPolicy_NormalizesHostCasing(t *testing.T) {
	// DNS is case-insensitive (RFC 1035). A grant for "Slack.COM:443"
	// must match a request to "slack.com:443" — connectors should not
	// hit a denial because of casing variance in either the manifest
	// or the URL.
	policy := NewHostPolicy(&cstore.Manifest{
		Capabilities: cstore.ManifestCapabilities{
			Network: &cstore.ManifestNetwork{Hosts: []string{"Slack.COM:443"}},
		},
	})
	if err := policy.CheckURL("https://slack.com/"); err != nil {
		t.Errorf("case-insensitive match failed: %v", err)
	}
}

func TestHostPolicy_AllowedHosts_ReturnsCopy(t *testing.T) {
	policy := NewHostPolicy(&cstore.Manifest{
		Capabilities: cstore.ManifestCapabilities{
			Network: &cstore.ManifestNetwork{Hosts: []string{"a.example.com:443", "b.example.com:443"}},
		},
	})
	hosts := policy.AllowedHosts()
	sort.Strings(hosts)
	if got := strings.Join(hosts, ","); got != "a.example.com:443,b.example.com:443" {
		t.Errorf("AllowedHosts() = %v", hosts)
	}
}

// TestHostPolicy_LocalOriginPermissive pins the ADR-0014
// "Local-origin (BYOCLI) wrap default" carve-out: a connector
// manifest with origin=local and no [capabilities.network] block
// gets a permissive policy. CheckURL / CheckHostPort short-circuit
// the allowlist gate. Hub-origin connectors with the same shape
// still deny.
func TestHostPolicy_LocalOriginPermissive(t *testing.T) {
	policy := NewHostPolicy(&cstore.Manifest{
		Connector: cstore.ManifestConnector{Origin: cstore.OriginLocal},
	})
	if !policy.Permissive {
		t.Fatal("local-origin manifest with no [capabilities.network] should be permissive")
	}
	// Allowlist is empty but every check must pass.
	for _, target := range []string{
		"https://api.linear.app/graphql",
		"https://api.github.com/repos",
		"http://internal.example/path",
	} {
		if err := policy.CheckURL(target); err != nil {
			t.Errorf("permissive policy should allow %q, got %v", target, err)
		}
	}
	for _, hp := range []string{
		"api.linear.app:443",
		"api.github.com:443",
	} {
		if err := policy.CheckHostPort(hp); err != nil {
			t.Errorf("permissive policy should allow %q, got %v", hp, err)
		}
	}
}

// TestHostPolicy_LocalOriginExplicitDeclarationIsStrict guards
// the bounded-carve-out: a local-origin connector that DID declare
// [capabilities.network] uses the strict allowlist, not permissive.
// The carve-out is the "omitted" case only.
func TestHostPolicy_LocalOriginExplicitDeclarationIsStrict(t *testing.T) {
	policy := NewHostPolicy(&cstore.Manifest{
		Connector: cstore.ManifestConnector{Origin: cstore.OriginLocal},
		Capabilities: cstore.ManifestCapabilities{
			Network: &cstore.ManifestNetwork{Hosts: []string{"api.linear.app:443"}},
		},
	})
	if policy.Permissive {
		t.Fatal("local-origin manifest with declared [capabilities.network] should NOT be permissive")
	}
	if err := policy.CheckURL("https://api.linear.app/graphql"); err != nil {
		t.Errorf("declared host should be allowed: %v", err)
	}
	if err := policy.CheckURL("https://evil.example/x"); err == nil {
		t.Error("undeclared host should be denied even on local-origin manifests when [capabilities.network] is present")
	}
}

// TestHostPolicy_HubOriginStillDeniesWhenOmitted regresses the
// hub-publisher contract: a hub-origin manifest with no
// [capabilities.network] block continues to deny, per ADR-0002's
// "no implicit grant" rule. The carve-out is local-only.
func TestHostPolicy_HubOriginStillDeniesWhenOmitted(t *testing.T) {
	// Origin not set (or any non-local value) → hub-style strict.
	policy := NewHostPolicy(&cstore.Manifest{
		Connector: cstore.ManifestConnector{Origin: ""},
	})
	if policy.Permissive {
		t.Fatal("hub-origin manifest with no [capabilities.network] must not be permissive")
	}
	if err := policy.CheckURL("https://anything.example/"); err == nil {
		t.Error("hub-origin with no [capabilities.network] should deny — ADR-0002's strict 'omitted = deny' rule")
	}
}
