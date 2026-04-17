package vault_test

import (
	"testing"

	"github.com/ALRubinger/aileron/core/vault"
)

func TestPostgresVault_ImplementsInterface(t *testing.T) {
	// Compile-time check that PostgresVault satisfies the Vault interface.
	var _ vault.Vault = (*vault.PostgresVault)(nil)
}

func TestNewPostgresVault(t *testing.T) {
	// NewPostgresVault with nil pool — should not panic during construction.
	// Operations will fail at query time, not at construction.
	v := vault.NewPostgresVault(nil)
	if v == nil {
		t.Fatal("expected non-nil PostgresVault")
	}
}
