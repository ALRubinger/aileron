package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// EnterpriseStore is a PostgreSQL implementation of store.EnterpriseStore.
type EnterpriseStore struct {
	db *DB
}

// NewEnterpriseStore returns a PostgreSQL-backed enterprise store.
func NewEnterpriseStore(db *DB) *EnterpriseStore {
	return &EnterpriseStore{db: db}
}

func (s *EnterpriseStore) Create(ctx context.Context, e model.Enterprise) error {
	providers, _ := json.Marshal(e.AllowedAuthProviders)
	domains, _ := json.Marshal(e.AllowedEmailDomains)
	_, err := s.db.Pool.Exec(ctx,
		`INSERT INTO enterprises
			(id, name, slug, plan, personal, billing_email, sso_required,
			 allowed_auth_providers, allowed_email_domains, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		e.ID, e.Name, e.Slug, string(e.Plan), e.Personal, e.BillingEmail, e.SSORequired,
		string(providers), string(domains), e.CreatedAt, e.UpdatedAt,
	)
	return err
}

func (s *EnterpriseStore) Get(ctx context.Context, id string) (model.Enterprise, error) {
	return s.scanOne(ctx,
		`SELECT id, name, slug, plan, personal, billing_email, sso_required,
			allowed_auth_providers, allowed_email_domains, created_at, updated_at
		 FROM enterprises WHERE id = $1`, id)
}

func (s *EnterpriseStore) GetBySlug(ctx context.Context, slug string) (model.Enterprise, error) {
	return s.scanOne(ctx,
		`SELECT id, name, slug, plan, personal, billing_email, sso_required,
			allowed_auth_providers, allowed_email_domains, created_at, updated_at
		 FROM enterprises WHERE slug = $1`, slug)
}

func (s *EnterpriseStore) Update(ctx context.Context, e model.Enterprise) error {
	providers, _ := json.Marshal(e.AllowedAuthProviders)
	domains, _ := json.Marshal(e.AllowedEmailDomains)
	tag, err := s.db.Pool.Exec(ctx,
		`UPDATE enterprises
		 SET name=$2, slug=$3, plan=$4, personal=$5, billing_email=$6, sso_required=$7,
			 allowed_auth_providers=$8, allowed_email_domains=$9, updated_at=$10
		 WHERE id=$1`,
		e.ID, e.Name, e.Slug, string(e.Plan), e.Personal, e.BillingEmail, e.SSORequired,
		string(providers), string(domains), e.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "enterprise", ID: e.ID}
	}
	return nil
}

func (s *EnterpriseStore) scanOne(ctx context.Context, query string, args ...any) (model.Enterprise, error) {
	var e model.Enterprise
	var plan string
	var providersJSON, domainsJSON string

	err := s.db.Pool.QueryRow(ctx, query, args...).Scan(
		&e.ID, &e.Name, &e.Slug, &plan, &e.Personal, &e.BillingEmail, &e.SSORequired,
		&providersJSON, &domainsJSON, &e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.Enterprise{}, &store.ErrNotFound{Entity: "enterprise", ID: fmt.Sprint(args...)}
		}
		return model.Enterprise{}, err
	}
	e.Plan = model.EnterprisePlan(plan)
	_ = json.Unmarshal([]byte(providersJSON), &e.AllowedAuthProviders)
	_ = json.Unmarshal([]byte(domainsJSON), &e.AllowedEmailDomains)
	return e, nil
}
