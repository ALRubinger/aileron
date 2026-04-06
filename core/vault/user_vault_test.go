package vault

import (
	"bytes"
	"context"
	"testing"
)

func TestUserScopedVault_WithKEK_EncryptsOnPut(t *testing.T) {
	kek := testKEK(t)
	inner := NewMemVault()
	uv := NewUserScopedVault(inner, kek)

	ctx := context.Background()
	path := "connected-accounts/usr_1/gmail"
	value := []byte(`{"refresh_token":"secret-token"}`)

	if err := uv.Put(ctx, path, value, Metadata{Type: "oauth_refresh_token"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Inner vault should have ciphertext, not plaintext.
	raw, err := inner.Get(ctx, path)
	if err != nil {
		t.Fatalf("inner.Get: %v", err)
	}
	if bytes.Equal(raw.Value, value) {
		t.Fatal("inner vault should store ciphertext, not plaintext")
	}
	if raw.Metadata.Labels[EncryptedLabel] != "true" {
		t.Fatal("expected encrypted label to be set")
	}
}

func TestUserScopedVault_WithKEK_RoundTrip(t *testing.T) {
	kek := testKEK(t)
	inner := NewMemVault()
	uv := NewUserScopedVault(inner, kek)

	ctx := context.Background()
	path := "connected-accounts/usr_1/gmail"
	value := []byte(`{"refresh_token":"secret-token"}`)

	if err := uv.Put(ctx, path, value, Metadata{Type: "oauth_refresh_token"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := uv.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got.Value, value) {
		t.Fatalf("value = %q, want %q", got.Value, value)
	}
}

func TestUserScopedVault_WithKEK_WrongKEKFails(t *testing.T) {
	kek1 := testKEK(t)
	kek2 := testKEK(t)
	inner := NewMemVault()

	uv1 := NewUserScopedVault(inner, kek1)
	uv2 := NewUserScopedVault(inner, kek2)

	ctx := context.Background()
	path := "connected-accounts/usr_1/gmail"

	if err := uv1.Put(ctx, path, []byte("secret"), Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, err := uv2.Get(ctx, path)
	if err == nil {
		t.Fatal("expected error when decrypting with wrong KEK")
	}
}

func TestUserScopedVault_NilKEK_Passthrough(t *testing.T) {
	inner := NewMemVault()
	uv := NewUserScopedVault(inner, nil)

	ctx := context.Background()
	path := "connectors/github/default"
	value := []byte("api-key-plaintext")

	if err := uv.Put(ctx, path, value, Metadata{Type: "api_key"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Inner vault should have plaintext (no encryption).
	raw, err := inner.Get(ctx, path)
	if err != nil {
		t.Fatalf("inner.Get: %v", err)
	}
	if !bytes.Equal(raw.Value, value) {
		t.Fatal("nil KEK should passthrough without encryption")
	}
	if raw.Metadata.Labels != nil && raw.Metadata.Labels[EncryptedLabel] == "true" {
		t.Fatal("should not set encrypted label when KEK is nil")
	}

	// Get through UserScopedVault should also return plaintext.
	got, err := uv.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got.Value, value) {
		t.Fatal("nil KEK passthrough Get failed")
	}
}

func TestUserScopedVault_NilKEK_EncryptedSecretErrors(t *testing.T) {
	kek := testKEK(t)
	inner := NewMemVault()

	// Store an encrypted secret.
	uvEncrypt := NewUserScopedVault(inner, kek)
	ctx := context.Background()
	path := "connected-accounts/usr_1/gmail"
	if err := uvEncrypt.Put(ctx, path, []byte("secret"), Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Try to read it with nil KEK — should error.
	uvNoKey := NewUserScopedVault(inner, nil)
	_, err := uvNoKey.Get(ctx, path)
	if err == nil {
		t.Fatal("expected error reading encrypted secret without KEK")
	}
}

func TestUserScopedVault_Delete(t *testing.T) {
	kek := testKEK(t)
	inner := NewMemVault()
	uv := NewUserScopedVault(inner, kek)

	ctx := context.Background()
	path := "test/secret"

	if err := uv.Put(ctx, path, []byte("secret"), Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := uv.Delete(ctx, path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := uv.Get(ctx, path)
	if err == nil {
		t.Fatal("expected error after deletion")
	}
}

func TestUserScopedVault_PreservesMetadata(t *testing.T) {
	kek := testKEK(t)
	inner := NewMemVault()
	uv := NewUserScopedVault(inner, kek)

	ctx := context.Background()
	meta := Metadata{
		Type: "oauth_refresh_token",
		Labels: map[string]string{
			"provider": "gmail",
			"user_id":  "usr_1",
		},
	}

	if err := uv.Put(ctx, "test/meta", []byte("secret"), meta); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := uv.Get(ctx, "test/meta")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Metadata.Type != "oauth_refresh_token" {
		t.Fatalf("type = %q, want %q", got.Metadata.Type, "oauth_refresh_token")
	}
	if got.Metadata.Labels["provider"] != "gmail" {
		t.Fatalf("provider = %q, want %q", got.Metadata.Labels["provider"], "gmail")
	}
}

func TestIsEncrypted(t *testing.T) {
	if IsEncrypted(Metadata{}) {
		t.Fatal("empty metadata should not be encrypted")
	}
	if IsEncrypted(Metadata{Labels: map[string]string{"foo": "bar"}}) {
		t.Fatal("unrelated labels should not be encrypted")
	}
	if !IsEncrypted(Metadata{Labels: map[string]string{EncryptedLabel: "true"}}) {
		t.Fatal("encrypted label should be detected")
	}
}
