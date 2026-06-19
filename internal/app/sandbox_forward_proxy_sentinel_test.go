package app

import (
	"net/http"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/sentinel"
)

// Unit-level coverage of the sentinel-swap decision helper, independent
// of the proxy plumbing. The end-to-end behavior is covered in
// sandbox_forward_proxy_sentinel_swap_test.go; these assert the decision
// branches and that the sentinel is stripped from the carrier on a swap.

func mustHostBindingSentinelSwap(t *testing.T) binding.HostBinding {
	t.Helper()
	hb, err := binding.NewHostBinding("api.github.com", "user/github", binding.SchemeBearer,
		binding.WithEmitMechanismSentinelSwap(), binding.WithSentinel(sentinel.GitHubTokenSentinel, "GH_TOKEN"))
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	return hb
}

func reqWithAuth(value string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	if value != "" {
		r.Header.Set(carrierHeader, value)
	}
	return r
}

func TestDecideSentinelSwap_InjectAlwaysInjects(t *testing.T) {
	hb, err := binding.NewHostBinding("github.com", "user/github", binding.SchemeBasic, binding.WithBasicUsername("x-access-token"))
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	// Even with a foreign carrier present, the inject mechanism injects
	// (overwrites).
	r := reqWithAuth("Bearer ghp_someforeigntoken")
	if got := decideSentinelSwap(r, hb); got != sentinelSwapInject {
		t.Errorf("inject decision = %v, want inject", got)
	}
}

func TestDecideSentinelSwap_SentinelIsStrippedAndInjected(t *testing.T) {
	hb := mustHostBindingSentinelSwap(t)
	r := reqWithAuth("Bearer " + sentinel.GitHubTokenSentinel)
	if got := decideSentinelSwap(r, hb); got != sentinelSwapInject {
		t.Errorf("sentinel decision = %v, want inject", got)
	}
	// The sentinel carrier must be stripped so the scheme can set the real
	// credential and the sentinel never rides upstream.
	if v := r.Header.Get(carrierHeader); v != "" {
		t.Errorf("carrier after swap = %q, want stripped (empty)", v)
	}
}

func TestDecideSentinelSwap_ForeignTokenIsNotSwapped(t *testing.T) {
	hb := mustHostBindingSentinelSwap(t)
	const foreign = "Bearer ghp_theusersowntoken1234567890abcd"
	r := reqWithAuth(foreign)
	if got := decideSentinelSwap(r, hb); got != sentinelSwapPassthroughForeign {
		t.Errorf("foreign-token decision = %v, want passthrough-foreign", got)
	}
	// The foreign carrier must be left intact for the unchanged forward.
	if v := r.Header.Get(carrierHeader); v != foreign {
		t.Errorf("foreign carrier mutated = %q, want unchanged %q", v, foreign)
	}
}

func TestDecideSentinelSwap_NoCarrierInjects(t *testing.T) {
	hb := mustHostBindingSentinelSwap(t)
	r := reqWithAuth("")
	if got := decideSentinelSwap(r, hb); got != sentinelSwapInject {
		t.Errorf("no-carrier decision = %v, want inject", got)
	}
}

func TestStripAuthScheme(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":     "abc",
		"bearer abc":     "abc",
		"token abc":      "abc",
		"TOKEN abc":      "abc",
		"abc":            "abc",
		"  Bearer  abc ": "abc",
		"Basic xyz":      "Basic xyz", // unrecognized scheme left intact
	}
	for in, want := range cases {
		if got := stripAuthScheme(in); got != want {
			t.Errorf("stripAuthScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBindingSentinelMatches_ReadsBindingSentinelValue(t *testing.T) {
	gh := mustHostBindingSentinelSwap(t)
	if !bindingSentinelMatches(gh, "Bearer "+sentinel.GitHubTokenSentinel) {
		t.Error("github binding did not recognize its own sentinel")
	}
	if !bindingSentinelMatches(gh, sentinel.GitHubTokenSentinel) {
		t.Error("github binding did not recognize the bare-carrier form of its sentinel")
	}
	if !bindingSentinelMatches(gh, "token "+sentinel.GitHubTokenSentinel) {
		t.Error("github binding did not recognize the token-prefixed form of its sentinel")
	}
}

func TestBindingSentinelMatches_DistinctSentinelsAreIndependent(t *testing.T) {
	gh := mustHostBindingSentinelSwap(t)
	const otherSentinel = "sk_AILERONSENTINELOTHER"
	other, err := binding.NewHostBinding("api.other.test", "user/other", binding.SchemeBearer,
		binding.WithEmitMechanismSentinelSwap(), binding.WithSentinel(otherSentinel, "OTHER_TOKEN"))
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	if !bindingSentinelMatches(other, "Bearer "+otherSentinel) {
		t.Error("second binding did not recognize its own sentinel")
	}
	if bindingSentinelMatches(other, "Bearer "+sentinel.GitHubTokenSentinel) {
		t.Error("second binding matched the github sentinel; recognition must be per-binding")
	}
	if bindingSentinelMatches(gh, "Bearer "+otherSentinel) {
		t.Error("github binding matched the second binding's sentinel; recognition must be per-binding")
	}
}

func TestBindingSentinelMatches_EmptySentinelNeverMatches(t *testing.T) {
	hb := binding.HostBinding{EmitMechanism: binding.EmitMechanismSentinelSwap}
	if bindingSentinelMatches(hb, "") {
		t.Error("empty-sentinel binding matched an empty carrier")
	}
	if bindingSentinelMatches(hb, "Bearer ghp_anything") {
		t.Error("empty-sentinel binding matched a non-empty carrier")
	}
}

func TestDecideSentinelSwap_SecondBindingSwapsIndependently(t *testing.T) {
	const otherSentinel = "sk_AILERONSENTINELOTHER"
	other, err := binding.NewHostBinding("api.other.test", "user/other", binding.SchemeBearer,
		binding.WithEmitMechanismSentinelSwap(), binding.WithSentinel(otherSentinel, "OTHER_TOKEN"))
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	r, _ := http.NewRequest(http.MethodGet, "https://api.other.test/me", nil)
	r.Header.Set(carrierHeader, "Bearer "+otherSentinel)
	if got := decideSentinelSwap(r, other); got != sentinelSwapInject {
		t.Errorf("second-binding sentinel decision = %v, want inject", got)
	}
	if v := r.Header.Get(carrierHeader); v != "" {
		t.Errorf("carrier after swap = %q, want stripped (empty)", v)
	}
	r2, _ := http.NewRequest(http.MethodGet, "https://api.other.test/me", nil)
	r2.Header.Set(carrierHeader, "Bearer "+sentinel.GitHubTokenSentinel)
	if got := decideSentinelSwap(r2, other); got != sentinelSwapPassthroughForeign {
		t.Errorf("foreign (github) sentinel on second host decision = %v, want passthrough-foreign", got)
	}
}
