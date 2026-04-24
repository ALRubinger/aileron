package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"

	"github.com/ALRubinger/aileron/internal/crypto"
)

func testKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, crypto.KEKLen)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("generating KEK: %v", err)
	}
	return kek
}

func TestEncryptedVault_RoundTrip(t *testing.T) {
	kek := testKEK(t)
	inner := NewMemVault()
	ev, err := NewEncryptedVault(inner, kek)
	if err != nil {
		t.Fatalf("NewEncryptedVault: %v", err)
	}

	ctx := context.Background()
	path := "test/secret"
	value := []byte("my-oauth-refresh-token")
	meta := Metadata{Type: "oauth_refresh_token", Labels: map[string]string{"provider": "gmail"}}

	if err := ev.Put(ctx, path, value, meta); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := ev.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if !bytes.Equal(got.Value, value) {
		t.Fatalf("value = %q, want %q", got.Value, value)
	}
	if got.Metadata.Type != meta.Type {
		t.Fatalf("metadata type = %q, want %q", got.Metadata.Type, meta.Type)
	}
	if got.Metadata.Labels["provider"] != "gmail" {
		t.Fatalf("metadata label provider = %q, want %q", got.Metadata.Labels["provider"], "gmail")
	}
}

func TestEncryptedVault_InnerStoresCiphertext(t *testing.T) {
	kek := testKEK(t)
	inner := NewMemVault()
	ev, err := NewEncryptedVault(inner, kek)
	if err != nil {
		t.Fatalf("NewEncryptedVault: %v", err)
	}

	ctx := context.Background()
	path := "test/secret"
	value := []byte("plaintext-secret")

	if err := ev.Put(ctx, path, value, Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Read directly from inner vault — should be ciphertext, not plaintext.
	raw, err := inner.Get(ctx, path)
	if err != nil {
		t.Fatalf("inner.Get: %v", err)
	}

	if bytes.Equal(raw.Value, value) {
		t.Fatal("inner vault should store ciphertext, not plaintext")
	}

	// Ciphertext should be longer than plaintext (nonce + tag overhead).
	if len(raw.Value) <= len(value) {
		t.Fatal("ciphertext should be longer than plaintext")
	}
}

func TestEncryptedVault_WrongKEK(t *testing.T) {
	kek1 := testKEK(t)
	kek2 := testKEK(t)
	inner := NewMemVault()

	ev1, _ := NewEncryptedVault(inner, kek1)
	ev2, _ := NewEncryptedVault(inner, kek2)

	ctx := context.Background()
	path := "test/secret"

	if err := ev1.Put(ctx, path, []byte("secret"), Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, err := ev2.Get(ctx, path)
	if err == nil {
		t.Fatal("expected error when decrypting with wrong KEK")
	}
}

func TestEncryptedVault_Delete(t *testing.T) {
	kek := testKEK(t)
	inner := NewMemVault()
	ev, _ := NewEncryptedVault(inner, kek)

	ctx := context.Background()
	path := "test/secret"

	if err := ev.Put(ctx, path, []byte("secret"), Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := ev.Delete(ctx, path); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := ev.Get(ctx, path)
	if err == nil {
		t.Fatal("expected error after deletion")
	}
}

func TestNewEncryptedVault_InvalidKEKLength(t *testing.T) {
	inner := NewMemVault()
	_, err := NewEncryptedVault(inner, []byte("too-short"))
	if err == nil {
		t.Fatal("expected error for invalid KEK length")
	}
}

func TestNewEncryptedVault_NilInner(t *testing.T) {
	kek := testKEK(t)
	_, err := NewEncryptedVault(nil, kek)
	if err == nil {
		t.Fatal("expected error for nil inner vault")
	}
}

func TestEncryptedVault_EmptyValue(t *testing.T) {
	kek := testKEK(t)
	inner := NewMemVault()
	ev, _ := NewEncryptedVault(inner, kek)

	ctx := context.Background()
	path := "test/empty"

	if err := ev.Put(ctx, path, []byte{}, Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := ev.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if len(got.Value) != 0 {
		t.Fatalf("expected empty value, got %d bytes", len(got.Value))
	}
}

func TestEncryptedVault_MetadataNotEncrypted(t *testing.T) {
	kek := testKEK(t)
	inner := NewMemVault()
	ev, _ := NewEncryptedVault(inner, kek)

	ctx := context.Background()
	path := "test/meta"
	meta := Metadata{
		Type:        "api_key",
		Environment: "production",
		Labels:      map[string]string{"connector": "stripe"},
	}

	if err := ev.Put(ctx, path, []byte("secret"), meta); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Metadata should be readable directly from inner vault.
	raw, err := inner.Get(ctx, path)
	if err != nil {
		t.Fatalf("inner.Get: %v", err)
	}

	if raw.Metadata.Type != "api_key" {
		t.Fatalf("inner metadata type = %q, want %q", raw.Metadata.Type, "api_key")
	}
	if raw.Metadata.Environment != "production" {
		t.Fatalf("inner metadata env = %q, want %q", raw.Metadata.Environment, "production")
	}
	if raw.Metadata.Labels["connector"] != "stripe" {
		t.Fatalf("inner metadata label connector = %q, want %q", raw.Metadata.Labels["connector"], "stripe")
	}
}
