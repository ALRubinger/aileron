package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/vault"
)

// installCredentialConnector writes a fake connector entry whose
// manifest declares `[capabilities.credential]`. Variant of
// installFakeConnector for the credential-mediation tests.
func installCredentialConnector(t *testing.T, store *cstore.Store, fqn, version, kind string) string {
	t.Helper()
	manifestTOML := `[connector]
name = "` + fqn + `"
version = "` + version + `"
publisher = "test"

[capabilities.credential]
kind = "` + kind + `"
`
	tb := &cstore.Tarball{
		BinaryName: "connector.wasm",
		Binary:     []byte("FAKE-BINARY"),
		Manifest:   []byte(manifestTOML),
		Signature:  []byte("FAKE-SIG"),
	}
	hashHex := tb.CanonicalHashHex()
	dir := filepath.Join(store.Root(), "connectors", "sha256", hashHex)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "connector.wasm"), tb.Binary, 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), tb.Manifest, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "signature.sig"), tb.Signature, 0o644); err != nil {
		t.Fatalf("write signature: %v", err)
	}
	return "sha256:" + hashHex
}

// bindingStoreWith builds a binding.VaultStore over an in-memory vault
// pre-populated with the given binding for the given connector. The
// service segment is taken from the binding name's middle segment.
func bindingStoreWith(t *testing.T, name, kind, connectorFQN string, value []byte) binding.Store {
	t.Helper()
	s := &binding.VaultStore{Vault: vault.NewMemVault()}
	bn, _, service, identity, err := binding.Parse(name)
	if err != nil {
		t.Fatalf("Parse(%q): %v", name, err)
	}
	if err := s.Put(context.Background(), binding.Binding{
		Name:         bn,
		Kind:         kind,
		Service:      service,
		Identity:     identity,
		ConnectorFQN: connectorFQN,
	}, value, binding.PutCreate); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	return s
}

func TestSandboxExecutor_WiresResolver_WhenBindingMatches(t *testing.T) {
	// ADR-0005 + ADR-0006: when the connector manifest declares
	// [capabilities.credential] and the user has created a matching
	// binding, the executor wires a per-step credential.Resolver into
	// sandbox.Call. The resolver, when called, returns the bound
	// vault entry. Uses api_key kind to keep the assertion focused on
	// resolver wiring; the OAuth2-specific JSON envelope shape is
	// covered by credential/oauth2_resolver_test.go.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installCredentialConnector(t, store, "github://test/api-echo", "1.0.0", "api_key")
	actions := installFakeAction(t, "github://test/api-echo", "1.0.0", hash,
		[]string{"call"}, []string{"call"})

	bs := bindingStoreWith(t, "api_key/api-echo/work", "api_key",
		"github://test/api-echo", []byte("test-token"))

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/api-echo": {resp: map[string]any{"ok": true}}}

	exec := NewSandboxExecutor(actions, store, rt, bs)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	c := rt.connectors["github://test/api-echo"]
	if len(c.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(c.calls))
	}
	resolver := c.calls[0].CredentialResolver
	if resolver == nil {
		t.Fatal("CredentialResolver was nil; expected wired resolver")
	}
	cred, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(cred.Value) != "test-token" {
		t.Errorf("Resolve.Value = %q, want test-token", cred.Value)
	}
	if cred.Kind != "api_key" {
		t.Errorf("Resolve.Kind = %q, want api_key", cred.Kind)
	}
}

func TestSandboxExecutor_NoResolver_WhenConnectorDeclaresNoCredential(t *testing.T) {
	// Connector manifest declares no [capabilities.credential]; even
	// when a binding exists, the executor passes nil because no
	// credential mediation is needed for this connector.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	actions := installFakeAction(t, "github://test/echo", "1.0.0", hash,
		[]string{"call"}, []string{"call"})

	bs := bindingStoreWith(t, "api_key/echo/work", "api_key",
		"github://test/echo", []byte("nope"))

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/echo": {resp: map[string]any{}}}

	exec := NewSandboxExecutor(actions, store, rt, bs)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	c := rt.connectors["github://test/echo"]
	if len(c.calls) == 0 || c.calls[0].CredentialResolver != nil {
		t.Errorf("expected nil CredentialResolver; got %v", c.calls[0].CredentialResolver)
	}
}

