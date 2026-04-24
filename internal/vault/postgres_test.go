package vault_test

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

func TestPostgresVault_ImplementsInterface(t *testing.T) {
	var _ vault.Vault = (*vault.PostgresVault)(nil)
}

func TestNewPostgresVault(t *testing.T) {
	v := vault.NewPostgresVault(nil)
	if v == nil {
		t.Fatal("expected non-nil PostgresVault")
	}
}

func TestNewPostgresVaultForTable(t *testing.T) {
	v := vault.NewPostgresVaultForTable(nil, "system_vault_secrets")
	if v == nil {
		t.Fatal("expected non-nil PostgresVault")
	}
}
