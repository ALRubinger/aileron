package postgres

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// SSOConfigStore is a PostgreSQL implementation of store.SSOConfigStore.
type SSOConfigStore struct {
	db *DB
}

// NewSSOConfigStore returns a PostgreSQL-backed SSO config store.
func NewSSOConfigStore(db *DB) *SSOConfigStore {
	return &SSOConfigStore{db: db}
}

func (s *SSOConfigStore) Create(ctx context.Context, cfg model.SSOConfig) error {
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO sso_configs
			(id, enterprise_id, provider, client_id, issuer_url, enabled, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		cfg.ID, cfg.EnterpriseID, string(cfg.Provider), cfg.ClientID,
		cfg.IssuerURL, cfg.Enabled, cfg.CreatedAt, cfg.UpdatedAt,
	)
	return err
}

func (s *SSOConfigStore) Get(ctx context.Context, id string) (model.SSOConfig, error) {
	return s.scanOne(ctx,
		`SELECT id, enterprise_id, provider, client_id, issuer_url, enabled, created_at, updated_at
		 FROM sso_configs WHERE id = $1`, id)
}

func (s *SSOConfigStore) GetByEnterprise(ctx context.Context, enterpriseID string) ([]model.SSOConfig, error) {
	rows, err := s.db.Pool.Query(ctx,
		`SELECT id, enterprise_id, provider, client_id, issuer_url, enabled, created_at, updated_at
		 FROM sso_configs WHERE enterprise_id = $1 ORDER BY created_at ASC`, enterpriseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []model.SSOConfig
	for rows.Next() {
		cfg, err := scanSSOConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, cfg)
	}
	return configs, rows.Err()
}

func (s *SSOConfigStore) Update(ctx context.Context, cfg model.SSOConfig) error {
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE sso_configs
		 SET enterprise_id=$2, provider=$3, client_id=$4, issuer_url=$5,
			 enabled=$6, updated_at=$7
		 WHERE id=$1`,
		cfg.ID, cfg.EnterpriseID, string(cfg.Provider), cfg.ClientID,
		cfg.IssuerURL, cfg.Enabled, cfg.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "sso_config", ID: cfg.ID}
	}
	return nil
}

func (s *SSOConfigStore) Delete(ctx context.Context, id string) error {
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM sso_configs WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "sso_config", ID: id}
	}
	return nil
}

func (s *SSOConfigStore) scanOne(ctx context.Context, query string, args ...any) (model.SSOConfig, error) {
	var cfg model.SSOConfig
	var provider string
	err := s.db.Pool.QueryRow(ctx, query, args...).Scan(
		&cfg.ID, &cfg.EnterpriseID, &provider, &cfg.ClientID,
		&cfg.IssuerURL, &cfg.Enabled, &cfg.CreatedAt, &cfg.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.SSOConfig{}, &store.ErrNotFound{Entity: "sso_config", ID: fmt.Sprint(args...)}
		}
		return model.SSOConfig{}, err
	}
	cfg.Provider = model.SSOProvider(provider)
	return cfg, nil
}

func scanSSOConfig(rows pgx.Rows) (model.SSOConfig, error) {
	var cfg model.SSOConfig
	var provider string
	err := rows.Scan(
		&cfg.ID, &cfg.EnterpriseID, &provider, &cfg.ClientID,
		&cfg.IssuerURL, &cfg.Enabled, &cfg.CreatedAt, &cfg.UpdatedAt,
	)
	if err != nil {
		return model.SSOConfig{}, err
	}
	cfg.Provider = model.SSOProvider(provider)
	return cfg, nil
}
