package vault_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"

	"github.com/ALRubinger/aileron/core/crypto"
	"github.com/ALRubinger/aileron/core/vault"
)

func TestNewPostgresSystemVault(t *testing.T) {
	v := vault.NewPostgresSystemVault(nil)
	if v == nil {
		t.Fatal("expected non-nil vault")
	}
	// Verify it satisfies the Vault interface.
	var _ vault.Vault = v
}

func TestNewPostgresVaultForTable(t *testing.T) {
	v := vault.NewPostgresVaultForTable(nil, "custom_table")
	if v == nil {
		t.Fatal("expected non-nil vault")
	}
	var _ vault.Vault = v
}

// TestSystemVault_EncryptedRoundTrip verifies that wrapping a MemVault with
// EncryptedVault (simulating the production system vault pattern) correctly
// encrypts and decrypts infrastructure secrets.
func TestSystemVault_EncryptedRoundTrip(t *testing.T) {
	key := make([]byte, crypto.KEKLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating key: %v", err)
	}

	inner := vault.NewMemVault()
	sv, err := vault.NewEncryptedVault(inner, key)
	if err != nil {
		t.Fatalf("NewEncryptedVault: %v", err)
	}

	ctx := context.Background()
	path := "slack-workspaces/T001/bot-token"
	value := []byte("xoxb-test-bot-token")
	meta := vault.Metadata{
		Type:   "slack_bot_token",
		Labels: map[string]string{"team_id": "T001", "team_name": "test-workspace"},
	}

	if err := sv.Put(ctx, path, value, meta); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := sv.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if !bytes.Equal(got.Value, value) {
		t.Fatalf("value = %q, want %q", got.Value, value)
	}
	if got.Metadata.Type != "slack_bot_token" {
		t.Fatalf("metadata type = %q, want %q", got.Metadata.Type, "slack_bot_token")
	}
	if got.Metadata.Labels["team_id"] != "T001" {
		t.Fatalf("metadata team_id = %q, want %q", got.Metadata.Labels["team_id"], "T001")
	}
}

// TestSystemVault_InnerStoresCiphertext verifies that the encrypted system vault
// stores ciphertext in the backing store, not plaintext.
func TestSystemVault_InnerStoresCiphertext(t *testing.T) {
	key := make([]byte, crypto.KEKLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating key: %v", err)
	}

	inner := vault.NewMemVault()
	sv, err := vault.NewEncryptedVault(inner, key)
	if err != nil {
		t.Fatalf("NewEncryptedVault: %v", err)
	}

	ctx := context.Background()
	path := "slack-workspaces/T002/bot-token"
	value := []byte("xoxb-plaintext-bot-token")

	if err := sv.Put(ctx, path, value, vault.Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	raw, err := inner.Get(ctx, path)
	if err != nil {
		t.Fatalf("inner.Get: %v", err)
	}

	if bytes.Equal(raw.Value, value) {
		t.Fatal("inner vault should store ciphertext, not plaintext")
	}
}

// TestSystemVault_WrongKey verifies decryption fails with incorrect key.
func TestSystemVault_WrongKey(t *testing.T) {
	key1 := make([]byte, crypto.KEKLen)
	key2 := make([]byte, crypto.KEKLen)
	rand.Read(key1)
	rand.Read(key2)

	inner := vault.NewMemVault()
	sv1, _ := vault.NewEncryptedVault(inner, key1)
	sv2, _ := vault.NewEncryptedVault(inner, key2)

	ctx := context.Background()
	path := "slack-workspaces/T003/bot-token"

	if err := sv1.Put(ctx, path, []byte("xoxb-secret"), vault.Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, err := sv2.Get(ctx, path)
	if err == nil {
		t.Fatal("expected error when decrypting with wrong key")
	}
}

// TestSystemVault_Delete verifies secrets can be removed.
func TestSystemVault_Delete(t *testing.T) {
	key := make([]byte, crypto.KEKLen)
	rand.Read(key)

	inner := vault.NewMemVault()
	sv, _ := vault.NewEncryptedVault(inner, key)

	ctx := context.Background()
	path := "slack-workspaces/T004/bot-token"

	sv.Put(ctx, path, []byte("xoxb-delete-me"), vault.Metadata{})

	if err := sv.Delete(ctx, path); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := sv.Get(ctx, path)
	if err == nil {
		t.Fatal("expected error after deletion")
	}
}

// TestSystemVault_Overwrite verifies that reinstallation overwrites existing tokens.
func TestSystemVault_Overwrite(t *testing.T) {
	key := make([]byte, crypto.KEKLen)
	rand.Read(key)

	inner := vault.NewMemVault()
	sv, _ := vault.NewEncryptedVault(inner, key)

	ctx := context.Background()
	path := "slack-workspaces/T005/bot-token"

	sv.Put(ctx, path, []byte("xoxb-old-token"), vault.Metadata{Type: "slack_bot_token"})
	sv.Put(ctx, path, []byte("xoxb-new-token"), vault.Metadata{Type: "slack_bot_token"})

	got, err := sv.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if !bytes.Equal(got.Value, []byte("xoxb-new-token")) {
		t.Fatalf("value = %q, want %q", got.Value, "xoxb-new-token")
	}
}

// TestSystemVault_IndependentFromUserVault verifies the two vaults are isolated.
func TestSystemVault_IndependentFromUserVault(t *testing.T) {
	sysKey := make([]byte, crypto.KEKLen)
	userKey := make([]byte, crypto.KEKLen)
	rand.Read(sysKey)
	rand.Read(userKey)

	// Separate backing stores simulate separate database tables.
	sysInner := vault.NewMemVault()
	userInner := vault.NewMemVault()

	sysVault, _ := vault.NewEncryptedVault(sysInner, sysKey)
	userVault, _ := vault.NewEncryptedVault(userInner, userKey)

	ctx := context.Background()

	// Write to system vault.
	sysVault.Put(ctx, "slack-workspaces/T001/bot-token", []byte("xoxb-bot"), vault.Metadata{})

	// Write to user vault at a different path.
	userVault.Put(ctx, "connected-accounts/usr_123/slack", []byte("xoxp-user"), vault.Metadata{})

	// System vault should not have the user secret.
	_, err := sysVault.Get(ctx, "connected-accounts/usr_123/slack")
	if err == nil {
		t.Fatal("system vault should not contain user secrets")
	}

	// User vault should not have the system secret.
	_, err = userVault.Get(ctx, "slack-workspaces/T001/bot-token")
	if err == nil {
		t.Fatal("user vault should not contain system secrets")
	}
}
