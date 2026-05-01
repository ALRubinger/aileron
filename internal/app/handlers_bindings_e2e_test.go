package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/vault"
)

// fakeRuntime / fakeConnector mirror the action package's fake; they
// record the sandbox.Call so the test can assert on the
// CredentialResolver wired through to the executor.
type fakeRuntime struct {
	connectors map[string]*fakeConnector
}

type fakeConnector struct {
	calls []sandbox.Call
}

func (r *fakeRuntime) Compile(_ context.Context, m *cstore.Manifest, _ []byte) (sandbox.Connector, error) {
	if r.connectors == nil {
		r.connectors = map[string]*fakeConnector{}
	}
	c, ok := r.connectors[m.Connector.Name]
	if !ok {
		c = &fakeConnector{}
		r.connectors[m.Connector.Name] = c
	}
	return c, nil
}
func (r *fakeRuntime) Close(_ context.Context) error { return nil }
func (c *fakeConnector) Invoke(_ context.Context, call sandbox.Call) (sandbox.Result, error) {
	c.calls = append(c.calls, call)
	return sandbox.Result{Output: map[string]any{"ok": true}}, nil
}
func (c *fakeConnector) Close(_ context.Context) error { return nil }

// installConnectorWithCredential writes a connector manifest into the
// cstore that declares an `api_key` credential capability.
func installConnectorWithCredential(t *testing.T, store *cstore.Store, fqn, version string) string {
	t.Helper()
	manifest := []byte(`[connector]
name = "` + fqn + `"
version = "` + version + `"
publisher = "test"

[capabilities.credential]
kind = "api_key"
scope = "issues:write"
`)
	tb := &cstore.Tarball{
		BinaryName: "connector.wasm",
		Binary:     []byte("BIN"),
		Manifest:   manifest,
		Signature:  []byte("SIG"),
	}
	hashHex := tb.CanonicalHashHex()
	dir := filepath.Join(store.Root(), "connectors", "sha256", hashHex)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "connector.wasm"), tb.Binary, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "manifest.toml"), tb.Manifest, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "signature.sig"), tb.Signature, 0o644)
	if err := store.RebuildIndex(); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	return "sha256:" + hashHex
}

// installAction writes an action manifest at the given connector FQN
// (no [[bindings]] block — bindings live in the binding store now).
func installAction(t *testing.T, fqn, version, hash string) *action.Store {
	t.Helper()
	dir := t.TempDir()
	body := `+++
name = "echo-action"
version = "1.0.0"
source = "hub://test/echo-action@1.0.0"

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
+++

# Echo action
`
	if err := os.WriteFile(filepath.Join(dir, "echo-action.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write action: %v", err)
	}
	store := action.NewStore(dir)
	if _, err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return store
}

// TestBinding_E2E_SetupThenResolve drives the full chain:
//
//  1. HTTP POST /v1/bindings/setup creates an api_key binding.
//  2. SandboxExecutor.Execute invokes the bound connector.
//  3. The fake connector observes a non-nil CredentialResolver.
//  4. Calling Resolve on it returns the value submitted in step 1
//     — proving the bytes flowed through vault → store → resolver
//     without the executor or test seeing them at any other point.
//
// This is the integration assertion the plan calls for.
func TestBinding_E2E_SetupThenResolve(t *testing.T) {
	cstoreStore := cstore.NewStore(t.TempDir())
	hash := installConnectorWithCredential(t, cstoreStore, "github://aileron/linear", "1.0.0")
	actionStore := installAction(t, "github://aileron/linear", "1.0.0", hash)

	v := vault.NewMemVault()
	bindings := &binding.VaultStore{Vault: v}
	auditStore := audit.NewMemStore()

	srv := &apiServer{
		bindings:      bindings,
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, nil),
		installer:     &cstore.Installer{Store: cstoreStore},
	}

	// 1. Bind via the HTTP API.
	body := `{
		"connector_fqn": "github://aileron/linear",
		"bindings": [
			{"identity": "team", "source": {"kind": "api_key", "value": "lin-secret"}}
		]
	}`
	rec := httptest.NewRecorder()
	srv.SetupBindings(rec,
		httptest.NewRequest(http.MethodPost, "/v1/bindings/setup", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 2. Run an action that uses the connector.
	rt := &fakeRuntime{}
	exec := action.NewSandboxExecutor(actionStore, cstoreStore, rt, bindings)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })
	if _, err := exec.Execute(context.Background(), "echo-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 3 + 4. The fake connector saw a wired resolver; resolving it
	// returns the bytes the user POSTed, validated against the kind.
	c := rt.connectors["github://aileron/linear"]
	if len(c.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(c.calls))
	}
	resolver := c.calls[0].CredentialResolver
	if resolver == nil {
		t.Fatal("CredentialResolver is nil; expected a wired resolver after setup")
	}
	cred, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(cred.Value) != "lin-secret" {
		t.Errorf("cred.Value = %q, want lin-secret", cred.Value)
	}
	if cred.Kind != "api_key" {
		t.Errorf("cred.Kind = %q, want api_key", cred.Kind)
	}

	// Audit trail records the binding lifecycle event without ever
	// embedding the credential bytes.
	events := dumpEvents(t, auditStore)
	if !containsEvent(events, "binding.created") {
		t.Error("expected binding.created event")
	}
	for _, e := range events {
		for _, v := range e.Payload {
			if s, ok := v.(string); ok && strings.Contains(s, "lin-secret") {
				t.Errorf("audit payload leaked credential bytes: %v", e.Payload)
			}
		}
	}
}

