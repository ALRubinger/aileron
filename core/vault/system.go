package vault

import "github.com/jackc/pgx/v5/pgxpool"

// NewPostgresSystemVault creates a PostgreSQL-backed vault for infrastructure
// secrets (ADR-0020). It uses the system_vault_secrets table, separate from
// user secrets in vault_secrets.
//
// Infrastructure secrets (Slack bot tokens, webhook signing keys) are stored
// here because they have no associated user and must be readable by the server
// autonomously. Wrap with EncryptedVault for at-rest encryption using a
// server-managed key (AILERON_SYSTEM_VAULT_KEY).
func NewPostgresSystemVault(pool *pgxpool.Pool) *PostgresVault {
	return NewPostgresVaultForTable(pool, "system_vault_secrets")
}
