package postgres

import (
	"context"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// SessionStore is a PostgreSQL implementation of store.SessionStore.
type SessionStore struct {
	db *DB
}

// NewSessionStore returns a PostgreSQL-backed session store.
func NewSessionStore(db *DB) *SessionStore {
	return &SessionStore{db: db}
}

func (s *SessionStore) Create(ctx context.Context, sess model.Session) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO sessions
			(id, user_id, token_hash, refresh_token_hash, expires_at, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		sess.ID, sess.UserID, sess.TokenHash, sess.RefreshTokenHash,
		sess.ExpiresAt, sess.CreatedAt,
	)
	return err
}

func (s *SessionStore) GetByTokenHash(ctx context.Context, tokenHash string) (model.Session, error) {
	return s.scanOne(ctx,
		`SELECT id, user_id, token_hash, refresh_token_hash, expires_at, created_at
		 FROM sessions WHERE token_hash = $1`, tokenHash)
}

func (s *SessionStore) GetByRefreshTokenHash(ctx context.Context, refreshHash string) (model.Session, error) {
	return s.scanOne(ctx,
		`SELECT id, user_id, token_hash, refresh_token_hash, expires_at, created_at
		 FROM sessions WHERE refresh_token_hash = $1`, refreshHash)
}

func (s *SessionStore) Delete(ctx context.Context, sessionID string) error {
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "session", ID: sessionID}
	}
	return nil
}

func (s *SessionStore) DeleteAllForUser(ctx context.Context, userID string) error {
	_, err := s.db.Pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
	return err
}

func (s *SessionStore) scanOne(ctx context.Context, query string, args ...any) (model.Session, error) {
	var sess model.Session
	err := s.db.Pool.QueryRow(ctx, query, args...).Scan(
		&sess.ID, &sess.UserID, &sess.TokenHash, &sess.RefreshTokenHash,
		&sess.ExpiresAt, &sess.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.Session{}, &store.ErrNotFound{Entity: "session", ID: ""}
		}
		return model.Session{}, err
	}
	return sess, nil
}
