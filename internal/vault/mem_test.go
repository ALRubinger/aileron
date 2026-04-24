package vault

import (
	"context"
	"testing"
)

func TestMemVault_PutAndGet(t *testing.T) {
	v := NewMemVault()
	ctx := context.Background()

	err := v.Put(ctx, "connectors/github/default", []byte("ghp_token123"), Metadata{
		Type: "api_key",
		Labels: map[string]string{"connector": "github"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	secret, err := v.Get(ctx, "connectors/github/default")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(secret.Value) != "ghp_token123" {
		t.Errorf("Value = %q, want %q", string(secret.Value), "ghp_token123")
	}
	if secret.Metadata.Type != "api_key" {
		t.Errorf("Metadata.Type = %q, want %q", secret.Metadata.Type, "api_key")
	}
	if secret.Path != "connectors/github/default" {
		t.Errorf("Path = %q, want %q", secret.Path, "connectors/github/default")
	}
}

func TestMemVault_GetNotFound(t *testing.T) {
	v := NewMemVault()
	_, err := v.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent secret")
	}
}

func TestMemVault_Delete(t *testing.T) {
	v := NewMemVault()
	ctx := context.Background()

	v.Put(ctx, "test/path", []byte("value"), Metadata{Type: "api_key"})
	v.Delete(ctx, "test/path")

	_, err := v.Get(ctx, "test/path")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestMemVault_PutOverwrite(t *testing.T) {
	v := NewMemVault()
	ctx := context.Background()

	v.Put(ctx, "key", []byte("v1"), Metadata{Type: "api_key"})
	v.Put(ctx, "key", []byte("v2"), Metadata{Type: "api_key"})

	secret, _ := v.Get(ctx, "key")
	if string(secret.Value) != "v2" {
		t.Errorf("Value = %q, want %q", string(secret.Value), "v2")
	}
}
