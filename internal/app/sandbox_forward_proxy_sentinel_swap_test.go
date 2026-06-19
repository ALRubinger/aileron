package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/sentinel"
	"github.com/ALRubinger/aileron/internal/vault"
)

// Emit-mechanism B sentinel-swap contract at the egress boundary (#1196),
// driven through the real forward-proxy CONNECT/TLS path against the
// built-in api.github.com binding (bearer, mechanism B):
//
//	(a) a request bearing the reserved sentinel is swapped: the upstream
//	    sees the real credential via the bearer scheme, never the sentinel.
//	(b) a request bearing a foreign (non-sentinel) token is NOT swapped:
//	    the upstream sees the foreign token unchanged and no real secret.
//	(c) the sentinel string never reaches the upstream in any case.
//	(d) neither the swap nor the no-swap audit event carries any secret,
//	    sentinel, or foreign-token bytes.

func TestSentinelSwap_SecondDistinctBindingSwapsIndependently(t *testing.T) {
	// A second, distinct mechanism-B binding swaps its own sentinel for its
	// own credential through the real proxy path, independently of GitHub
	// (#1247). The GitHub sentinel on this host is foreign and must not be
	// swapped, proving recognition is per-binding, not GitHub-special.
	const otherSentinelVal = "sk_AILERONSENTINELSECONDBINDINGXXXXXXXX"
	const realOther = "sk_realsecondbinding_secret"

	other, err := binding.NewHostBinding("api.second.test", "user/second", binding.SchemeBearer,
		binding.WithEmitMechanismB(), binding.WithSentinel(otherSentinelVal, "SECOND_TOKEN"))
	if err != nil {
		t.Fatalf("NewHostBinding(second): %v", err)
	}

	var upstreamAuth string
	store := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    store,
		auditRecorder: audit.NewRecorder(store, nil, func() string { return "audit-second-binding" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "user/second", "user", []byte(realOther)),
		hostBindings:  binding.HostBindings{other},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	// (1) the second binding's own sentinel is swapped for its credential.
	req, _ := http.NewRequest(http.MethodGet, "https://api.second.test/me", nil)
	req.Header.Set("Authorization", "Bearer "+otherSentinelVal)
	resp, err := setup.client.Do(req)
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if upstreamAuth != "Bearer "+realOther {
		t.Errorf("upstream Authorization = %q, want Bearer %s (second binding must swap its own sentinel)", upstreamAuth, realOther)
	}
	if strings.Contains(upstreamAuth, otherSentinelVal) {
		t.Errorf("upstream leaked the second sentinel: %q", upstreamAuth)
	}

	// (2) GitHub's sentinel on this host is foreign and must be forwarded
	// unchanged, never swapped for the second binding's credential.
	upstreamAuth = ""
	req2, _ := http.NewRequest(http.MethodGet, "https://api.second.test/me", nil)
	req2.Header.Set("Authorization", "Bearer "+sentinel.GitHubTokenSentinel)
	resp2, err := setup.client.Do(req2)
	if err != nil {
		t.Fatalf("GET through proxy (foreign): %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if upstreamAuth != "Bearer "+sentinel.GitHubTokenSentinel {
		t.Errorf("foreign (github) sentinel upstream = %q, want forwarded unchanged", upstreamAuth)
	}
	if strings.Contains(upstreamAuth, realOther) {
		t.Errorf("upstream leaked the second binding's real credential on a foreign carrier: %q", upstreamAuth)
	}
	assertNoSecretInAudit(t, store, realOther)
}

func newGitHubSentinelServer(t *testing.T, v vault.Vault, capture *string) (*apiServer, *audit.MemStore) {
	t.Helper()
	ghBindings := githubDefaultBindings(t)
	store := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    store,
		auditRecorder: audit.NewRecorder(store, nil, func() string { return "audit-sentinel-swap" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         v,
		hostBindings:  ghBindings,
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if capture != nil {
				*capture = req.Header.Get("Authorization")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
	}
	return srv, store
}

func TestSentinelSwap_SentinelIsSwappedForRealCredential(t *testing.T) {
	const realToken = "ghp_realrealreal_secret"
	v := mustVaultWith(t, "user/github", "user", []byte(realToken))
	var upstreamAuth string
	srv, store := newGitHubSentinelServer(t, v, &upstreamAuth)
	setup := newHostBindingProxySetup(t, srv)

	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// gh sends the planted sentinel as a bearer token.
	req.Header.Set("Authorization", "Bearer "+sentinel.GitHubTokenSentinel)
	resp, err := setup.client.Do(req)
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// (a) the upstream sees the real credential via bearer.
	if upstreamAuth != "Bearer "+realToken {
		t.Errorf("upstream Authorization = %q, want Bearer %s", upstreamAuth, realToken)
	}
	// (c) the sentinel never reaches the upstream.
	if strings.Contains(upstreamAuth, sentinel.GitHubTokenSentinel) {
		t.Errorf("upstream Authorization leaked the sentinel: %q", upstreamAuth)
	}
	// The in-container client never sees the real token.
	if strings.Contains(string(body), realToken) {
		t.Error("response body leaked the real token")
	}
	// (d) audit carries no secret/sentinel bytes; a binding_injected event
	// is recorded for the swap.
	assertNoSecretInAudit(t, store, realToken, sentinel.GitHubTokenSentinel)
	assertAuditDecision(t, store, "binding_injected")
}

func TestSentinelSwap_ForeignTokenIsNotSwapped(t *testing.T) {
	const realToken = "ghp_realrealreal_secret"
	const foreignToken = "ghp_userssowntokenABCDEF1234567890wxyz"
	v := mustVaultWith(t, "user/github", "user", []byte(realToken))
	var upstreamAuth string
	srv, store := newGitHubSentinelServer(t, v, &upstreamAuth)
	setup := newHostBindingProxySetup(t, srv)

	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// The agent supplied its own token, not the sentinel.
	req.Header.Set("Authorization", "Bearer "+foreignToken)
	resp, err := setup.client.Do(req)
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// (b) the foreign token is forwarded unchanged; no real secret.
	if upstreamAuth != "Bearer "+foreignToken {
		t.Errorf("upstream Authorization = %q, want the foreign token unchanged (Bearer %s)", upstreamAuth, foreignToken)
	}
	if strings.Contains(upstreamAuth, realToken) {
		t.Errorf("upstream Authorization leaked the real token on a foreign-token request: %q", upstreamAuth)
	}
	// (c) the sentinel never appears either.
	if strings.Contains(upstreamAuth, sentinel.GitHubTokenSentinel) {
		t.Errorf("upstream Authorization leaked the sentinel: %q", upstreamAuth)
	}
	if strings.Contains(string(body), realToken) {
		t.Error("response body leaked the real token")
	}
	// (d) audit records the no-swap decision and carries no real secret.
	assertNoSecretInAudit(t, store, realToken, sentinel.GitHubTokenSentinel)
	assertAuditDecision(t, store, "foreign_token_not_swapped")
}

func TestSentinelSwap_BareSentinelWithoutSchemePrefixIsSwapped(t *testing.T) {
	// Some clients send the token value alone (no "Bearer " prefix). The
	// gate tolerates that and still recognizes our plant.
	const realToken = "ghp_bareseal_secret"
	v := mustVaultWith(t, "user/github", "user", []byte(realToken))
	var upstreamAuth string
	srv, _ := newGitHubSentinelServer(t, v, &upstreamAuth)
	setup := newHostBindingProxySetup(t, srv)

	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", sentinel.GitHubTokenSentinel)
	resp, err := setup.client.Do(req)
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if upstreamAuth != "Bearer "+realToken {
		t.Errorf("upstream Authorization = %q, want Bearer %s (bare sentinel must still swap)", upstreamAuth, realToken)
	}
}

func TestSentinelSwap_NoCarrierStillInjects(t *testing.T) {
	// A mechanism-B host with no inbound carrier injects per the binding's
	// scheme (the sentinel-swap gate governs only the carrier-present case).
	const realToken = "ghp_nocarrier_secret"
	v := mustVaultWith(t, "user/github", "user", []byte(realToken))
	var upstreamAuth string
	srv, _ := newGitHubSentinelServer(t, v, &upstreamAuth)
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.github.com/user")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if upstreamAuth != "Bearer "+realToken {
		t.Errorf("upstream Authorization = %q, want Bearer %s on a no-carrier mechanism-B request", upstreamAuth, realToken)
	}
}

// assertNoSecretInAudit scans every recorded audit event's payload for
// any of the forbidden values (the real secret and the sentinel) and
// fails if any appears. It mirrors sandbox_proxy_audit_shape_test.go's
// grep-style isolation assertion.
func assertNoSecretInAudit(t *testing.T, store *audit.MemStore, forbidden ...string) {
	t.Helper()
	events, _ := store.ListEvents(context.Background(), audit.EventFilter{})
	if len(events) == 0 {
		t.Fatal("no audit events recorded; expected at least one proxy event")
	}
	for _, ev := range events {
		raw, err := json.Marshal(ev.Payload)
		if err != nil {
			t.Fatalf("marshal audit payload: %v", err)
		}
		for _, secret := range forbidden {
			if secret != "" && strings.Contains(string(raw), secret) {
				t.Errorf("audit payload leaked forbidden value %q: %s", secret, raw)
			}
		}
	}
}

// assertAuditDecision asserts at least one recorded event carries the
// given aileron.proxy.decision value.
func assertAuditDecision(t *testing.T, store *audit.MemStore, decision string) {
	t.Helper()
	events, _ := store.ListEvents(context.Background(), audit.EventFilter{})
	for _, ev := range events {
		if d, ok := ev.Payload["aileron.proxy.decision"].(string); ok && d == decision {
			return
		}
	}
	t.Errorf("no audit event with decision %q; events=%d", decision, len(events))
}
