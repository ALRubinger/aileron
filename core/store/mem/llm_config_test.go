package mem_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
)

func TestLLMConfigStore_Upsert_Create(t *testing.T) {
	s := mem.NewLLMConfigStore()
	now := time.Now()

	cfg := model.LLMConfig{
		ID:             "llm_1",
		OwnerType:      model.LLMConfigOwnerUser,
		OwnerID:        "usr_1",
		Provider:       model.LLMProviderAnthropic,
		ModelResearch:  "claude-haiku-4-5-20251001",
		ModelSynthesis: "claude-sonnet-4-6",
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	result, err := s.Upsert(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ID != "llm_1" {
		t.Errorf("expected ID llm_1, got %s", result.ID)
	}

	got, err := s.GetByOwner(context.Background(), model.LLMConfigOwnerUser, "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Provider != model.LLMProviderAnthropic {
		t.Errorf("expected anthropic, got %s", got.Provider)
	}
}

func TestLLMConfigStore_Upsert_Update(t *testing.T) {
	s := mem.NewLLMConfigStore()
	now := time.Now()

	cfg := model.LLMConfig{
		ID:             "llm_1",
		OwnerType:      model.LLMConfigOwnerUser,
		OwnerID:        "usr_1",
		Provider:       model.LLMProviderAnthropic,
		ModelResearch:  "claude-haiku-4-5-20251001",
		ModelSynthesis: "claude-sonnet-4-6",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	s.Upsert(context.Background(), cfg)

	// Upsert with new model — should keep original ID and CreatedAt.
	updated := model.LLMConfig{
		ID:             "llm_2_ignored",
		OwnerType:      model.LLMConfigOwnerUser,
		OwnerID:        "usr_1",
		Provider:       model.LLMProviderAnthropic,
		ModelResearch:  "claude-haiku-4-5-20251001",
		ModelSynthesis: "claude-opus-4-6",
		CreatedAt:      now.Add(time.Hour),
		UpdatedAt:      now.Add(time.Hour),
	}
	result, err := s.Upsert(context.Background(), updated)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ID != "llm_1" {
		t.Errorf("expected original ID llm_1, got %s", result.ID)
	}
	if !result.CreatedAt.Equal(now) {
		t.Errorf("expected original CreatedAt preserved")
	}
	if result.ModelSynthesis != "claude-opus-4-6" {
		t.Errorf("expected updated model, got %s", result.ModelSynthesis)
	}
}

func TestLLMConfigStore_GetByOwner_NotFound(t *testing.T) {
	s := mem.NewLLMConfigStore()
	_, err := s.GetByOwner(context.Background(), model.LLMConfigOwnerUser, "usr_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	var notFound *store.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}

func TestLLMConfigStore_Delete(t *testing.T) {
	s := mem.NewLLMConfigStore()
	now := time.Now()
	cfg := model.LLMConfig{
		ID:        "llm_1",
		OwnerType: model.LLMConfigOwnerUser,
		OwnerID:   "usr_1",
		Provider:  model.LLMProviderAnthropic,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.Upsert(context.Background(), cfg)

	if err := s.Delete(context.Background(), "llm_1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err := s.GetByOwner(context.Background(), model.LLMConfigOwnerUser, "usr_1")
	if err == nil {
		t.Fatal("expected ErrNotFound after delete")
	}
}

func TestLLMConfigStore_Delete_NotFound(t *testing.T) {
	s := mem.NewLLMConfigStore()
	err := s.Delete(context.Background(), "llm_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	var notFound *store.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}

func TestLLMConfigStore_SeparateOwners(t *testing.T) {
	s := mem.NewLLMConfigStore()
	now := time.Now()

	// User config.
	s.Upsert(context.Background(), model.LLMConfig{
		ID:             "llm_user",
		OwnerType:      model.LLMConfigOwnerUser,
		OwnerID:        "usr_1",
		Provider:       model.LLMProviderAnthropic,
		ModelSynthesis: "user-model",
		CreatedAt:      now,
		UpdatedAt:      now,
	})

	// Enterprise config for different owner.
	s.Upsert(context.Background(), model.LLMConfig{
		ID:             "llm_ent",
		OwnerType:      model.LLMConfigOwnerEnterprise,
		OwnerID:        "ent_1",
		Provider:       model.LLMProviderAnthropic,
		ModelSynthesis: "enterprise-model",
		CreatedAt:      now,
		UpdatedAt:      now,
	})

	userCfg, err := s.GetByOwner(context.Background(), model.LLMConfigOwnerUser, "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if userCfg.ModelSynthesis != "user-model" {
		t.Errorf("expected user-model, got %s", userCfg.ModelSynthesis)
	}

	entCfg, err := s.GetByOwner(context.Background(), model.LLMConfigOwnerEnterprise, "ent_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entCfg.ModelSynthesis != "enterprise-model" {
		t.Errorf("expected enterprise-model, got %s", entCfg.ModelSynthesis)
	}
}
