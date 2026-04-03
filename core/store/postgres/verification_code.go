package postgres

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// VerificationCodeStore is a PostgreSQL implementation of store.VerificationCodeStore.
type VerificationCodeStore struct {
	db *DB
}

// NewVerificationCodeStore returns a PostgreSQL-backed verification code store.
func NewVerificationCodeStore(db *DB) *VerificationCodeStore {
	return &VerificationCodeStore{db: db}
}

func (s *VerificationCodeStore) Create(ctx context.Context, code model.VerificationCode) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO verification_codes (id, user_id, code_hash, expires_at, used, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		code.ID, code.UserID, code.CodeHash, code.ExpiresAt, code.Used, code.CreatedAt,
	)
	return err
}

func (s *VerificationCodeStore) GetActiveByUserID(ctx context.Context, userID string) (model.VerificationCode, error) {
	var code model.VerificationCode
	err := s.db.Pool.QueryRow(ctx,
		`SELECT id, user_id, code_hash, expires_at, used, created_at
		 FROM verification_codes
		 WHERE user_id = $1 AND used = false AND expires_at > now()
		 ORDER BY created_at DESC LIMIT 1`, userID,
	).Scan(&code.ID, &code.UserID, &code.CodeHash, &code.ExpiresAt, &code.Used, &code.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.VerificationCode{}, &store.ErrNotFound{Entity: "verification_code", ID: fmt.Sprintf("user=%s", userID)}
		}
		return model.VerificationCode{}, err
	}
	return code, nil
}

func (s *VerificationCodeStore) MarkUsed(ctx context.Context, codeID string) error {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE verification_codes SET used = true WHERE id = $1`, codeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "verification_code", ID: codeID}
	}
	return nil
}

func (s *VerificationCodeStore) DeleteExpiredForUser(ctx context.Context, userID string) error {
	_, err := s.db.Pool.Exec(ctx,
		`DELETE FROM verification_codes WHERE user_id = $1 AND (used = true OR expires_at <= now())`,
		userID)
	return err
}
