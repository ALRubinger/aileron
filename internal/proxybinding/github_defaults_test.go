package proxybinding

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/sentinel"
)

// GitHub ships as a trusted built-in descriptor (defaults/github.yaml,
// #1248), replacing the bespoke Go bindings. These tests assert the
// descriptor contract through the public Load/LoadHostBindings output: the
// two GitHub bindings appear in the built-in layer with no user descriptor
// configured, github.com is basic/inject with no sentinel, and
// api.github.com is bearer/sentinel-swap carrying the GitHub sentinel value
// and GH_TOKEN env. They
// assert against the loaded table, never the file internals, so they
// survive a descriptor reformat that preserves the contract.

func TestLoad_BuiltinIncludesGitHub(t *testing.T) {
	entries, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	apex, ok := findEntry(entries, "github.com")
	if !ok {
		t.Fatalf("built-in load missing github.com; got %v", entries)
	}
	if apex.Scheme != binding.SchemeBasic {
		t.Errorf("github.com scheme = %q, want basic", apex.Scheme)
	}
	if apex.EmitMechanism != string(binding.EmitMechanismInject) {
		t.Errorf("github.com emit_mechanism = %q, want inject", apex.EmitMechanism)
	}
	if apex.CredentialRef != "user/github" {
		t.Errorf("github.com credential_ref = %q, want user/github", apex.CredentialRef)
	}
	if apex.Username != "x-access-token" {
		t.Errorf("github.com username = %q, want x-access-token", apex.Username)
	}
	// The inject mechanism carries no sentinel block.
	if apex.Sentinel != nil {
		t.Errorf("github.com carries a sentinel block %+v, want none for inject", apex.Sentinel)
	}

	api, ok := findEntry(entries, "api.github.com")
	if !ok {
		t.Fatalf("built-in load missing api.github.com; got %v", entries)
	}
	if api.Scheme != binding.SchemeBearer {
		t.Errorf("api.github.com scheme = %q, want bearer", api.Scheme)
	}
	if api.EmitMechanism != string(binding.EmitMechanismSentinelSwap) {
		t.Errorf("api.github.com emit_mechanism = %q, want sentinel-swap", api.EmitMechanism)
	}
	if api.CredentialRef != "user/github" {
		t.Errorf("api.github.com credential_ref = %q, want user/github", api.CredentialRef)
	}
	if api.Sentinel == nil {
		t.Fatal("api.github.com missing sentinel block, want one for sentinel-swap")
	}
	if api.Sentinel.Value != sentinel.GitHubTokenSentinel {
		t.Errorf("api.github.com sentinel.value = %q, want %q", api.Sentinel.Value, sentinel.GitHubTokenSentinel)
	}
	if api.Sentinel.Env != "GH_TOKEN" {
		t.Errorf("api.github.com sentinel.env = %q, want GH_TOKEN", api.Sentinel.Env)
	}
}

// The descriptor adapts to a binding table whose Match resolves both
// GitHub hosts with the right scheme, emit mechanism, and sentinel shape.
// This is the production path: LoadHostBindings is what the daemon wiring
// (#1248) calls with no user descriptor configured.
func TestLoadHostBindings_MatchesGitHub(t *testing.T) {
	table, err := LoadHostBindings(LoadOptions{})
	if err != nil {
		t.Fatalf("LoadHostBindings: %v", err)
	}

	apex, ok := table.Match("github.com")
	if !ok {
		t.Fatal("table.Match(github.com) = false, want true")
	}
	if apex.Scheme != binding.SchemeBasic {
		t.Errorf("github.com scheme = %q, want basic", apex.Scheme)
	}
	if apex.BasicUsername != "x-access-token" {
		t.Errorf("github.com BasicUsername = %q, want x-access-token", apex.BasicUsername)
	}
	if apex.EmitMechanism != binding.EmitMechanismInject {
		t.Errorf("github.com EmitMechanism = %q, want inject", apex.EmitMechanism)
	}
	if apex.SentinelValue != "" || apex.SentinelEnv != "" {
		t.Errorf("github.com carries a sentinel (%q,%q), want none for inject", apex.SentinelValue, apex.SentinelEnv)
	}

	api, ok := table.Match("api.github.com")
	if !ok {
		t.Fatal("table.Match(api.github.com) = false, want true")
	}
	if api.Scheme != binding.SchemeBearer {
		t.Errorf("api.github.com scheme = %q, want bearer", api.Scheme)
	}
	if api.EmitMechanism != binding.EmitMechanismSentinelSwap {
		t.Errorf("api.github.com EmitMechanism = %q, want sentinel-swap", api.EmitMechanism)
	}
	if api.SentinelValue != sentinel.GitHubTokenSentinel {
		t.Errorf("api.github.com SentinelValue = %q, want %q", api.SentinelValue, sentinel.GitHubTokenSentinel)
	}
	if api.SentinelEnv != "GH_TOKEN" {
		t.Errorf("api.github.com SentinelEnv = %q, want GH_TOKEN", api.SentinelEnv)
	}
}

