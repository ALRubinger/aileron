package app

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
)

// gitHubHostBindings contract (#1195): the constructor returns exactly
// the two user-level GitHub bindings, both naming the user/github
// credential-ref, with github.com -> basic (username x-access-token) and
// api.github.com -> bearer. No descriptor inlines a secret: a binding
// names where the credential lives, never its value.

func TestGitHubHostBindings_ReturnsExactlyTwoBindings(t *testing.T) {
	bindings, err := gitHubHostBindings()
	if err != nil {
		t.Fatalf("gitHubHostBindings: %v", err)
	}
	if len(bindings) != 2 {
		t.Fatalf("len(bindings) = %d, want 2", len(bindings))
	}
}

func TestGitHubHostBindings_ApexIsBasicWithAccessTokenUser(t *testing.T) {
	bindings, err := gitHubHostBindings()
	if err != nil {
		t.Fatalf("gitHubHostBindings: %v", err)
	}
	hb, ok := bindings.Match("github.com")
	if !ok {
		t.Fatal("no binding matched github.com")
	}
	if hb.Scheme != binding.SchemeBasic {
		t.Errorf("github.com scheme = %q, want %q", hb.Scheme, binding.SchemeBasic)
	}
	if hb.BasicUsername != "x-access-token" {
		t.Errorf("github.com BasicUsername = %q, want x-access-token", hb.BasicUsername)
	}
	if hb.CredentialRef != "user/github" {
		t.Errorf("github.com CredentialRef = %q, want user/github", hb.CredentialRef)
	}
}

func TestGitHubHostBindings_APIIsBearer(t *testing.T) {
	bindings, err := gitHubHostBindings()
	if err != nil {
		t.Fatalf("gitHubHostBindings: %v", err)
	}
	hb, ok := bindings.Match("api.github.com")
	if !ok {
		t.Fatal("no binding matched api.github.com")
	}
	if hb.Scheme != binding.SchemeBearer {
		t.Errorf("api.github.com scheme = %q, want %q", hb.Scheme, binding.SchemeBearer)
	}
	if hb.CredentialRef != "user/github" {
		t.Errorf("api.github.com CredentialRef = %q, want user/github", hb.CredentialRef)
	}
	// The bearer binding carries no basic username (it is irrelevant to
	// the scheme); a stray username here would be a copy-paste bug.
	if hb.BasicUsername != "" {
		t.Errorf("api.github.com BasicUsername = %q, want empty for bearer", hb.BasicUsername)
	}
}

func TestGitHubHostBindings_ExactHostsOnly(t *testing.T) {
	// Only the two exact apexes are sealed. A different github.com
	// subdomain (e.g. raw content host) must not match either binding,
	// so it falls through to passthrough rather than receiving a
	// credential it was never scoped for.
	bindings, err := gitHubHostBindings()
	if err != nil {
		t.Fatalf("gitHubHostBindings: %v", err)
	}
	for _, host := range []string{
		"raw.githubusercontent.com",
		"codeload.github.com",
		"gist.github.com",
		"evilgithub.com",
		"github.com.attacker.test",
	} {
		if _, ok := bindings.Match(host); ok {
			t.Errorf("host %q unexpectedly matched a GitHub binding; want passthrough", host)
		}
	}
}

func TestGitHubHostBindings_NoSecretBytesInDescriptors(t *testing.T) {
	// A binding descriptor names a credential-ref, never the credential
	// value. Assert no field looks like an inlined token.
	bindings, err := gitHubHostBindings()
	if err != nil {
		t.Fatalf("gitHubHostBindings: %v", err)
	}
	for _, hb := range bindings {
		for _, field := range []string{hb.HostPattern, hb.CredentialRef, hb.Scheme, hb.BasicUsername} {
			lower := strings.ToLower(field)
			// GitHub token prefixes (ghp_, gho_, github_pat_) would be the
			// shape of an inlined secret. The basic username is a fixed,
			// non-secret sentinel and is allowed even though it ends in
			// "token".
			if lower == "x-access-token" {
				continue
			}
			for _, marker := range []string{"ghp_", "gho_", "github_pat_", "token"} {
				if strings.Contains(lower, marker) {
					t.Errorf("binding field %q looks like an inlined secret (matched %q)", field, marker)
				}
			}
		}
	}
}
