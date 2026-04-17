package postgres

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// UserInstructionStore is a PostgreSQL implementation of store.UserInstructionStore.
type UserInstructionStore struct {
	db *DB
}

// NewUserInstructionStore returns a PostgreSQL-backed user instruction store.
func NewUserInstructionStore(db *DB) *UserInstructionStore {
	return &UserInstructionStore{db: db}
}

const userInstructionColumns = `id, user_id, body, scope, active, created_at, updated_at`

func (s *UserInstructionStore) Create(ctx context.Context, ins model.UserInstruction) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO user_instructions
			(`+userInstructionColumns+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		ins.ID, ins.UserID, ins.Body, ins.Scope, ins.Active,
		ins.CreatedAt, ins.UpdatedAt,
	)
	return err
}

func (s *UserInstructionStore) Get(ctx context.Context, instructionID string) (model.UserInstruction, error) {
	return s.scanOne(ctx,
		`SELECT `+userInstructionColumns+` FROM user_instructions WHERE id = $1`, instructionID)
}

func (s *UserInstructionStore) List(ctx context.Context, filter store.UserInstructionFilter) ([]model.UserInstruction, error) {
	query := `SELECT ` + userInstructionColumns + ` FROM user_instructions WHERE 1=1`
	args := []any{}
	argIdx := 1

	if filter.UserID != "" {
		query += fmt.Sprintf(" AND user_id = $%d", argIdx)
		args = append(args, filter.UserID)
		argIdx++
	}
	if filter.Active != nil {
		query += fmt.Sprintf(" AND active = $%d", argIdx)
		args = append(args, *filter.Active)
		argIdx++
	}

	query += " ORDER BY created_at ASC"

	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instructions []model.UserInstruction
	for rows.Next() {
		ins, err := scanUserInstruction(rows)
		if err != nil {
			return nil, err
		}
		instructions = append(instructions, ins)
	}
	return instructions, rows.Err()
}

func (s *UserInstructionStore) Update(ctx context.Context, ins model.UserInstruction) error {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE user_instructions
		 SET body=$2, scope=$3, active=$4, updated_at=$5
		 WHERE id=$1`,
		ins.ID, ins.Body, ins.Scope, ins.Active, ins.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "user_instruction", ID: ins.ID}
	}
	return nil
}

func (s *UserInstructionStore) Delete(ctx context.Context, instructionID string) error {
	tag, err := s.db.Pool.Exec(ctx,
		`DELETE FROM user_instructions WHERE id = $1`, instructionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "user_instruction", ID: instructionID}
	}
	return nil
}

func (s *UserInstructionStore) scanOne(ctx context.Context, query string, args ...any) (model.UserInstruction, error) {
	row := s.db.Pool.QueryRow(ctx, query, args...)
	var ins model.UserInstruction
	err := row.Scan(
		&ins.ID, &ins.UserID, &ins.Body, &ins.Scope, &ins.Active,
		&ins.CreatedAt, &ins.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.UserInstruction{}, &store.ErrNotFound{Entity: "user_instruction", ID: fmt.Sprint(args...)}
		}
		return model.UserInstruction{}, err
	}
	return ins, nil
}

func scanUserInstruction(rows pgx.Rows) (model.UserInstruction, error) {
	var ins model.UserInstruction
	err := rows.Scan(
		&ins.ID, &ins.UserID, &ins.Body, &ins.Scope, &ins.Active,
		&ins.CreatedAt, &ins.UpdatedAt,
	)
	if err != nil {
		return model.UserInstruction{}, err
	}
	return ins, nil
}
