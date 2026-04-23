//go:build integration

package storetest

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
)

// TestLLMConfigStore exercises the store.LLMConfigStore contract.
func TestLLMConfigStore(t *testing.T, h *Harness) {
	ctx := context.Background()

	t.Run("UpsertNewAndGetByOwner", func(t *testing.T) {
		ownerID := uid("usr")
		n := now()
		cfg := model.LLMConfig{
			ID:             uid("llm"),
			OwnerType:      model.LLMConfigOwnerUser,
			OwnerID:        ownerID,
			Provider:       model.LLMProviderAnthropic,
			ModelResearch:  "claude-sonnet-4-5-20250514",
			ModelSynthesis: "claude-sonnet-4-5-20250514",
			CreatedAt:      n,
			UpdatedAt:      n,
		}
		got, err := h.LLMConfigs.Upsert(ctx, cfg)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if got.Provider != model.LLMProviderAnthropic {
			t.Errorf("Provider = %q, want %q", got.Provider, model.LLMProviderAnthropic)
		}

		fetched, err := h.LLMConfigs.GetByOwner(ctx, model.LLMConfigOwnerUser, ownerID)
		if err != nil {
			t.Fatalf("GetByOwner: %v", err)
		}
		if fetched.ModelResearch != "claude-sonnet-4-5-20250514" {
			t.Errorf("ModelResearch = %q", fetched.ModelResearch)
		}
	})

	t.Run("UpsertOverwrite", func(t *testing.T) {
		ownerID := uid("ent")
		n := now()
		cfg := model.LLMConfig{
			ID:        uid("llm"),
			OwnerType: model.LLMConfigOwnerEnterprise,
			OwnerID:   ownerID,
			Provider:  model.LLMProviderOpenAI,
			CreatedAt: n, UpdatedAt: n,
		}
		first, err := h.LLMConfigs.Upsert(ctx, cfg)
		if err != nil {
			t.Fatalf("first Upsert: %v", err)
		}

		cfg.ID = uid("llm") // new ID should be ignored; existing ID kept
		cfg.Provider = model.LLMProviderAnthropic
		cfg.ModelResearch = "opus"
		cfg.UpdatedAt = now()
		second, err := h.LLMConfigs.Upsert(ctx, cfg)
		if err != nil {
			t.Fatalf("second Upsert: %v", err)
		}
		if second.ID != first.ID {
			t.Errorf("ID changed from %q to %q on upsert", first.ID, second.ID)
		}

		fetched, err := h.LLMConfigs.GetByOwner(ctx, model.LLMConfigOwnerEnterprise, ownerID)
		if err != nil {
			t.Fatalf("GetByOwner: %v", err)
		}
		if fetched.Provider != model.LLMProviderAnthropic {
			t.Errorf("Provider = %q, want %q", fetched.Provider, model.LLMProviderAnthropic)
		}
		if fetched.ModelResearch != "opus" {
			t.Errorf("ModelResearch = %q, want %q", fetched.ModelResearch, "opus")
		}
	})

	t.Run("GetByOwnerNotFound", func(t *testing.T) {
		_, err := h.LLMConfigs.GetByOwner(ctx, model.LLMConfigOwnerUser, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("Delete", func(t *testing.T) {
		ownerID := uid("usr")
		n := now()
		cfg := model.LLMConfig{
			ID:        uid("llm"),
			OwnerType: model.LLMConfigOwnerUser,
			OwnerID:   ownerID,
			Provider:  model.LLMProviderAnthropic,
			CreatedAt: n, UpdatedAt: n,
		}
		got, err := h.LLMConfigs.Upsert(ctx, cfg)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		if err := h.LLMConfigs.Delete(ctx, got.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		_, err = h.LLMConfigs.GetByOwner(ctx, model.LLMConfigOwnerUser, ownerID)
		assertNotFound(t, err)
	})

	t.Run("DeleteNotFound", func(t *testing.T) {
		err := h.LLMConfigs.Delete(ctx, "nonexistent")
		assertNotFound(t, err)
	})
}
