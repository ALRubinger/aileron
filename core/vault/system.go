package vault

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresSystemVault is a PostgreSQL-backed vault for infrastructure secrets
// (ADR-0020). It stores secrets in the system_vault_secrets table, separate
// from user secrets in vault_secrets.
//
// Infrastructure secrets (Slack bot tokens, webhook signing keys) are stored
// here because they have no associated user and must be readable by the server
// autonomously. Wrap with EncryptedVault for at-rest encryption using a
// server-managed key (AILERON_SYSTEM_VAULT_KEY).
type PostgresSystemVault struct {
	pool *pgxpool.Pool
}

// NewPostgresSystemVault creates a PostgreSQL-backed system vault.
func NewPostgresSystemVault(pool *pgxpool.Pool) *PostgresSystemVault {
	return &PostgresSystemVault{pool: pool}
}

func (v *PostgresSystemVault) Get(ctx context.Context, path string) (Secret, error) {
	var value []byte
	var metaJSON []byte

	err := v.pool.QueryRow(ctx,
		`SELECT value, metadata FROM system_vault_secrets WHERE path = $1`, path,
	).Scan(&value, &metaJSON)

	if err != nil {
		if err == pgx.ErrNoRows {
			return Secret{}, &errNotFound{path: path}
		}
		return Secret{}, fmt.Errorf("system vault: get failed: %w", err)
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

func (v *PostgresSystemVault) Put(ctx context.Context, path string, value []byte, meta Metadata) error {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("system vault: marshal metadata: %w", err)
	}

	_, err = v.pool.Exec(ctx,
		`INSERT INTO system_vault_secrets (path, value, metadata)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (path) DO UPDATE SET value = $2, metadata = $3, updated_at = now()`,
		path, value, metaJSON,
	)
	if err != nil {
		return fmt.Errorf("system vault: put failed: %w", err)
	}
	return nil
}

func (v *PostgresSystemVault) Delete(ctx context.Context, path string) error {
	tag, err := v.pool.Exec(ctx,
		`DELETE FROM system_vault_secrets WHERE path = $1`, path)
	if err != nil {
		return fmt.Errorf("system vault: delete failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &errNotFound{path: path}
	}
	return nil
}
