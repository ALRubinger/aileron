package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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

// installActionWithBinding writes an action manifest containing a
// `[[bindings]]` entry that points the named connector at the given
// vault path.
func installActionWithBinding(t *testing.T, fqn, version, hash, vaultPath string) *Store {
	t.Helper()
	dir := t.TempDir()
	manifest := `+++
name = "test-action"
version = "1.0.0"
source = "hub://test/test-action@1.0.0"

[[requires.connectors]]
name = "` + fqn + `"
version = "` + version + `"
hash = "` + hash + `"
capabilities = ["call"]

[match]
intent = "test"

[[execute]]
id = "call"
connector = "` + fqn + `"
op = "call"

[[bindings]]
connector = "` + fqn + `"
vault_path = "` + vaultPath + `"
+++

# Test Action
`
	if err := os.WriteFile(filepath.Join(dir, "test-action.md"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write action: %v", err)
	}
	store := NewStore(dir)
	if _, err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return store
}

func TestSandboxExecutor_WiresResolver_WhenBindingMatches(t *testing.T) {
	// ADR-0005 credential mediation: when the connector manifest
	// declares [capabilities.credential] and the action declares a
	// matching [[bindings]] entry, the executor wires a per-step
	// credential.Resolver into sandbox.Call. The resolver, when
	// called, returns the bound vault entry.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installCredentialConnector(t, store, "github://test/oauth-echo", "1.0.0", "oauth2")
	actions := installActionWithBinding(t, "github://test/oauth-echo", "1.0.0", hash, "oauth2/test")

	v := vault.NewMemVault()
	if err := v.Put(context.Background(), "oauth2/test",
		[]byte("test-token"), vault.Metadata{Type: "oauth2"}); err != nil {
		t.Fatalf("vault.Put: %v", err)
	}

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/oauth-echo": {resp: map[string]any{"ok": true}}}

	exec := NewSandboxExecutor(actions, store, rt, v)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	c := rt.connectors["github://test/oauth-echo"]
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
	if cred.Kind != "oauth2" {
		t.Errorf("Resolve.Kind = %q, want oauth2", cred.Kind)
	}
}

func TestSandboxExecutor_NoResolver_WhenConnectorDeclaresNoCredential(t *testing.T) {
	// Connector manifest declares no [capabilities.credential]; even
	// when a binding is mistakenly authored, the executor passes nil
	// because no credential mediation is needed for this connector.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.0.0")
	actions := installActionWithBinding(t, "github://test/echo", "1.0.0", hash, "oauth2/anything")

	v := vault.NewMemVault()
	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/echo": {resp: map[string]any{}}}

	exec := NewSandboxExecutor(actions, store, rt, v)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	c := rt.connectors["github://test/echo"]
	if len(c.calls) == 0 || c.calls[0].CredentialResolver != nil {
		t.Errorf("expected nil CredentialResolver; got %v", c.calls[0].CredentialResolver)
	}
}

func TestSandboxExecutor_NoResolver_WhenActionLacksBinding(t *testing.T) {
	// Connector manifest declares [capabilities.credential] but the
	// action has no [[bindings]] entry → resolver is nil. The host
	// will return `binding_required` if the connector tries to use
	// the credential path at runtime; the executor itself does not
	// pre-empt the call.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installCredentialConnector(t, store, "github://test/oauth-echo", "1.0.0", "oauth2")
	// Same action but without the [[bindings]] block.
	actions := installFakeAction(t, "github://test/oauth-echo", "1.0.0", hash,
		[]string{"call"}, []string{"call"})

	v := vault.NewMemVault()
	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/oauth-echo": {resp: map[string]any{}}}

	exec := NewSandboxExecutor(actions, store, rt, v)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	c := rt.connectors["github://test/oauth-echo"]
	if len(c.calls) == 0 || c.calls[0].CredentialResolver != nil {
		t.Errorf("expected nil CredentialResolver when no binding; got %v", c.calls[0].CredentialResolver)
	}
}

func TestSandboxExecutor_NoResolver_WhenVaultMissing(t *testing.T) {
	// Connector declares credential, action declares binding, but the
	// executor has no Vault wired. Mirrors a dev-mode launch where the
	// vault hasn't been unlocked; the resolver field is nil and the
	// host fails closed at credential reference time.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installCredentialConnector(t, store, "github://test/oauth-echo", "1.0.0", "oauth2")
	actions := installActionWithBinding(t, "github://test/oauth-echo", "1.0.0", hash, "oauth2/test")

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/oauth-echo": {resp: map[string]any{}}}

	exec := NewSandboxExecutor(actions, store, rt, nil) // nil Vault
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	if _, err := exec.Execute(context.Background(), "test-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	c := rt.connectors["github://test/oauth-echo"]
	if len(c.calls) == 0 || c.calls[0].CredentialResolver != nil {
		t.Errorf("expected nil CredentialResolver with no Vault; got %v", c.calls[0].CredentialResolver)
	}
}

func TestSandboxExecutor_BindingRequiredFromSandboxBecomesFailure(t *testing.T) {
	// When the host returns sandbox.ClassBindingRequired, the executor
	// converts it into failure.BindingRequired so the agent's
	// tool-result envelope carries the closed-taxonomy class per
	// ADR-0010.
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installCredentialConnector(t, store, "github://test/oauth-echo", "1.0.0", "oauth2")
	actions := installActionWithBinding(t, "github://test/oauth-echo", "1.0.0", hash, "oauth2/test")

	v := vault.NewMemVault() // empty vault — no entry

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/oauth-echo": {
		err: &sandbox.Error{
			Class:    sandbox.ClassBindingRequired,
			Boundary: sandbox.BoundarySandbox,
			Message:  "no binding",
		},
	}}

	exec := NewSandboxExecutor(actions, store, rt, v)
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

func TestBindingFor_NilManifestReturnsNil(t *testing.T) {
	if got := bindingFor(nil, "github://x/y"); got != nil {
		t.Errorf("bindingFor(nil) = %v, want nil", got)
	}
}

func TestBindingFor_UnmatchedConnectorReturnsNil(t *testing.T) {
	m := &Manifest{Bindings: []Binding{{Connector: "github://a/b", VaultPath: "x"}}}
	if got := bindingFor(m, "github://other/y"); got != nil {
		t.Errorf("bindingFor unmatched = %v, want nil", got)
	}
}

// stubResolverForTests is a Resolver that the executor pre-populates;
// this test ensures the sentinel-error path stays Is-comparable.
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
