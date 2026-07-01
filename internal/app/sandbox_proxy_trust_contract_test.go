package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/model"
)

// trustContractProxyServer wires an apiServer whose single host binding
// carries a per-step trust contract (#1735). upstreamAuth records the
// Authorization header the upstream saw so a test can assert whether a
// credential was injected; upstreamDialed records whether the upstream was
// reached at all, so a denial can assert the request never left the proxy.
func trustContractProxyServer(t *testing.T, hb binding.HostBinding, upstreamAuth *string, upstreamDialed *bool) (*apiServer, *audit.MemStore) {
	t.Helper()
	store := audit.NewMemStore()
	srv := &apiServer{
		auditStore:    store,
		auditRecorder: audit.NewRecorder(store, nil, func() string { return "audit-trust" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         mustVaultWith(t, "api_key/github/octocat", "api_key", []byte("hb_secret")),
		hostBindings:  binding.HostBindings{hb},
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if upstreamDialed != nil {
				*upstreamDialed = true
			}
			if upstreamAuth != nil {
				*upstreamAuth = req.Header.Get("Authorization")
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

func mustTrustBinding(t *testing.T, host, scheme string, opts ...binding.HostBindingOption) binding.HostBinding {
	t.Helper()
	hb, err := binding.NewHostBinding(host, "api_key/github/octocat", scheme, opts...)
	if err != nil {
		t.Fatalf("NewHostBinding: %v", err)
	}
	return hb
}

func onlyEvent(t *testing.T, store *audit.MemStore) audit.Event {
	t.Helper()
	events, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	return events[0]
}

// (a) An allowlisted host with an allowed effect injects the credential and
// records binding_injected carrying the plan/step/tool/effect identity and
// the upstream host.
func TestTrustContract_AllowedHostAndEffectInjects(t *testing.T) {
	hb := mustTrustBinding(t, "api.example.test", "bearer",
		binding.WithTrustContract("read", []string{"api.example.test"}),
		binding.WithToolIdentity("plan-7", "step-3", "linear.viewer"),
	)
	var upstreamAuth string
	srv, store := trustContractProxyServer(t, hb, &upstreamAuth, nil)
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.example.test/v1/resource")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if upstreamAuth != "Bearer hb_secret" {
		t.Fatalf("upstream Authorization = %q, want Bearer hb_secret", upstreamAuth)
	}
	evt := onlyEvent(t, store)
	if evt.EventType != model.EventTypeSandboxProxyBindingInjected {
		t.Fatalf("event type = %q, want binding_injected", evt.EventType)
	}
	if got := evt.Payload["aileron.trust.effect"]; got != "read" {
		t.Errorf("trust.effect = %v, want read", got)
	}
	if got := evt.Payload["aileron.plan.id"]; got != "plan-7" {
		t.Errorf("plan.id = %v, want plan-7", got)
	}
	if got := evt.Payload["aileron.step.id"]; got != "step-3" {
		t.Errorf("step.id = %v, want step-3", got)
	}
	if got := evt.Payload["aileron.tool.name"]; got != "linear.viewer" {
		t.Errorf("tool.name = %v, want linear.viewer", got)
	}
	if got := evt.Payload["aileron.proxy.upstream.host"]; got != "api.example.test" {
		t.Errorf("upstream.host = %v, want api.example.test", got)
	}
}

// (b) A host outside the binding's allowlist is denied with a 403 and
// trust_denied/trust_host_not_allowed. It is NOT passed through and no
// credential is injected (the upstream is never dialed).
func TestTrustContract_NonAllowlistedHostDenied(t *testing.T) {
	// The binding matches the wildcard host but scopes egress to a single
	// allowed host; the request targets a different subdomain.
	hb := mustTrustBinding(t, "*.example.test", "bearer",
		binding.WithTrustContract("read", []string{"allowed.example.test"}),
		binding.WithToolIdentity("plan-7", "step-3", "linear.viewer"),
	)
	var upstreamAuth string
	var dialed bool
	srv, store := trustContractProxyServer(t, hb, &upstreamAuth, &dialed)
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://denied.example.test/v1/resource")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if dialed {
		t.Error("upstream was dialed on a denied request; want no egress")
	}
	if upstreamAuth != "" {
		t.Errorf("credential injected on a denied request: %q", upstreamAuth)
	}
	evt := onlyEvent(t, store)
	if evt.EventType != model.EventTypeSandboxProxyTrustDenied {
		t.Fatalf("event type = %q, want trust_denied", evt.EventType)
	}
	if got := evt.Payload["aileron.proxy.reject_reason"]; got != trustRejectHostNotAllowed {
		t.Errorf("reject_reason = %v, want %q", got, trustRejectHostNotAllowed)
	}
}

// (c) A read-effect binding denies a mutating method with
// trust_effect_not_allowed, before injecting or dialing upstream.
func TestTrustContract_ReadEffectDeniesMutatingMethod(t *testing.T) {
	hb := mustTrustBinding(t, "api.example.test", "bearer",
		binding.WithTrustContract("read", []string{"api.example.test"}),
	)
	var upstreamAuth string
	var dialed bool
	srv, store := trustContractProxyServer(t, hb, &upstreamAuth, &dialed)
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Post("https://api.example.test/v1/resource", "application/json", strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("POST through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if dialed {
		t.Error("upstream was dialed on a read-effect mutating request; want denial")
	}
	if upstreamAuth != "" {
		t.Errorf("credential injected on a denied request: %q", upstreamAuth)
	}
	evt := onlyEvent(t, store)
	if evt.EventType != model.EventTypeSandboxProxyTrustDenied {
		t.Fatalf("event type = %q, want trust_denied", evt.EventType)
	}
	if got := evt.Payload["aileron.proxy.reject_reason"]; got != trustRejectEffectNotAllowed {
		t.Errorf("reject_reason = %v, want %q", got, trustRejectEffectNotAllowed)
	}
}

// (d) A write-effect binding admits a POST: all methods pass the effect gate
// for a write-class effect, so the credential is injected.
func TestTrustContract_WriteEffectAdmitsPost(t *testing.T) {
	hb := mustTrustBinding(t, "api.example.test", "bearer",
		binding.WithTrustContract("write", []string{"api.example.test"}),
	)
	var upstreamAuth string
	srv, store := trustContractProxyServer(t, hb, &upstreamAuth, nil)
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Post("https://api.example.test/v1/resource", "application/json", strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("POST through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if upstreamAuth != "Bearer hb_secret" {
		t.Fatalf("upstream Authorization = %q, want Bearer hb_secret", upstreamAuth)
	}
	evt := onlyEvent(t, store)
	if evt.EventType != model.EventTypeSandboxProxyBindingInjected {
		t.Fatalf("event type = %q, want binding_injected", evt.EventType)
	}
	if got := evt.Payload["aileron.trust.effect"]; got != "write" {
		t.Errorf("trust.effect = %v, want write", got)
	}
}

// (e) A binding with an empty trust contract still injects on any method:
// back-compat for every pre-existing binding (resolves the empty-default
// concern). The gate is skipped entirely for an empty allowlist and empty
// effect, and no identity fields appear in the payload.
func TestTrustContract_EmptyContractStillInjects(t *testing.T) {
	hb := mustTrustBinding(t, "api.example.test", "bearer") // no trust contract
	var upstreamAuth string
	srv, store := trustContractProxyServer(t, hb, &upstreamAuth, nil)
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Post("https://api.example.test/v1/resource", "application/json", strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("POST through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty contract is unconstrained)", resp.StatusCode)
	}
	if upstreamAuth != "Bearer hb_secret" {
		t.Fatalf("upstream Authorization = %q, want Bearer hb_secret", upstreamAuth)
	}
	evt := onlyEvent(t, store)
	if evt.EventType != model.EventTypeSandboxProxyBindingInjected {
		t.Fatalf("event type = %q, want binding_injected", evt.EventType)
	}
	if _, ok := evt.Payload["aileron.trust.effect"]; ok {
		t.Error("empty-contract binding emitted aileron.trust.effect; want omitted")
	}
	if _, ok := evt.Payload["aileron.plan.id"]; ok {
		t.Error("empty-contract binding emitted aileron.plan.id; want omitted")
	}
}

// (f) The trust gate runs before the sentinel-swap branch, so it applies to
// a sentinel-swap binding whose inbound carrier is a foreign token too: a
// read-effect sentinel-swap binding denies a POST with a foreign token
// before ever reaching the swap decision, with no egress.
func TestTrustContract_GateAppliesOnSentinelSwapForeignBranch(t *testing.T) {
	hb := mustTrustBinding(t, "api.github.com", "bearer",
		binding.WithEmitMechanismSentinelSwap(),
		binding.WithSentinel("ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA", "GH_TOKEN"),
		binding.WithTrustContract("read", []string{"api.github.com"}),
	)
	var upstreamAuth string
	var dialed bool
	srv, store := trustContractProxyServer(t, hb, &upstreamAuth, &dialed)
	// The sentinel-swap binding's vault ref must resolve; reuse the default.
	setup := newHostBindingProxySetup(t, srv)

	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/user", strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// A foreign token on the sentinel-swap host; the trust gate must deny
	// before the swap branch would forward it.
	req.Header.Set("Authorization", "Bearer ghp_userssowntokenABCDEF1234567890wxyz")
	resp, err := setup.client.Do(req)
	if err != nil {
		t.Fatalf("POST through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if dialed {
		t.Error("upstream was dialed on a denied sentinel-swap request; want no egress")
	}
	evt := onlyEvent(t, store)
	if evt.EventType != model.EventTypeSandboxProxyTrustDenied {
		t.Fatalf("event type = %q, want trust_denied", evt.EventType)
	}
	if got := evt.Payload["aileron.proxy.reject_reason"]; got != trustRejectEffectNotAllowed {
		t.Errorf("reject_reason = %v, want %q", got, trustRejectEffectNotAllowed)
	}
}
