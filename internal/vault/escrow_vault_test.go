package vault

import (
	"context"
	"errors"
	"testing"
)

func TestDenyPlaintextVault_Get(t *testing.T) {
	v := NewDenyPlaintextVault()
	_, err := v.Get(context.Background(), "connected-accounts/usr_1/gmail")
	if err == nil {
		t.Fatal("expected error from DenyPlaintextVault.Get")
	}
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Errorf("expected ErrCredentialUnavailable, got: %v", err)
	}
}

func TestDenyPlaintextVault_Put(t *testing.T) {
	v := NewDenyPlaintextVault()
	err := v.Put(context.Background(), "path", []byte("data"), Metadata{})
	if err == nil {
		t.Fatal("expected error from DenyPlaintextVault.Put")
	}
}

func TestDenyPlaintextVault_Delete(t *testing.T) {
	v := NewDenyPlaintextVault()
	err := v.Delete(context.Background(), "path")
	if err == nil {
		t.Fatal("expected error from DenyPlaintextVault.Delete")
	}
}
