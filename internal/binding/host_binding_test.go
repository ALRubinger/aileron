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

func TestNewHostBinding_DefaultsToEmitMechanismInject(t *testing.T) {
	// A binding constructed without an emit-mechanism option defaults to
	// the inject mechanism (plant nothing, inject unconditionally).
	hb, err := binding.NewHostBinding("api.example.com", "user/example", "bearer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hb.EmitMechanism != binding.EmitMechanismInject {
		t.Errorf("EmitMechanism = %q, want inject by default", hb.EmitMechanism)
	}
}

func TestNewHostBinding_WithEmitMechanismSentinelSwap(t *testing.T) {
	hb, err := binding.NewHostBinding("api.github.com", "user/github", "bearer",
		binding.WithEmitMechanismSentinelSwap(), binding.WithSentinel("ghp_sentinel", "GH_TOKEN"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hb.EmitMechanism != binding.EmitMechanismSentinelSwap {
		t.Errorf("EmitMechanism = %q, want sentinel-swap after WithEmitMechanismSentinelSwap", hb.EmitMechanism)
	}
	if hb.SentinelValue != "ghp_sentinel" {
		t.Errorf("SentinelValue = %q, want ghp_sentinel", hb.SentinelValue)
	}
	if hb.SentinelEnv != "GH_TOKEN" {
		t.Errorf("SentinelEnv = %q, want GH_TOKEN", hb.SentinelEnv)
	}
}

func TestNewHostBinding_SentinelSwapRequiresSentinel(t *testing.T) {
	if _, err := binding.NewHostBinding("api.github.com", "user/github", "bearer",
		binding.WithEmitMechanismSentinelSwap()); err == nil {
		t.Fatal("expected error for sentinel-swap binding with no sentinel, got nil")
	}
	if _, err := binding.NewHostBinding("api.github.com", "user/github", "bearer",
		binding.WithEmitMechanismSentinelSwap(), binding.WithSentinel("ghp_sentinel", "")); err == nil {
		t.Fatal("expected error for sentinel-swap binding with empty sentinel env, got nil")
	}
	if _, err := binding.NewHostBinding("api.github.com", "user/github", "bearer",
		binding.WithEmitMechanismSentinelSwap(), binding.WithSentinel("", "GH_TOKEN")); err == nil {
		t.Fatal("expected error for sentinel-swap binding with empty sentinel value, got nil")
	}
}

func TestNewHostBinding_InjectRejectsSentinel(t *testing.T) {
	if _, err := binding.NewHostBinding("api.example.com", "user/example", "bearer",
		binding.WithSentinel("ghp_sentinel", "GH_TOKEN")); err == nil {
		t.Fatal("expected error for inject binding carrying a sentinel, got nil")
	}
}

func TestNewHostBinding_SentinelIndependentOfScheme(t *testing.T) {
	bearer, err := binding.NewHostBinding("api.bearer.test", "user/bearer", binding.SchemeBearer,
		binding.WithEmitMechanismSentinelSwap(), binding.WithSentinel("sent_bearer", "BEARER_TOKEN"))
	if err != nil {
		t.Fatalf("bearer sentinel-swap with sentinel: %v", err)
	}
	if bearer.SentinelValue != "sent_bearer" || bearer.SentinelEnv != "BEARER_TOKEN" {
		t.Errorf("bearer sentinel-swap sentinel = (%q,%q)", bearer.SentinelValue, bearer.SentinelEnv)
	}
	header, err := binding.NewHostBinding("api.header.test", "user/header", binding.SchemeHeaderTemplate,
		binding.WithHeaderTemplate("Authorization", "<token>"),
		binding.WithEmitMechanismSentinelSwap(), binding.WithSentinel("sent_header", "HEADER_TOKEN"))
	if err != nil {
		t.Fatalf("header-template sentinel-swap with sentinel: %v", err)
	}
	if header.SentinelValue != "sent_header" || header.SentinelEnv != "HEADER_TOKEN" {
		t.Errorf("header-template sentinel-swap sentinel = (%q,%q)", header.SentinelValue, header.SentinelEnv)
	}
}

func TestNewHostBinding_RejectsInvalidScheme(t *testing.T) {
	if _, err := binding.NewHostBinding("api.example.com", "oauth2/github/octocat", "magic"); err == nil {
		t.Fatal("expected error for unknown scheme, got nil")
	}
}

func TestNewHostBinding_AcceptsUserCredentialRef(t *testing.T) {
	// The user-level namespace (`user/<service>`) is the second valid
	// credential-ref shape: it is what `aileron auth <service>` writes
	// and what the GitHub bindings (#1195) name.
	hb, err := binding.NewHostBinding("api.github.com", "user/github", "bearer")
	if err != nil {
		t.Fatalf("unexpected error for user/github ref: %v", err)
	}
	if hb.CredentialRef != "user/github" {
		t.Errorf("CredentialRef = %q, want user/github", hb.CredentialRef)
	}
}

func TestNewHostBinding_RejectsOtherTwoSegmentRefs(t *testing.T) {
	// Only `user/<service>` is a valid two-segment ref; a connector-style
	// ref must still carry the full triple. A two-segment connector ref
	// remains a configuration error.
	for _, ref := range []string{"oauth2/github", "api_key/stripe", "user/", "user/Bad-CASE"} {
		if _, err := binding.NewHostBinding("api.example.com", ref, "bearer"); err == nil {
			t.Errorf("expected error for credential ref %q, got nil", ref)
		}
	}
}

func TestNewHostBinding_BasicSchemeRequiresUsername(t *testing.T) {
	// A basic binding with no username can never produce a well-formed
	// Authorization header, so it is rejected at construction rather than
	// failing closed at egress.
	if _, err := binding.NewHostBinding("github.com", "user/github", "basic"); err == nil {
		t.Fatal("expected error for basic scheme without username, got nil")
	}
	hb, err := binding.NewHostBinding("github.com", "user/github", "basic",
		binding.WithBasicUsername("x-access-token"))
	if err != nil {
		t.Fatalf("unexpected error for basic scheme with username: %v", err)
	}
	if hb.BasicUsername != "x-access-token" {
		t.Errorf("BasicUsername = %q, want x-access-token", hb.BasicUsername)
	}
	if hb.Scheme != binding.SchemeBasic {
		t.Errorf("Scheme = %q, want %q", hb.Scheme, binding.SchemeBasic)
	}
}

func TestNewHostBinding_HeaderTemplateRequiresHeaderAndTemplate(t *testing.T) {
	// A header-template binding with no header or no template can never
	// produce a well-formed header, so it is rejected at construction.
	if _, err := binding.NewHostBinding("api.linear.app", "user/linear", "header-template"); err == nil {
		t.Fatal("expected error for header-template without header/template, got nil")
	}
	if _, err := binding.NewHostBinding("api.linear.app", "user/linear", "header-template",
		binding.WithHeaderTemplate("Authorization", "")); err == nil {
		t.Fatal("expected error for header-template with empty template, got nil")
	}
	hb, err := binding.NewHostBinding("api.linear.app", "user/linear", "header-template",
		binding.WithHeaderTemplate("Authorization", "{token}"))
	if err != nil {
		t.Fatalf("unexpected error for valid header-template: %v", err)
	}
	if hb.HeaderName != "Authorization" || hb.HeaderTemplate != "{token}" {
		t.Errorf("header params = (%q,%q), want (Authorization,{token})", hb.HeaderName, hb.HeaderTemplate)
	}
}

func TestNewHostBinding_QueryParamRequiresName(t *testing.T) {
	if _, err := binding.NewHostBinding("api.example.com", "user/example", "query-param"); err == nil {
		t.Fatal("expected error for query-param without a param name, got nil")
	}
	hb, err := binding.NewHostBinding("api.example.com", "user/example", "query-param",
		binding.WithQueryParam("api_key"))
	if err != nil {
		t.Fatalf("unexpected error for valid query-param: %v", err)
	}
	if hb.QueryParamName != "api_key" {
		t.Errorf("QueryParamName = %q, want api_key", hb.QueryParamName)
	}
}

func TestNewHostBinding_SigV4ResignRequiresAllParams(t *testing.T) {
	// A sigv4-resign binding needs an access key id, region, and service to
	// produce a well-formed signature, so each missing individually is a
	// construction error (the acceptance criterion: no invalid binding
	// reaches the matcher). The secret access key is never a binding param;
	// it travels in the resolved credential value.
	missingCases := []struct {
		name                         string
		accessKeyID, region, service string
	}{
		{"missing access key id", "", "us-east-1", "s3"},
		{"missing region", "AKIDEXAMPLE", "", "s3"},
		{"missing service", "AKIDEXAMPLE", "us-east-1", ""},
	}
	for _, tc := range missingCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := binding.NewHostBinding("s3.amazonaws.com", "user/aws", "sigv4-resign",
				binding.WithSigV4Resign(tc.accessKeyID, tc.region, tc.service)); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
	// Bare sigv4-resign with no options is also rejected.
	if _, err := binding.NewHostBinding("s3.amazonaws.com", "user/aws", "sigv4-resign"); err == nil {
		t.Fatal("expected error for sigv4-resign without params, got nil")
	}

	hb, err := binding.NewHostBinding("s3.amazonaws.com", "user/aws", "sigv4-resign",
		binding.WithSigV4Resign("AKIDEXAMPLE", "us-east-1", "s3"))
	if err != nil {
		t.Fatalf("unexpected error for valid sigv4-resign: %v", err)
	}
	if hb.Scheme != binding.SchemeSigV4Resign {
		t.Errorf("Scheme = %q, want %q", hb.Scheme, binding.SchemeSigV4Resign)
	}
	if hb.AccessKeyID != "AKIDEXAMPLE" || hb.Region != "us-east-1" || hb.Service != "s3" {
		t.Errorf("sigv4 params = (%q,%q,%q), want (AKIDEXAMPLE,us-east-1,s3)",
			hb.AccessKeyID, hb.Region, hb.Service)
	}
}

func TestWithSigV4Resign_IgnoredByNonSigV4Scheme(t *testing.T) {
	// The sigv4 params are scheme-specific: a bearer binding that happens to
	// carry them constructs fine and simply ignores them at egress.
	hb, err := binding.NewHostBinding("api.example.com", "user/example", "bearer",
		binding.WithSigV4Resign("AKIDEXAMPLE", "us-east-1", "s3"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hb.Scheme != binding.SchemeBearer {
		t.Errorf("Scheme = %q, want bearer", hb.Scheme)
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