// Only the two exact apexes are sealed (no wildcard). A different
// github.com subdomain (raw content, gist, codeload) or a lookalike host
// must not match, so it falls through to passthrough rather than receiving
// a credential it was never scoped for.
func TestLoadHostBindings_GitHubExactHostsOnly(t *testing.T) {
	table, err := LoadHostBindings(LoadOptions{})
	if err != nil {
		t.Fatalf("LoadHostBindings: %v", err)
	}
	for _, host := range []string{
		"raw.githubusercontent.com",
		"codeload.github.com",
		"gist.github.com",
		"evilgithub.com",
		"github.com.attacker.test",
	} {
		if _, ok := table.Match(host); ok {
			t.Errorf("host %q unexpectedly matched a GitHub binding; want passthrough", host)
		}
	}
}

// The shipped descriptor names a credential-ref, never the credential
// bytes. No binding field looks like an inlined GitHub token. The sentinel
// value mimics the ghp_ prefix by design (it is the non-secret placeholder
// the proxy swaps), so it is exempted from the leak check.
func TestLoadHostBindings_GitHubNoSecretBytes(t *testing.T) {
	table, err := LoadHostBindings(LoadOptions{})
	if err != nil {
		t.Fatalf("LoadHostBindings: %v", err)
	}
	for _, hb := range table {
		if hb.HostPattern != "github.com" && hb.HostPattern != "api.github.com" {
			continue
		}
		for _, field := range []string{hb.CredentialRef, hb.Scheme, hb.BasicUsername} {
			lower := strings.ToLower(field)
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

// The trusted GitHub default is a normal layer in the built-in < user
// precedence, not privileged. A user descriptor overriding github.com wins
// for that host, while api.github.com stays the shipped default. This
// proves GitHub is overridable exactly like Linear (#1248 acceptance
// criterion 4).
func TestLoad_UserOverridesGitHub(t *testing.T) {
	dir := t.TempDir()
	userPath := writeDescriptor(t, dir, "user.yaml",
		"version: v1\nbindings:\n  - host: github.com\n    credential_ref: user/github-override\n    scheme: bearer\n")

	entries, err := Load(LoadOptions{UserPath: userPath})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	apex, ok := findEntry(entries, "github.com")
	if !ok {
		t.Fatal("missing github.com after user override")
	}
	if apex.Scheme != binding.SchemeBearer {
		t.Errorf("after user override github.com scheme = %q, want bearer", apex.Scheme)
	}
	if apex.CredentialRef != "user/github-override" {
		t.Errorf("github.com credential_ref = %q, want user/github-override (user wins)", apex.CredentialRef)
	}

	// api.github.com is untouched by the override and stays the shipped
	// default (bearer, sentinel-swap, GitHub sentinel).
	api, ok := findEntry(entries, "api.github.com")
	if !ok {
		t.Fatal("user override of github.com dropped the shipped api.github.com default")
	}
	if api.Scheme != binding.SchemeBearer || api.EmitMechanism != string(binding.EmitMechanismSentinelSwap) {
		t.Errorf("api.github.com = %q/%q, want bearer/sentinel-swap (default preserved)", api.Scheme, api.EmitMechanism)
	}
	if api.Sentinel == nil || api.Sentinel.Value != sentinel.GitHubTokenSentinel {
		t.Errorf("api.github.com sentinel changed under a github.com-only override: %+v", api.Sentinel)
	}
}