func TestSandboxExecutor_NoResolver_WhenNoBindingExists(t *testing.T) {
	// Connector manifest declares [capabilities.credential] but the
	// binding store has no matching entry → resolver is nil. The host
	// surfaces `binding_required` on the first credential reference.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installCredentialConnector(t, store, "github://test/api-echo", "1.0.0", "oauth2")
	actions := installFakeAction(t, "github://test/api-echo", "1.0.0", hash,
		[]string{"call"}, []string{"call"})

	bs := &binding.VaultStore{Vault: vault.NewMemVault()} // empty store
	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/api-echo": {resp: map[string]any{}}}

	exec := NewSandboxExecutor(actions, store, rt, bs)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	c := rt.connectors["github://test/api-echo"]
	if len(c.calls) == 0 || c.calls[0].CredentialResolver != nil {
		t.Errorf("expected nil CredentialResolver when no binding; got %v", c.calls[0].CredentialResolver)
	}
}

func TestSandboxExecutor_NoResolver_WhenAmbiguousBinding(t *testing.T) {
	// Per ADR-0006 the runtime never picks silently. Two bindings of
	// the same connector+kind → resolver is nil; the sandbox returns
	// `binding_required`. The agent's tool-result envelope carries
	// the names so the user can disambiguate.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installCredentialConnector(t, store, "github://test/api-echo", "1.0.0", "oauth2")
	actions := installFakeAction(t, "github://test/api-echo", "1.0.0", hash,
		[]string{"call"}, []string{"call"})

	bs := bindingStoreWith(t, "oauth2/oauth-echo/work", "oauth2",
		"github://test/api-echo", []byte("v1"))
	if err := bs.Put(context.Background(), binding.Binding{
		Name:         "oauth2/oauth-echo/personal",
		Kind:         "oauth2",
		Service:      "oauth-echo",
		Identity:     "personal",
		ConnectorFQN: "github://test/api-echo",
	}, []byte("v2"), binding.PutCreate); err != nil {
		t.Fatalf("seed second binding: %v", err)
	}

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/api-echo": {resp: map[string]any{}}}

	exec := NewSandboxExecutor(actions, store, rt, bs)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	c := rt.connectors["github://test/api-echo"]
	if len(c.calls) == 0 || c.calls[0].CredentialResolver != nil {
		t.Errorf("expected nil CredentialResolver on ambiguous binding; got %v", c.calls[0].CredentialResolver)
	}
}

func TestSandboxExecutor_NoResolver_WhenBindingsNil(t *testing.T) {
	// Mirrors a dev-mode launch with no vault wired. resolver is nil;
	// the host fails closed at credential reference time.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installCredentialConnector(t, store, "github://test/api-echo", "1.0.0", "oauth2")
	actions := installFakeAction(t, "github://test/api-echo", "1.0.0", hash,
		[]string{"call"}, []string{"call"})

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/api-echo": {resp: map[string]any{}}}

	exec := NewSandboxExecutor(actions, store, rt, nil) // nil Bindings
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	c := rt.connectors["github://test/api-echo"]
	if len(c.calls) == 0 || c.calls[0].CredentialResolver != nil {
		t.Errorf("expected nil CredentialResolver with no Bindings; got %v", c.calls[0].CredentialResolver)
	}
}

func TestSandboxExecutor_BindingRequiredFromSandboxBecomesFailure(t *testing.T) {
	// When the host returns sandbox.ClassBindingRequired, the executor
	// converts it into failure.BindingRequired so the agent's
	// tool-result envelope carries the closed-taxonomy class per
	// ADR-0010.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installCredentialConnector(t, store, "github://test/api-echo", "1.0.0", "oauth2")
	actions := installFakeAction(t, "github://test/api-echo", "1.0.0", hash,
		[]string{"call"}, []string{"call"})

	bs := &binding.VaultStore{Vault: vault.NewMemVault()} // empty

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/api-echo": {
		err: &sandbox.Error{
			Class:    sandbox.ClassBindingRequired,
			Boundary: sandbox.BoundarySandbox,
			Message:  "no binding",
		},
	}}

	exec := NewSandboxExecutor(actions, store, rt, bs)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	res, err := exec.Execute(context.Background(), "test-action", nil)
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if res.Failure == nil {
		t.Fatal("expected Result.Failure")
	}
	if string(res.Failure.Class()) != "binding_required" {
		t.Errorf("class = %q, want binding_required", res.Failure.Class())
	}
}

// stubResolverForTests retains coverage on the credential package's
// VaultResolver sentinel-error path.
func TestVaultResolver_Resolve_ReturnsSentinelOnMissing(t *testing.T) {
	r := &credential.VaultResolver{
		Vault:        vault.NewMemVault(),
		VaultPath:    "missing/path",
		ExpectedKind: "oauth2",
	}
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrBindingMissing) {
		t.Errorf("err = %v, want ErrBindingMissing", err)
	}
}
