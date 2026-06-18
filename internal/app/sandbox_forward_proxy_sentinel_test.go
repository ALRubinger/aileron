package app

import (
	"net/http"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/sentinel"
)

// Unit-level coverage of the sentinel-swap decision helper, independent
// of the proxy plumbing (#1196). The end-to-end behavior is covered in
// sandbox_forward_proxy_sentinel_swap_test.go; these assert the decision
// branches and that the sentinel is stripped from the carrier on a swap.

func mustHostBindingB(t *testing.T) binding.HostBinding {
	t.Helper()
	hb, err := binding.NewHostBinding("api.github.com", githubCredentialRef, binding.SchemeBearer, binding.WithEmitMechanismB())
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

func TestDecideSentinelSwap_MechanismAAlwaysInjects(t *testing.T) {
	hb, err := binding.NewHostBinding("github.com", githubCredentialRef, binding.SchemeBasic, binding.WithBasicUsername("x-access-token"))
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	// Even with a foreign carrier present, mechanism A injects (overwrites).
	r := reqWithAuth("Bearer ghp_someforeigntoken")
	if got := decideSentinelSwap(r, hb); got != sentinelSwapInject {
		t.Errorf("mechanism A decision = %v, want inject", got)
	}
}

func TestDecideSentinelSwap_SentinelIsStrippedAndInjected(t *testing.T) {
	hb := mustHostBindingB(t)
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
	hb := mustHostBindingB(t)
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
	hb := mustHostBindingB(t)
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

func TestBindingSentinelMatches_OnlyGitHubRefRecognized(t *testing.T) {
	gh := mustHostBindingB(t)
	if !bindingSentinelMatches(gh, "Bearer "+sentinel.GitHubTokenSentinel) {
		t.Error("github binding did not recognize its own sentinel")
	}
	// A mechanism-B binding for a different credential-ref has no wired
	// recognizer yet, so it never matches (fails safe: no swap).
	other, err := binding.NewHostBinding("api.other.test", "user/other", binding.SchemeBearer, binding.WithEmitMechanismB())
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	if bindingSentinelMatches(other, "Bearer "+sentinel.GitHubTokenSentinel) {
		t.Error("non-github binding matched the github sentinel; recognizers must be per-binding")
	}
}
