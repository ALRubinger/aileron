package app

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

func TestNewLocalEncryptedVault(t *testing.T) {
	v, err := newLocalEncryptedVault()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v == nil {
		t.Fatal("expected non-nil vault")
	}
}

func TestNewLocalEncryptedVault_RoundTrip(t *testing.T) {
	v, err := newLocalEncryptedVault()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx := context.Background()
	path := "test/secret"
	value := []byte("my-secret-token")

	if err := v.Put(ctx, path, value, vault.Metadata{Type: "token"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := v.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Value) != "my-secret-token" {
		t.Fatalf("expected my-secret-token, got %s", got.Value)
	}
}

func TestNewLocalEncryptedVault_UniqueKEKs(t *testing.T) {
	// Two vaults should have different KEKs — a secret encrypted by one
	// should not be decryptable by the other.
	v1, _ := newLocalEncryptedVault()
	v2, _ := newLocalEncryptedVault()

	ctx := context.Background()
	path := "test/secret"

	v1.Put(ctx, path, []byte("secret"), vault.Metadata{})

	// v2 has a different KEK and different MemVault — Get should fail (not found).
	_, err := v2.Get(ctx, path)
	if err == nil {
		t.Fatal("expected error — different vault instances should be independent")
	}
}