// TestBinding_E2E_NoBindingSurfacesAsBindingRequired drives the
// negative path: action invocation without a matching binding produces
// a nil resolver, and the sandbox boundary surfaces `binding_required`.
// Here we assert the resolver is nil at the executor level (the
// sandbox-side surface is covered by the credential_integration_test
// in internal/sandbox).
func TestBinding_E2E_NoBindingSurfacesAsBindingRequired(t *testing.T) {
	cstoreStore := cstore.NewStore(t.TempDir())
	hash := installConnectorWithCredential(t, cstoreStore, "github://aileron/linear", "1.0.0")
	actionStore := installAction(t, "github://aileron/linear", "1.0.0", hash)

	v := vault.NewMemVault()
	bindings := &binding.VaultStore{Vault: v} // no entries

	rt := &fakeRuntime{}
	exec := action.NewSandboxExecutor(actionStore, cstoreStore, rt, bindings)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })
	if _, err := exec.Execute(context.Background(), "echo-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	c := rt.connectors["github://aileron/linear"]
	if len(c.calls) == 0 || c.calls[0].CredentialResolver != nil {
		t.Errorf("expected nil CredentialResolver (no binding); got %v",
			c.calls[0].CredentialResolver)
	}
}

// TestBinding_E2E_AmbiguousResolutionYieldsNilResolver asserts that
// when the user has two bindings of the same connector+kind, the
// store fails fast (per ADR-0006: never silently pick) and the
// executor sees a nil resolver. The CLI surfaces the candidates to
// the user; the sandbox returns binding_required.
func TestBinding_E2E_AmbiguousResolutionYieldsNilResolver(t *testing.T) {
	cstoreStore := cstore.NewStore(t.TempDir())
	hash := installConnectorWithCredential(t, cstoreStore, "github://aileron/linear", "1.0.0")
	actionStore := installAction(t, "github://aileron/linear", "1.0.0", hash)

	bindings := &binding.VaultStore{Vault: vault.NewMemVault()}
	for _, identity := range []string{"work", "personal"} {
		if err := bindings.Put(context.Background(), binding.Binding{
			Name:         binding.Name("api_key/linear/" + identity),
			Kind:         "api_key",
			Service:      "linear",
			Identity:     identity,
			ConnectorFQN: "github://aileron/linear",
		}, []byte("v"), binding.PutCreate); err != nil {
			t.Fatalf("seed %s: %v", identity, err)
		}
	}

	rt := &fakeRuntime{}
	exec := action.NewSandboxExecutor(actionStore, cstoreStore, rt, bindings)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })
	if _, err := exec.Execute(context.Background(), "echo-action", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	c := rt.connectors["github://aileron/linear"]
	if len(c.calls) == 0 || c.calls[0].CredentialResolver != nil {
		t.Errorf("expected nil CredentialResolver on ambiguity; got %v",
			c.calls[0].CredentialResolver)
	}

	// And confirm the binding store itself surfaces the candidates so
	// the CLI/agent can guide the user.
	_, err := bindings.Resolve(context.Background(), "github://aileron/linear", "api_key")
	var ambErr *binding.AmbiguousError
	if !errorsAs(err, &ambErr) {
		t.Fatalf("Resolve err = %v, want AmbiguousError", err)
	}
	if len(ambErr.Candidates) != 2 {
		t.Errorf("len(Candidates) = %d, want 2", len(ambErr.Candidates))
	}
}

// errorsAs is a tiny local shim so this test file does not have to
// import errors and shadow the standard helper.
func errorsAs(err error, target any) bool {
	if err == nil {
		return false
	}
	type unwrappable interface{ Unwrap() error }
	for {
		if t, ok := target.(**binding.AmbiguousError); ok {
			if e, ok := err.(*binding.AmbiguousError); ok {
				*t = e
				return true
			}
		}
		u, ok := err.(unwrappable)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
}
