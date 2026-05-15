package app

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/vault"
)

// TestEvaluateScopeDrift_NoGrantRecordMarksStale: a binding with an
// empty GrantedScopes set is migration-marked `no_grant_record` on
// the first install-after-upgrade so the user reauthorizes once and
// teaches us what was granted.
func TestEvaluateScopeDrift_NoGrantRecordMarksStale(t *testing.T) {
	b := binding.Binding{
		Name: "oauth2/google/work", Kind: "oauth2",
		Status: binding.StatusActive,
		// GrantedScopes is intentionally empty — pre-tracking state.
	}
	got := evaluateScopeDrift(b, []string{"https://www.googleapis.com/auth/drive"})
	if got == nil {
		t.Fatal("expected stale update, got nil")
	}
	if got.Status != binding.StatusStale {
		t.Errorf("Status = %q, want stale", got.Status)
	}
	if got.StaleReason != binding.StaleReasonNoGrantRecord {
		t.Errorf("StaleReason = %q, want %q", got.StaleReason, binding.StaleReasonNoGrantRecord)
	}
	if got.MissingScopes != nil {
		t.Errorf("MissingScopes = %v, want nil (no record to diff against)", got.MissingScopes)
	}
}

// TestEvaluateScopeDrift_AlreadyMigrationStaleNoChange: a binding
// already migration-marked stays put. UpdateMetadata is not free —
// it round-trips through the vault — so skipping no-op writes is the
// load-bearing reason for this branch.
func TestEvaluateScopeDrift_AlreadyMigrationStaleNoChange(t *testing.T) {
	b := binding.Binding{
		Name: "oauth2/google/work", Kind: "oauth2",
		Status:      binding.StatusStale,
		StaleReason: binding.StaleReasonNoGrantRecord,
	}
	if got := evaluateScopeDrift(b, []string{"x"}); got != nil {
		t.Errorf("re-evaluation produced a write: %+v", got)
	}
}

// TestEvaluateScopeDrift_DriftStampsMissingScopes: the binding's grant
// is missing at least one scope the manifest demands → stale with
// stale_reason=scope_drift, missing_scopes populated.
func TestEvaluateScopeDrift_DriftStampsMissingScopes(t *testing.T) {
	b := binding.Binding{
		Name: "oauth2/google/work", Kind: "oauth2",
		Status: binding.StatusActive,
		GrantedScopes: []string{
			"https://www.googleapis.com/auth/gmail.readonly",
		},
	}
	got := evaluateScopeDrift(b, []string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/drive",
	})
	if got == nil {
		t.Fatal("expected stale update, got nil")
	}
	if got.StaleReason != binding.StaleReasonScopeDrift {
		t.Errorf("StaleReason = %q, want scope_drift", got.StaleReason)
	}
	if !reflect.DeepEqual(got.MissingScopes,
		[]string{"https://www.googleapis.com/auth/drive"}) {
		t.Errorf("MissingScopes = %v", got.MissingScopes)
	}
}

// TestEvaluateScopeDrift_SameDriftStateNoChange: if a binding is
// already stale with the same MissingScopes list, the evaluator
// returns nil so we don't churn the vault on every reinstall.
func TestEvaluateScopeDrift_SameDriftStateNoChange(t *testing.T) {
	b := binding.Binding{
		Status:        binding.StatusStale,
		StaleReason:   binding.StaleReasonScopeDrift,
		GrantedScopes: []string{"a"},
		MissingScopes: []string{"b"},
	}
	if got := evaluateScopeDrift(b, []string{"a", "b"}); got != nil {
		t.Errorf("re-evaluation produced a write: %+v", got)
	}
}

// TestEvaluateScopeDrift_GrantNowSatisfiesClearsStale: a previously
// stale binding whose grant now covers every required scope (e.g.
// because the connector dropped its demand) flips back to active and
// the stale labels are zeroed.
func TestEvaluateScopeDrift_GrantNowSatisfiesClearsStale(t *testing.T) {
	b := binding.Binding{
		Status:        binding.StatusStale,
		StaleReason:   binding.StaleReasonScopeDrift,
		GrantedScopes: []string{"a", "b"},
		MissingScopes: []string{"c"},
	}
	got := evaluateScopeDrift(b, []string{"a"})
	if got == nil {
		t.Fatal("expected clear-stale update, got nil")
	}
	if got.Status != binding.StatusActive {
		t.Errorf("Status = %q, want active", got.Status)
	}
	if got.StaleReason != "" {
		t.Errorf("StaleReason = %q, want empty", got.StaleReason)
	}
	if got.MissingScopes != nil {
		t.Errorf("MissingScopes = %v, want nil", got.MissingScopes)
	}
}

