package action

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// TestSandboxExecutor_ResultCarriesConnectorProvenance asserts a successful
// Result surfaces the actor provenance for the walk-back an
// output.materialized event records (issue #1753): the connector build
// (version + content hash) from the manifest's requires entry, and the
// non-secret identity label + binding name when a single credential binding
// resolves. The credential value itself never appears on the Result.
func TestSandboxExecutor_ResultCarriesConnectorProvenance(t *testing.T) {
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installCredentialConnector(t, store, "github://test/api-echo", "2.3.1", "api_key")
	actions := installFakeAction(t, "github://test/api-echo", "2.3.1", hash,
		[]string{"call"}, []string{"call"})

	bs := bindingStoreWith(t, "api_key/api-echo/work", "api_key",
		"github://test/api-echo", []byte("test-token"))

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/api-echo": {resp: map[string]any{"ok": true}}}

	exec := NewSandboxExecutor(actions, store, rt, bs)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	res, err := exec.Execute(context.Background(), "test-action", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Failure != nil {
		t.Fatalf("unexpected failure: %v", res.Failure)
	}
	p := res.Provenance
	if p.ConnectorVersion != "2.3.1" {
		t.Errorf("ConnectorVersion = %q, want 2.3.1 (from the manifest requires entry)", p.ConnectorVersion)
	}
	if p.ConnectorHash != hash {
		t.Errorf("ConnectorHash = %q, want %q", p.ConnectorHash, hash)
	}
	if p.IdentityLabel != "work" {
		t.Errorf("IdentityLabel = %q, want work (the binding's identity segment)", p.IdentityLabel)
	}
	if p.CredentialBinding != "api_key/api-echo/work" {
		t.Errorf("CredentialBinding = %q, want api_key/api-echo/work", p.CredentialBinding)
	}
	// The credential value must never leak onto the provenance.
	if p.IdentityLabel == "test-token" || p.CredentialBinding == "test-token" {
		t.Error("provenance must carry references, never the credential value")
	}
}

// TestSandboxExecutor_ResultOmitsIdentity_WhenNoBinding asserts a connector
// that declares no credential capability produces a Result whose provenance
// carries the connector build but omits (leaves empty) the identity and
// binding — an honest omission, not a guess.
func TestSandboxExecutor_ResultOmitsIdentity_WhenNoBinding(t *testing.T) {
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	hash := installFakeConnector(t, store, "github://test/echo", "1.4.0")
	actions := installFakeAction(t, "github://test/echo", "1.4.0", hash,
		[]string{"call"}, []string{"call"})

	// A binding exists but the connector declares no [capabilities.credential],
	// so the executor resolves no identity for it.
	bs := bindingStoreWith(t, "api_key/echo/work", "api_key",
		"github://test/echo", []byte("nope"))

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{"github://test/echo": {resp: map[string]any{"ok": true}}}

	exec := NewSandboxExecutor(actions, store, rt, bs)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	res, err := exec.Execute(context.Background(), "test-action", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	p := res.Provenance
	if p.ConnectorVersion != "1.4.0" || p.ConnectorHash != hash {
		t.Errorf("connector build = %q/%q, want 1.4.0/%q", p.ConnectorVersion, p.ConnectorHash, hash)
	}
	if p.IdentityLabel != "" || p.CredentialBinding != "" {
		t.Errorf("identity/binding = %q/%q, want both empty for a credential-less action", p.IdentityLabel, p.CredentialBinding)
	}
}

// TestSandboxExecutor_ResultOmitsIdentity_WhenAmbiguousBinding asserts that an
// ambiguous binding (two matching entries) yields no identity attribution:
// the runtime never picks one silently (ADR-0006), so the provenance carries
// the connector build but omits identity/binding.
func TestSandboxExecutor_ResultOmitsIdentity_WhenAmbiguousBinding(t *testing.T) {
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
	rt.connectors = map[string]*fakeConnector{"github://test/api-echo": {resp: map[string]any{"ok": true}}}

	exec := NewSandboxExecutor(actions, store, rt, bs)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	res, err := exec.Execute(context.Background(), "test-action", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Provenance.IdentityLabel != "" || res.Provenance.CredentialBinding != "" {
		t.Errorf("ambiguous binding must not attribute a single identity; got %q/%q",
			res.Provenance.IdentityLabel, res.Provenance.CredentialBinding)
	}
	if res.Provenance.ConnectorHash != hash {
		t.Errorf("connector hash should still surface; got %q", res.Provenance.ConnectorHash)
	}
}
