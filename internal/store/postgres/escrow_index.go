package postgres

import (
	"context"
	"time"
)

// EscrowIndexStore persists the server-side escrow index in PostgreSQL so it
// survives server restarts. Maps vault paths to enclave escrow IDs.
type EscrowIndexStore struct {
	db *DB
}

// NewEscrowIndexStore returns a PostgreSQL-backed escrow index store.
func NewEscrowIndexStore(db *DB) *EscrowIndexStore {
	return &EscrowIndexStore{db: db}
}

// Upsert creates or updates an escrow index entry.
func (s *EscrowIndexStore) Upsert(ctx context.Context, vaultPath, escrowID, userID string, expiresAt time.Time) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO escrow_index (vault_path, escrow_id, user_id, expires_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (vault_path) DO UPDATE SET
			escrow_id = $2, user_id = $3, expires_at = $4`,
		vaultPath, escrowID, userID, expiresAt,
	)
	return err
}

// Delete removes an escrow index entry by vault path.
func (s *EscrowIndexStore) Delete(ctx context.Context, vaultPath string) error {
	_, err := s.db.Pool.Exec(ctx,
		`DELETE FROM escrow_index WHERE vault_path = $1`,
		vaultPath,
	)
	return err
}

// LoadAll returns all non-expired escrow index entries as a map of
// vault_path → escrow_id.
func (s *EscrowIndexStore) LoadAll(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT vault_path, escrow_id FROM escrow_index WHERE expires_at > now()`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var path, id string
		if err := rows.Scan(&path, &id); err != nil {
			return nil, err
		}
		result[path] = id
	}
	return result, rows.Err()
}

// DeleteExpired removes all expired entries and returns the count removed.
func (s *EscrowIndexStore) DeleteExpired(ctx context.Context) (int, error) {
	tag, err := s.db.Pool.Exec(ctx,
		`DELETE FROM escrow_index WHERE expires_at <= now()`,
	)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
