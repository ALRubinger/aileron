package postgres

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/jackc/pgx/v5"
)

// LLMConfigStore is a PostgreSQL implementation of store.LLMConfigStore.
type LLMConfigStore struct {
	db *DB
}

// NewLLMConfigStore returns a PostgreSQL-backed LLM config store.
func NewLLMConfigStore(db *DB) *LLMConfigStore {
	return &LLMConfigStore{db: db}
}

const llmConfigColumns = `id, owner_type, owner_id, provider, model_research, model_synthesis, created_at, updated_at`

func (s *LLMConfigStore) Upsert(ctx context.Context, cfg model.LLMConfig) (model.LLMConfig, error) {
	err := s.db.Pool.QueryRow(ctx,
		`INSERT INTO llm_configs (`+llmConfigColumns+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (owner_type, owner_id) DO UPDATE SET
			provider = $4, model_research = $5, model_synthesis = $6, updated_at = $8
		 RETURNING id, created_at`,
		cfg.ID, string(cfg.OwnerType), cfg.OwnerID, string(cfg.Provider),
		cfg.ModelResearch, cfg.ModelSynthesis,
		cfg.CreatedAt, cfg.UpdatedAt,
	).Scan(&cfg.ID, &cfg.CreatedAt)
	if err != nil {
		return model.LLMConfig{}, err
	}
	return cfg, nil
}

func (s *LLMConfigStore) GetByOwner(ctx context.Context, ownerType model.LLMConfigOwnerType, ownerID string) (model.LLMConfig, error) {
	row := s.db.Pool.QueryRow(ctx,
		`SELECT `+llmConfigColumns+` FROM llm_configs WHERE owner_type = $1 AND owner_id = $2`,
		string(ownerType), ownerID,
	)

	var cfg model.LLMConfig
	var ownerTypeStr, providerStr string
	err := row.Scan(
		&cfg.ID, &ownerTypeStr, &cfg.OwnerID, &providerStr,
		&cfg.ModelResearch, &cfg.ModelSynthesis,
		&cfg.CreatedAt, &cfg.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.LLMConfig{}, &store.ErrNotFound{
				Entity: "llm_config",
				ID:     fmt.Sprintf("%s/%s", ownerType, ownerID),
			}
		}
		return model.LLMConfig{}, err
	}
	cfg.OwnerType = model.LLMConfigOwnerType(ownerTypeStr)
	cfg.Provider = model.LLMProvider(providerStr)
	return cfg, nil
}

func (s *LLMConfigStore) Delete(ctx context.Context, configID string) error {
	tag, err := s.db.Pool.Exec(ctx,
		`DELETE FROM llm_configs WHERE id = $1`, configID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &store.ErrNotFound{Entity: "llm_config", ID: configID}
	}
	return nil
}
