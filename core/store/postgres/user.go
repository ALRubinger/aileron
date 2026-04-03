package postgres

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// UserStore is a PostgreSQL implementation of store.UserStore.
type UserStore struct {
	db *DB
}

// NewUserStore returns a PostgreSQL-backed user store.
func NewUserStore(db *DB) *UserStore {
	return &UserStore{db: db}
}

const userColumns = `id, enterprise_id, email, display_name, avatar_url, role, status,
	auth_provider, auth_provider_subject_id, password_hash, last_login_at, created_at, updated_at`

func (s *UserStore) Create(ctx context.Context, u model.User) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO users
			(`+userColumns+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		u.ID, u.EnterpriseID, u.Email, u.DisplayName, u.AvatarURL,
		string(u.Role), string(u.Status), u.AuthProvider, u.AuthProviderSubjectID,
		u.PasswordHash,
		u.LastLoginAt, u.CreatedAt, u.UpdatedAt,
	)
	return err
}

func (s *UserStore) Get(ctx context.Context, id string) (model.User, error) {
	return s.scanOne(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id)
}

func (s *UserStore) GetByEmail(ctx context.Context, email string) (model.User, error) {
	return s.scanOne(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = $1`, email)
}

func (s *UserStore) GetByProviderSubject(ctx context.Context, provider, subjectID string) (model.User, error) {
	return s.scanOne(ctx,
		`SELECT `+userColumns+` FROM users WHERE auth_provider = $1 AND auth_provider_subject_id = $2`,
		provider, subjectID)
}

func (s *UserStore) List(ctx context.Context, filter store.UserFilter) ([]model.User, error) {
	query := `SELECT ` + userColumns + ` FROM users WHERE 1=1`
	args := []any{}
	argIdx := 1

	if filter.EnterpriseID != "" {
		query += fmt.Sprintf(" AND enterprise_id = $%d", argIdx)
		args = append(args, filter.EnterpriseID)
		argIdx++
	}
	if filter.Status != nil {
		query += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, string(*filter.Status))
		argIdx++
	}

	query += " ORDER BY created_at ASC"

	if filter.PageSize > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argIdx)
		args = append(args, filter.PageSize)
	}

	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []model.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *UserStore) Update(ctx context.Context, u model.User) error {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE users
		 SET enterprise_id=$2, email=$3, display_name=$4, avatar_url=$5, role=$6,
			 status=$7, auth_provider=$8, auth_provider_subject_id=$9,
			 password_hash=$10, last_login_at=$11, updated_at=$12
		 WHERE id=$1`,
		u.ID, u.EnterpriseID, u.Email, u.DisplayName, u.AvatarURL,
		string(u.Role), string(u.Status), u.AuthProvider, u.AuthProviderSubjectID,
		u.PasswordHash, u.LastLoginAt, u.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "user", ID: u.ID}
	}
	return nil
}

func (s *UserStore) scanOne(ctx context.Context, query string, args ...any) (model.User, error) {
	row := s.db.Pool.QueryRow(ctx, query, args...)
	var u model.User
	var role, status string
	err := row.Scan(
		&u.ID, &u.EnterpriseID, &u.Email, &u.DisplayName, &u.AvatarURL,
		&role, &status, &u.AuthProvider, &u.AuthProviderSubjectID,
		&u.PasswordHash, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.User{}, &store.ErrNotFound{Entity: "user", ID: fmt.Sprint(args...)}
		}
		return model.User{}, err
	}
	u.Role = model.UserRole(role)
	u.Status = model.UserStatus(status)
	return u, nil
}

// scanUser scans a user from a pgx.Rows iterator.
func scanUser(rows pgx.Rows) (model.User, error) {
	var u model.User
	var role, status string
	err := rows.Scan(
		&u.ID, &u.EnterpriseID, &u.Email, &u.DisplayName, &u.AvatarURL,
		&role, &status, &u.AuthProvider, &u.AuthProviderSubjectID,
		&u.PasswordHash, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return model.User{}, err
	}
	u.Role = model.UserRole(role)
	u.Status = model.UserStatus(status)
	return u, nil
}
