package vault_test

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/vault"
)

// listContract drives the contract every Vault implementation owes
// List: returns plaintext metadata for every Put entry, omits the
// value bytes, survives Delete, and tolerates an empty store.
func listContract(t *testing.T, v vault.Vault) {
	t.Helper()
	ctx := context.Background()

	// Empty store.
	got, err := v.List(ctx)
	if err != nil {
		t.Fatalf("List on empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty store returned %d entries; want 0", len(got))
	}

	// Two puts.
	if err := v.Put(ctx, "api_key/slack/work",
		[]byte("xoxb"), vault.Metadata{Type: "api_key", Labels: map[string]string{"identity": "work"}}); err != nil {
		t.Fatalf("Put work: %v", err)
	}
	if err := v.Put(ctx, "api_key/slack/personal",
		[]byte("xoxb-2"), vault.Metadata{Type: "api_key", Labels: map[string]string{"identity": "personal"}}); err != nil {
		t.Fatalf("Put personal: %v", err)
	}

	got, err = v.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(got))
	}
	paths := []string{got[0].Path, got[1].Path}
	sort.Strings(paths)
	if paths[0] != "api_key/slack/personal" || paths[1] != "api_key/slack/work" {
		t.Errorf("paths = %v", paths)
	}
	for _, e := range got {
		if e.Metadata.Type != "api_key" {
			t.Errorf("entry %s: Metadata.Type = %q, want api_key", e.Path, e.Metadata.Type)
		}
		// Entry by construction has no Value field — listing must never
		// expose ciphertext or plaintext bytes.
	}

	// After delete the entry disappears.
	if err := v.Delete(ctx, "api_key/slack/work"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err = v.List(ctx)
	if err != nil {
		t.Fatalf("List post-delete: %v", err)
	}
	if len(got) != 1 || got[0].Path != "api_key/slack/personal" {
		t.Errorf("post-delete entries = %+v, want only personal", got)
	}
}

func TestMemVault_List(t *testing.T) {
	listContract(t, vault.NewMemVault())
}

func TestFileVault_List(t *testing.T) {
	dir := t.TempDir()
	v, err := vault.NewFileVault(filepath.Join(dir, "vault.json"))
	if err != nil {
		t.Fatalf("NewFileVault: %v", err)
	}
	listContract(t, v)
}

func TestEncryptedVault_ListPassesThroughMetadata(t *testing.T) {
	// EncryptedVault is a decorator; List must work the same way the
	// inner vault works because metadata is not encrypted.
	dir := t.TempDir()
	inner, err := vault.NewFileVault(filepath.Join(dir, "vault.json"))
	if err != nil {
		t.Fatalf("NewFileVault: %v", err)
	}
	kek := make([]byte, crypto.KEKLen)
	for i := range kek {
		kek[i] = byte(i)
	}
	v, err := vault.NewEncryptedVault(inner, kek)
	if err != nil {
		t.Fatalf("NewEncryptedVault: %v", err)
	}
	listContract(t, v)
}

func TestUserScopedVault_ListWorksWithoutKEK(t *testing.T) {
	// Listing must not require an unlock per ADR-0011. The user-scoped
	// decorator with no KEK still serves List from the inner vault.
	mem := vault.NewMemVault()
	if err := mem.Put(context.Background(), "api_key/x/y",
		[]byte("ciphertext"), vault.Metadata{Type: "api_key"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v := vault.NewUserScopedVault(mem, nil)
	got, err := v.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Path != "api_key/x/y" {
		t.Errorf("entries = %+v", got)
	}
}

func TestDenyPlaintextVault_ListIsEmpty(t *testing.T) {
	got, err := vault.NewDenyPlaintextVault().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("entries = %+v, want empty", got)
	}
}
