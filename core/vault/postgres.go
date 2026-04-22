package vault

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresVault is a PostgreSQL-backed implementation of the Vault interface.
// Stores secrets as byte values with JSON metadata. The table name is
// configurable to support separate storage for user secrets (vault_secrets)
// and infrastructure secrets (system_vault_secrets, ADR-0020).
type PostgresVault struct {
	pool  *pgxpool.Pool
	table string
}

// NewPostgresVault creates a Postgres-backed vault using the given connection pool.
// Secrets are stored in the vault_secrets table.
func NewPostgresVault(pool *pgxpool.Pool) *PostgresVault {
	return &PostgresVault{pool: pool, table: "vault_secrets"}
}

// NewPostgresVaultForTable creates a Postgres-backed vault that reads and writes
// to the specified table. The table must have the same schema as vault_secrets
// (path, value, metadata, created_at, updated_at).
func NewPostgresVaultForTable(pool *pgxpool.Pool, table string) *PostgresVault {
	return &PostgresVault{pool: pool, table: table}
}

func (v *PostgresVault) Get(ctx context.Context, path string) (Secret, error) {
	var value []byte
	var metaJSON []byte

	err := v.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT value, metadata FROM %s WHERE path = $1`, v.table), path,
	).Scan(&value, &metaJSON)

	if err != nil {
		if err == pgx.ErrNoRows {
			return Secret{}, fmt.Errorf("vault: secret not found: %s", path)
		}
		return Secret{}, fmt.Errorf("vault: get failed: %w", err)
	}

	var meta Metadata
	if len(metaJSON) > 0 {
		json.Unmarshal(metaJSON, &meta)
	}

	return Secret{
		Path:     path,
		Value:    value,
		Metadata: meta,
	}, nil
}

func (v *PostgresVault) Put(ctx context.Context, path string, value []byte, meta Metadata) error {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("vault: marshal metadata: %w", err)
	}

	_, err = v.pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (path, value, metadata)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (path) DO UPDATE SET value = $2, metadata = $3, updated_at = now()`, v.table),
		path, value, metaJSON,
	)
	if err != nil {
		return fmt.Errorf("vault: put failed: %w", err)
	}
	return nil
}

func (v *PostgresVault) Delete(ctx context.Context, path string) error {
	tag, err := v.pool.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE path = $1`, v.table), path)
	if err != nil {
		return fmt.Errorf("vault: delete failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("vault: secret not found: %s", path)
	}
	return nil
}