// TestEvaluateScopeDrift_ActiveAndSatisfiedNoChange: the steady-state
// case — binding active, grant covers every required scope. Returns
// nil so no vault write happens.
func TestEvaluateScopeDrift_ActiveAndSatisfiedNoChange(t *testing.T) {
	b := binding.Binding{
		Status:        binding.StatusActive,
		GrantedScopes: []string{"a", "b"},
	}
	if got := evaluateScopeDrift(b, []string{"a"}); got != nil {
		t.Errorf("active+satisfied produced a write: %+v", got)
	}
}

// TestMakeScopeDriftHook_MarksMatchingBindingStale: the hook walks
// the binding store, finds bindings tagged with the installed
// connector's FQN, and stamps stale on the ones with drift.
func TestMakeScopeDriftHook_MarksMatchingBindingStale(t *testing.T) {
	store := &binding.VaultStore{Vault: vault.NewMemVault()}
	ctx := context.Background()
	mustPut := func(name binding.Name, fqn string, granted []string) {
		t.Helper()
		_, kind, service, identity, _ := binding.Parse(string(name))
		if err := store.Put(ctx, binding.Binding{
			Name: name, Kind: kind, Service: service, Identity: identity,
			ConnectorFQN: fqn, GrantedScopes: granted,
		}, []byte("envelope"), binding.PutCreate); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}
	mustPut("oauth2/google/work", "github://acme/google", []string{"a"})
	// Binding for a different connector — must NOT be touched.
	mustPut("oauth2/slack/work", "github://acme/slack", []string{"a"})

	hook := makeScopeDriftHook(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hook(ctx, "github://acme/google", []string{"a", "b"})

	got, err := store.Get(ctx, "oauth2/google/work")
	if err != nil {
		t.Fatalf("Get google: %v", err)
	}
	if got.Status != binding.StatusStale {
		t.Errorf("google binding not marked stale: status=%q", got.Status)
	}
	if !reflect.DeepEqual(got.MissingScopes, []string{"b"}) {
		t.Errorf("MissingScopes = %v, want [b]", got.MissingScopes)
	}

	untouched, err := store.Get(ctx, "oauth2/slack/work")
	if err != nil {
		t.Fatalf("Get slack: %v", err)
	}
	if untouched.Status == binding.StatusStale {
		t.Errorf("slack binding was incorrectly marked stale; hook scope leaked across FQNs")
	}
}

// TestMakeScopeDriftHook_SkipsNonOAuthBindings: api_key bindings have
// no OAuth grant to check; the hook must leave them untouched even
// when their connector's manifest declares OAuth scopes (it shouldn't,
// but if it did, the api_key binding kind is the wrong target).
func TestMakeScopeDriftHook_SkipsNonOAuthBindings(t *testing.T) {
	store := &binding.VaultStore{Vault: vault.NewMemVault()}
	ctx := context.Background()
	if err := store.Put(ctx, binding.Binding{
		Name: "api_key/linear/team", Kind: "api_key", Service: "linear",
		Identity: "team", ConnectorFQN: "github://acme/linear",
	}, []byte("v"), binding.PutCreate); err != nil {
		t.Fatalf("Put: %v", err)
	}
	hook := makeScopeDriftHook(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hook(ctx, "github://acme/linear", []string{"a", "b"})

	got, _ := store.Get(ctx, "api_key/linear/team")
	if got.Status == binding.StatusStale {
		t.Errorf("api_key binding incorrectly marked stale: %+v", got)
	}
}

// TestMakeScopeDriftHook_MigratesUntrackedBinding: a binding with
// no recorded GrantedScopes is migration-marked stale with
// no_grant_record on the first hook firing — proves the "mark all
// pre-existing bindings stale on migration" choice.
func TestMakeScopeDriftHook_MigratesUntrackedBinding(t *testing.T) {
	store := &binding.VaultStore{Vault: vault.NewMemVault()}
	ctx := context.Background()
	if err := store.Put(ctx, binding.Binding{
		Name: "oauth2/google/work", Kind: "oauth2", Service: "google",
		Identity: "work", ConnectorFQN: "github://acme/google",
		// No GrantedScopes — predates scope tracking.
	}, []byte("envelope"), binding.PutCreate); err != nil {
		t.Fatalf("Put: %v", err)
	}
	hook := makeScopeDriftHook(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hook(ctx, "github://acme/google", []string{"a"})

	got, _ := store.Get(ctx, "oauth2/google/work")
	if got.Status != binding.StatusStale {
		t.Errorf("untracked binding not migrated to stale: status=%q", got.Status)
	}
	if got.StaleReason != binding.StaleReasonNoGrantRecord {
		t.Errorf("StaleReason = %q, want no_grant_record", got.StaleReason)
	}
}

// TestMakeScopeDriftHook_NilStoreNoPanic: defensive — early callers
// during apiServer construction may invoke the hook before the
// binding store is wired.
func TestMakeScopeDriftHook_NilStoreNoPanic(t *testing.T) {
	hook := makeScopeDriftHook(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hook(context.Background(), "github://acme/x", []string{"a"})
	// No panic = pass.
}
