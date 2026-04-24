package postgres

import (
	"context"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/jackc/pgx/v5"
)

// UserKeyMaterialStore is a PostgreSQL implementation of store.UserKeyMaterialStore.
type UserKeyMaterialStore struct {
	db *DB
}

// NewUserKeyMaterialStore returns a PostgreSQL-backed user key material store.
func NewUserKeyMaterialStore(db *DB) *UserKeyMaterialStore {
	return &UserKeyMaterialStore{db: db}
}

func (s *UserKeyMaterialStore) Create(ctx context.Context, m model.UserKeyMaterial) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO user_key_materials
			(user_id, salt, kek_verification, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5)`,
		m.UserID, m.Salt, m.KEKVerification, m.CreatedAt, m.UpdatedAt,
	)
	return err
}

func (s *UserKeyMaterialStore) Get(ctx context.Context, userID string) (model.UserKeyMaterial, error) {
	var m model.UserKeyMaterial
	err := s.db.Pool.QueryRow(ctx,
		`SELECT user_id, salt, kek_verification, created_at, updated_at
		 FROM user_key_materials WHERE user_id = $1`, userID,
	).Scan(&m.UserID, &m.Salt, &m.KEKVerification, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.UserKeyMaterial{}, &store.ErrNotFound{Entity: "user_key_material", ID: userID}
		}
		return model.UserKeyMaterial{}, err
	}
	return m, nil
}

func (s *UserKeyMaterialStore) Update(ctx context.Context, m model.UserKeyMaterial) error {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE user_key_materials
		 SET salt = $2, kek_verification = $3, updated_at = $4
		 WHERE user_id = $1`,
		m.UserID, m.Salt, m.KEKVerification, m.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "user_key_material", ID: m.UserID}
	}
	return nil
}
