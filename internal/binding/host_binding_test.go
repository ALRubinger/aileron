package binding_test

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
)

// mustHostBinding constructs a HostBinding or fails the test. Used to
// build the lookup tables under test.
func mustHostBinding(t *testing.T, host, ref, scheme string) binding.HostBinding {
	t.Helper()
	hb, err := binding.NewHostBinding(host, ref, scheme)
	if err != nil {
		t.Fatalf("NewHostBinding(%q,%q,%q): %v", host, ref, scheme, err)
	}
	return hb
}

func TestNewHostBinding_AcceptsValidInput(t *testing.T) {
	hb, err := binding.NewHostBinding("API.Example.com", "oauth2/github/octocat", "bearer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hb.HostPattern != "api.example.com" {
		t.Errorf("HostPattern = %q, want lowercased api.example.com", hb.HostPattern)
	}
	if hb.CredentialRef != "oauth2/github/octocat" {
		t.Errorf("CredentialRef = %q", hb.CredentialRef)
	}
	if hb.Scheme != "bearer" {
		t.Errorf("Scheme = %q", hb.Scheme)
	}
}

func TestNewHostBinding_RejectsInvalidScheme(t *testing.T) {
	if _, err := binding.NewHostBinding("api.example.com", "oauth2/github/octocat", "magic"); err == nil {
		t.Fatal("expected error for unknown scheme, got nil")
	}
}

func TestNewHostBinding_RejectsInvalidCredentialRef(t *testing.T) {
	for _, ref := range []string{"", "not-a-binding-name", "oauth2/github", "../escape/x"} {
		if _, err := binding.NewHostBinding("api.example.com", ref, "bearer"); err == nil {
			t.Errorf("expected error for credential ref %q, got nil", ref)
		}
	}
}

func TestNewHostBinding_RejectsInvalidHostPattern(t *testing.T) {
	for _, host := range []string{"", "  ", "*.com", "*", "a.*.com", "foo*.com", "*."} {
		if _, err := binding.NewHostBinding(host, "oauth2/github/octocat", "bearer"); err == nil {
			t.Errorf("expected error for host pattern %q, got nil", host)
		}
	}
}

func TestHostBindings_Match_Exact(t *testing.T) {
	tbl := binding.HostBindings{
		mustHostBinding(t, "api.example.com", "oauth2/github/octocat", "bearer"),
	}
	hb, ok := tbl.Match("api.example.com")
	if !ok {
		t.Fatal("expected exact match")
	}
	if hb.CredentialRef != "oauth2/github/octocat" {
		t.Errorf("CredentialRef = %q", hb.CredentialRef)
	}
	// Port-stripping is the caller's job; the matcher sees the bare host.
	if _, ok := tbl.Match("other.example.com"); ok {
		t.Error("expected no match for unrelated host")
	}
}

func TestHostBindings_Match_Wildcard(t *testing.T) {
	tbl := binding.HostBindings{
		mustHostBinding(t, "*.example.com", "oauth2/github/octocat", "bearer"),
	}
	for _, host := range []string{"a.example.com", "a.b.example.com", "deep.nested.example.com"} {
		if _, ok := tbl.Match(host); !ok {
			t.Errorf("expected wildcard match for %q", host)
		}
	}
	// Apex does not match a wildcard.
	if _, ok := tbl.Match("example.com"); ok {
		t.Error("wildcard must not match the apex host")
	}
	// A different domain does not match.
	if _, ok := tbl.Match("a.other.com"); ok {
		t.Error("wildcard must not match a different domain")
	}
}

func TestHostBindings_Match_ExactBeatsWildcard(t *testing.T) {
	tbl := binding.HostBindings{
		mustHostBinding(t, "*.example.com", "oauth2/wild/card", "bearer"),
		mustHostBinding(t, "api.example.com", "oauth2/exact/match", "bearer"),
	}
	hb, ok := tbl.Match("api.example.com")
	if !ok {
		t.Fatal("expected a match")
	}
	if hb.CredentialRef != "oauth2/exact/match" {
		t.Errorf("CredentialRef = %q, want exact to win over wildcard", hb.CredentialRef)
	}
}

func TestHostBindings_Match_LongestSuffixWins(t *testing.T) {
	tbl := binding.HostBindings{
		mustHostBinding(t, "*.example.com", "oauth2/short/suffix", "bearer"),
		mustHostBinding(t, "*.svc.example.com", "oauth2/long/suffix", "bearer"),
	}
	hb, ok := tbl.Match("a.svc.example.com")
	if !ok {
		t.Fatal("expected a match")
	}
	if hb.CredentialRef != "oauth2/long/suffix" {
		t.Errorf("CredentialRef = %q, want longest matching suffix to win", hb.CredentialRef)
	}
	// A host only the short wildcard covers still resolves to the short one.
	hb2, ok := tbl.Match("a.example.com")
	if !ok {
		t.Fatal("expected a match for short-only host")
	}
	if hb2.CredentialRef != "oauth2/short/suffix" {
		t.Errorf("CredentialRef = %q, want short suffix", hb2.CredentialRef)
	}
}

func TestHostBindings_Match_NoMatchReturnsFalse(t *testing.T) {
	tbl := binding.HostBindings{
		mustHostBinding(t, "api.example.com", "oauth2/github/octocat", "bearer"),
	}
	if _, ok := tbl.Match("nope.test"); ok {
		t.Error("expected no match")
	}
	if _, ok := tbl.Match(""); ok {
		t.Error("empty host must not match")
	}
}

func TestHostBindings_Match_NilTableIsEmpty(t *testing.T) {
	var tbl binding.HostBindings
	if _, ok := tbl.Match("api.example.com"); ok {
		t.Error("nil table must never match (preserves passthrough default)")
	}
}

func TestHostBindings_Match_CaseInsensitiveHost(t *testing.T) {
	tbl := binding.HostBindings{
		mustHostBinding(t, "api.example.com", "oauth2/github/octocat", "bearer"),
	}
	if _, ok := tbl.Match("API.Example.COM"); !ok {
		t.Error("host match must be case-insensitive")
	}
}
