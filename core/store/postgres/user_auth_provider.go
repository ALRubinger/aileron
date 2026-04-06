package postgres

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// UserAuthProviderStore is a PostgreSQL implementation of store.UserAuthProviderStore.
type UserAuthProviderStore struct {
	db *DB
}

// NewUserAuthProviderStore returns a PostgreSQL-backed user auth provider store.
func NewUserAuthProviderStore(db *DB) *UserAuthProviderStore {
	return &UserAuthProviderStore{db: db}
}

const uapColumns = `id, user_id, provider, subject_id, created_at`

func (s *UserAuthProviderStore) Create(ctx context.Context, link model.UserAuthProvider) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO user_auth_providers (`+uapColumns+`)
		 VALUES ($1,$2,$3,$4,$5)`,
		link.ID, link.UserID, link.Provider, link.SubjectID, link.CreatedAt,
	)
	return err
}

func (s *UserAuthProviderStore) GetByProviderSubject(ctx context.Context, provider, subjectID string) (model.UserAuthProvider, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT `+uapColumns+` FROM user_auth_providers WHERE provider = $1 AND subject_id = $2`,
		provider, subjectID)
	var link model.UserAuthProvider
	err := row.Scan(&link.ID, &link.UserID, &link.Provider, &link.SubjectID, &link.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.UserAuthProvider{}, &store.ErrNotFound{Entity: "user_auth_provider", ID: provider + "/" + subjectID}
		}
		return model.UserAuthProvider{}, err
	}
	return link, nil
}

func (s *UserAuthProviderStore) ListByUser(ctx context.Context, userID string) ([]model.UserAuthProvider, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT `+uapColumns+` FROM user_auth_providers WHERE user_id = $1 ORDER BY created_at ASC`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []model.UserAuthProvider
	for rows.Next() {
		var link model.UserAuthProvider
		if err := rows.Scan(&link.ID, &link.UserID, &link.Provider, &link.SubjectID, &link.CreatedAt); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

func (s *UserAuthProviderStore) Delete(ctx context.Context, id string) error {
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM user_auth_providers WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "user_auth_provider", ID: id}
	}
	return nil
}

func (s *UserAuthProviderStore) DeleteByUserAndProvider(ctx context.Context, userID, provider string) error {
	tag, err := s.db.Pool.Exec(ctx,
		`DELETE FROM user_auth_providers WHERE user_id = $1 AND provider = $2`,
		userID, provider)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "user_auth_provider", ID: fmt.Sprintf("%s/%s", userID, provider)}
	}
	return nil
}
