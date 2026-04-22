package vault

import (
	"testing"
)

func TestPostgresVault_ImplementsInterface(t *testing.T) {
	// Compile-time check that PostgresVault satisfies the Vault interface.
	var _ Vault = (*PostgresVault)(nil)
}

func TestNewPostgresVault(t *testing.T) {
	// NewPostgresVault with nil pool — should not panic during construction.
	// Operations will fail at query time, not at construction.
	v := NewPostgresVault(nil)
	if v == nil {
		t.Fatal("expected non-nil PostgresVault")
	}
	if v.table != "vault_secrets" {
		t.Errorf("table = %q, want vault_secrets", v.table)
	}
}

func TestNewPostgresVaultForTable(t *testing.T) {
	v := NewPostgresVaultForTable(nil, "system_vault_secrets")
	if v == nil {
		t.Fatal("expected non-nil PostgresVault")
	}
	if v.table != "system_vault_secrets" {
		t.Errorf("table = %q, want system_vault_secrets", v.table)
	}
}

func TestPostgresVault_SQLHelpers(t *testing.T) {
	v := NewPostgresVault(nil)

	if got := v.selectSQL(); got != "SELECT value, metadata FROM vault_secrets WHERE path = $1" {
		t.Errorf("selectSQL = %q", got)
	}
	if got := v.upsertSQL(); got != "INSERT INTO vault_secrets (path, value, metadata) VALUES ($1, $2, $3) ON CONFLICT (path) DO UPDATE SET value = $2, metadata = $3, updated_at = now()" {
		t.Errorf("upsertSQL = %q", got)
	}
	if got := v.deleteSQL(); got != "DELETE FROM vault_secrets WHERE path = $1" {
		t.Errorf("deleteSQL = %q", got)
	}
}

func TestPostgresVault_SQLHelpers_CustomTable(t *testing.T) {
	v := NewPostgresVaultForTable(nil, "system_vault_secrets")

	if got := v.selectSQL(); got != "SELECT value, metadata FROM system_vault_secrets WHERE path = $1" {
		t.Errorf("selectSQL = %q", got)
	}
	if got := v.upsertSQL(); got != "INSERT INTO system_vault_secrets (path, value, metadata) VALUES ($1, $2, $3) ON CONFLICT (path) DO UPDATE SET value = $2, metadata = $3, updated_at = now()" {
		t.Errorf("upsertSQL = %q", got)
	}
	if got := v.deleteSQL(); got != "DELETE FROM system_vault_secrets WHERE path = $1" {
		t.Errorf("deleteSQL = %q", got)
	}
}
