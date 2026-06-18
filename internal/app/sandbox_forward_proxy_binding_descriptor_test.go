package app

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/audit"
	connectorspec "github.com/ALRubinger/aileron/internal/connector/spec"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// The generalization proof (#1197): a third-party CLI nobody special-cased
// in the proxy (Linear, api.linear.app) is sealed purely by a descriptor
// that flows through the real loader into the binding table, with zero
// per-CLI proxy code. Linear authenticates with its API key sent verbatim
// in the Authorization header with NO Bearer prefix; the header-template
// scheme expresses that. The agent in the sandbox never holds the key; the
// daemon resolves user/linear and injects it at the TLS forward-proxy
// boundary.
func TestBindingDescriptor_LinearSealedViaDescriptorOnly(t *testing.T) {
	const token = "lin_api_secret_key"
	v := mustVaultWith(t, "user/linear", "user", []byte(token))

	// Load the Linear binding through the REAL loader (built-in defaults
	// layer, no overrides). No hand-built binding, no per-CLI code.
	table, err := proxybinding.LoadHostBindings(proxybinding.LoadOptions{})
	if err != nil {
		t.Fatalf("LoadHostBindings: %v", err)
	}

	auditStore := audit.NewMemStore()
	var upstreamAuth string
	srv := &apiServer{
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, func() string { return "audit-linear-descriptor" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         v,
		hostBindings:  table,
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"data":{"viewer":{"id":"me"}}}`)),
			}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.linear.app/graphql")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	// Verbatim Authorization: the key with NO "Bearer " prefix.
	if upstreamAuth != token {
		t.Fatalf("upstream Authorization = %q, want verbatim %q (no Bearer)", upstreamAuth, token)
	}
	if strings.HasPrefix(upstreamAuth, "Bearer ") {
		t.Errorf("upstream Authorization = %q carries a Bearer prefix; Linear wants the bare key", upstreamAuth)
	}

	// Sealing: the in-container client never sees the raw token, in the
	// body or any response header.
	if strings.Contains(string(body), token) {
		t.Error("response body leaked the token")
	}
	for k, vals := range resp.Header {
		for _, val := range vals {
			if strings.Contains(val, token) {
				t.Errorf("response header %s leaked the token: %q", k, val)
			}
		}
	}

	// Sealing in the audit trail: no audit event carries the secret.
	events, err := auditStore.ListEvents(t.Context(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("audit list events: %v", err)
	}
	for _, ev := range events {
		raw, _ := json.Marshal(ev.Payload)
		if strings.Contains(string(raw), token) {
			t.Errorf("audit event %s leaked the token", ev.EventType)
		}
	}
}

// A query-param descriptor (a different scheme) injects the credential as
// a URL query parameter and seals it. This exercises the query-param arm
// of the generic injection-scheme mapping through the real proxy boundary,
// proving the descriptor path is not Linear/header-template specific.
func TestBindingDescriptor_QueryParamSchemeInjectsAndSeals(t *testing.T) {
	const token = "qp_api_secret"
	dir := t.TempDir()
	userPath := filepath.Join(dir, "user.yaml")
	if err := os.WriteFile(userPath, []byte(
		"version: v1\nbindings:\n  - host: api.qpexample.test\n    credential_ref: user/qpexample\n    scheme: query-param\n    query_param: api_key\n"), 0o600); err != nil {
		t.Fatalf("write user descriptor: %v", err)
	}

	table, err := proxybinding.LoadHostBindings(proxybinding.LoadOptions{UserPath: userPath})
	if err != nil {
		t.Fatalf("LoadHostBindings: %v", err)
	}

	v := mustVaultWith(t, "user/qpexample", "user", []byte(token))
	var upstreamQuery string
	srv := &apiServer{
		auditStore:    audit.NewMemStore(),
		auditRecorder: audit.NewRecorder(audit.NewMemStore(), nil, func() string { return "audit-qp" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         v,
		hostBindings:  table,
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamQuery = req.URL.Query().Get("api_key")
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.qpexample.test/v1/resource")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if upstreamQuery != token {
		t.Errorf("upstream api_key query = %q, want %q", upstreamQuery, token)
	}
	// Sealing: the in-container client never sees the raw token, in the
	// body or any response header.
	if strings.Contains(string(body), token) {
		t.Error("response body leaked the token")
	}
	for k, vals := range resp.Header {
		for _, val := range vals {
			if strings.Contains(val, token) {
				t.Errorf("response header %s leaked the token: %q", k, val)
			}
		}
	}
}

// Fail-closed: a locked/absent vault yields no Authorization header and no
// secret, and the upstream is not dialed. The descriptor only names where
// the credential lives; resolution happens daemon-side and fails closed.
func TestBindingDescriptor_LinearLockedVaultFailsClosed(t *testing.T) {
	// token is a defensive placeholder: it is deliberately NOT stored in
	// the vault. The point of this test is that no credential resolves, so
	// the leak assertions below confirm the fail-closed path never invents
	// or echoes a secret. Any string would do; a token-shaped one makes the
	// assertion's intent legible.
	const token = "lin_locked_secret"
	// No user/linear entry: the binding matches but resolution misses.
	v := mustVaultWith(t, "user/other", "user", []byte("unrelated"))

	table, err := proxybinding.LoadHostBindings(proxybinding.LoadOptions{})
	if err != nil {
		t.Fatalf("LoadHostBindings: %v", err)
	}

	var upstreamAuth string
	upstreamCalled := false
	srv := &apiServer{
		auditStore:    audit.NewMemStore(),
		auditRecorder: audit.NewRecorder(audit.NewMemStore(), nil, func() string { return "audit-linear-locked" }),
		specLoader:    func() ([]connectorspec.Spec, error) { return nil, nil },
		vault:         v,
		hostBindings:  table,
		sandboxProxyClient: &http.Client{Transport: sandboxProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			upstreamCalled = true
			upstreamAuth = req.Header.Get("Authorization")
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})},
	}
	setup := newHostBindingProxySetup(t, srv)

	resp, err := setup.client.Get("https://api.linear.app/graphql")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = 200 with missing credential; want fail-closed error")
	}
	if upstreamCalled {
		t.Error("upstream dialed despite missing credential; want fail-closed before dial")
	}
	if strings.Contains(string(body), token) || strings.Contains(upstreamAuth, token) {
		t.Error("token leaked on fail-closed path")
	}
}

// "Config, not code": no source file on the proxy path branches on a
// CLI/service name. Sealing Linear must be a descriptor change, never a
// code change. This greppable invariant fails if anyone reintroduces a
// per-CLI branch in proxy code (the BYOCLI mistake, #959 / ADR-0024).
func TestBindingDescriptor_NoPerCLIBranchInProxyCode(t *testing.T) {
	// Files implementing the forward-proxy boundary and the host-binding
	// injection path. A literal CLI/service name here would mean the
	// substrate special-cased a vendor instead of staying generic.
	proxyPathFiles := []string{
		"handlers_sandbox_forward_proxy.go",
		"handlers_connector_operations.go",
		"sandbox_forward_proxy_sentinel.go",
	}
	// Lowercased needles for CLI/service names that must never appear as a
	// branch in proxy code. GitHub is intentionally excluded: its bindings
	// are a deliberate built-in Go pair (github_bindings.go), which is a
	// separate seeding file, not the generic proxy path under test here.
	forbidden := []string{"linear", "printingpress", "printing_press"}

	for _, name := range proxyPathFiles {
		path := filepath.Join(".", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		lower := strings.ToLower(string(data))
		for _, needle := range forbidden {
			if strings.Contains(lower, needle) {
				t.Errorf("%s references %q: proxy code must stay generic, seal CLIs via descriptor (config, not code)", name, needle)
			}
		}
	}
}
