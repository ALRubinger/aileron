package vault

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresVault is a PostgreSQL-backed implementation of the Vault interface.
// Stores secrets as byte values with JSON metadata. This is Phase 1 of the
// vault persistence model (ADR-0010) — plaintext storage in Postgres.
// Phase 2 layers EncryptedVault on top for at-rest encryption.
type PostgresVault struct {
	pool *pgxpool.Pool
}

// NewPostgresVault creates a Postgres-backed vault using the given connection pool.
func NewPostgresVault(pool *pgxpool.Pool) *PostgresVault {
	return &PostgresVault{pool: pool}
}

func (v *PostgresVault) Get(ctx context.Context, path string) (Secret, error) {
	var value []byte
	var metaJSON []byte

	err := v.pool.QueryRow(ctx,
		`SELECT value, metadata FROM vault_secrets WHERE path = $1`, path,
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
		`INSERT INTO vault_secrets (path, value, metadata)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (path) DO UPDATE SET value = $2, metadata = $3, updated_at = now()`,
		path, value, metaJSON,
	)
	if err != nil {
		return fmt.Errorf("vault: put failed: %w", err)
	}
	return nil
}

func (v *PostgresVault) Delete(ctx context.Context, path string) error {
	tag, err := v.pool.Exec(ctx,
		`DELETE FROM vault_secrets WHERE path = $1`, path)
	if err != nil {
		return fmt.Errorf("vault: delete failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("vault: secret not found: %s", path)
	}
	return nil
}
