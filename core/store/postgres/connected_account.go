package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// ConnectedAccountStore is a PostgreSQL implementation of store.ConnectedAccountStore.
type ConnectedAccountStore struct {
	db *DB
}

// NewConnectedAccountStore returns a PostgreSQL-backed connected account store.
func NewConnectedAccountStore(db *DB) *ConnectedAccountStore {
	return &ConnectedAccountStore{db: db}
}

const connectedAccountColumns = `id, user_id, provider, scopes, status,
	external_user_id, external_team_id, created_at, updated_at`

func (s *ConnectedAccountStore) Create(ctx context.Context, a model.ConnectedAccount) error {
	scopes, _ := json.Marshal(a.Scopes)
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO connected_accounts
			(`+connectedAccountColumns+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		a.ID, a.UserID, string(a.Provider), string(scopes),
		string(a.Status), a.ExternalUserID, a.ExternalTeamID,
		a.CreatedAt, a.UpdatedAt,
	)
	return err
}

func (s *ConnectedAccountStore) Upsert(ctx context.Context, a model.ConnectedAccount) (model.ConnectedAccount, error) {
	scopes, _ := json.Marshal(a.Scopes)
	var id string
	var createdAt time.Time
	err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO connected_accounts
			(`+connectedAccountColumns+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (user_id, provider) DO UPDATE SET
			scopes = $4, status = $5, external_user_id = $6,
			external_team_id = $7, updated_at = $9
		 RETURNING id, created_at`,
		a.ID, a.UserID, string(a.Provider), string(scopes),
		string(a.Status), a.ExternalUserID, a.ExternalTeamID,
		a.CreatedAt, a.UpdatedAt,
	).Scan(&id, &createdAt)
	if err != nil {
		return model.ConnectedAccount{}, err
	}
	a.ID = id
	a.CreatedAt = createdAt
	return a, nil
}

func (s *ConnectedAccountStore) Get(ctx context.Context, accountID string) (model.ConnectedAccount, error) {
	return s.scanOne(ctx,
		`SELECT `+connectedAccountColumns+` FROM connected_accounts WHERE id = $1`, accountID)
}

func (s *ConnectedAccountStore) List(ctx context.Context, filter store.ConnectedAccountFilter) ([]model.ConnectedAccount, error) {
	query := `SELECT ` + connectedAccountColumns + ` FROM connected_accounts WHERE 1=1`
	args := []any{}
	argIdx := 1

	if filter.UserID != "" {
		query += fmt.Sprintf(" AND user_id = $%d", argIdx)
		args = append(args, filter.UserID)
		argIdx++
	}
	if filter.Provider != nil {
		query += fmt.Sprintf(" AND provider = $%d", argIdx)
		args = append(args, string(*filter.Provider))
		argIdx++
	}
	if filter.Status != nil {
		query += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, string(*filter.Status))
		argIdx++
	}
	if filter.ExternalUserID != "" {
		query += fmt.Sprintf(" AND external_user_id = $%d", argIdx)
		args = append(args, filter.ExternalUserID)
		argIdx++
	}
	if filter.ExternalTeamID != "" {
		query += fmt.Sprintf(" AND external_team_id = $%d", argIdx)
		args = append(args, filter.ExternalTeamID)
		argIdx++
	}

	query += " ORDER BY created_at ASC"

	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []model.ConnectedAccount
	for rows.Next() {
		a, err := scanConnectedAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (s *ConnectedAccountStore) Delete(ctx context.Context, accountID string) error {
	tag, err := s.db.Pool.Exec(ctx,
		`DELETE FROM connected_accounts WHERE id = $1`, accountID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "connected_account", ID: accountID}
	}
	return nil
}

func (s *ConnectedAccountStore) scanOne(ctx context.Context, query string, args ...any) (model.ConnectedAccount, error) {
	row := s.db.Pool.QueryRow(ctx, query, args...)
	var a model.ConnectedAccount
	var provider, status, scopesJSON string
	err := row.Scan(
		&a.ID, &a.UserID, &provider, &scopesJSON,
		&status, &a.ExternalUserID, &a.ExternalTeamID,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.ConnectedAccount{}, &store.ErrNotFound{Entity: "connected_account", ID: fmt.Sprint(args...)}
		}
		return model.ConnectedAccount{}, err
	}
	a.Provider = model.ConnectedAccountProvider(provider)
	a.Status = model.ConnectedAccountStatus(status)
	_ = json.Unmarshal([]byte(scopesJSON), &a.Scopes)
	return a, nil
}

func scanConnectedAccount(rows pgx.Rows) (model.ConnectedAccount, error) {
	var a model.ConnectedAccount
	var provider, status, scopesJSON string
	err := rows.Scan(
		&a.ID, &a.UserID, &provider, &scopesJSON,
		&status, &a.ExternalUserID, &a.ExternalTeamID,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return model.ConnectedAccount{}, err
	}
	a.Provider = model.ConnectedAccountProvider(provider)
	a.Status = model.ConnectedAccountStatus(status)
	_ = json.Unmarshal([]byte(scopesJSON), &a.Scopes)
	return a, nil
}
