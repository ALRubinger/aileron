package storetest

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
)

// TestSSOConfigStore exercises the store.SSOConfigStore contract.
func TestSSOConfigStore(t *testing.T, h *Harness) {
	ctx := context.Background()
	ent := h.SeedEnterprise(t, ctx)

	t.Run("CreateAndGet", func(t *testing.T) {
		id := uid("sso")
		n := now()
		cfg := model.SSOConfig{
			ID:           id,
			EnterpriseID: ent.ID,
			Provider:     model.SSOProviderGoogle,
			ClientID:     "client-id-123",
			IssuerURL:    "https://accounts.google.com",
			Enabled:      true,
			CreatedAt:    n,
			UpdatedAt:    n,
		}
		if err := h.SSOConfigs.Create(ctx, cfg); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.SSOConfigs.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Provider != model.SSOProviderGoogle {
			t.Errorf("Provider = %q, want %q", got.Provider, model.SSOProviderGoogle)
		}
		if got.ClientID != "client-id-123" {
			t.Errorf("ClientID = %q, want %q", got.ClientID, "client-id-123")
		}
		if got.IssuerURL != "https://accounts.google.com" {
			t.Errorf("IssuerURL = %q, want %q", got.IssuerURL, "https://accounts.google.com")
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		_, err := h.SSOConfigs.Get(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("GetByEnterprise", func(t *testing.T) {
		ent2 := h.SeedEnterprise(t, ctx)
		n := now()
		for _, p := range []model.SSOProvider{model.SSOProviderGoogle, model.SSOProviderOkta} {
			cfg := model.SSOConfig{
				ID: uid("sso"), EnterpriseID: ent2.ID,
				Provider: p, ClientID: "cid",
				CreatedAt: n, UpdatedAt: n,
			}
			if err := h.SSOConfigs.Create(ctx, cfg); err != nil {
				t.Fatalf("Create %s: %v", p, err)
			}
		}

		configs, err := h.SSOConfigs.GetByEnterprise(ctx, ent2.ID)
		if err != nil {
			t.Fatalf("GetByEnterprise: %v", err)
		}
		if len(configs) != 2 {
			t.Errorf("got %d configs, want 2", len(configs))
		}
	})

	t.Run("Update", func(t *testing.T) {
		ent3 := h.SeedEnterprise(t, ctx)
		id := uid("sso")
		n := now()
		cfg := model.SSOConfig{
			ID: id, EnterpriseID: ent3.ID,
			Provider: model.SSOProviderOkta, ClientID: "old-cid",
			Enabled: true, CreatedAt: n, UpdatedAt: n,
		}
		if err := h.SSOConfigs.Create(ctx, cfg); err != nil {
			t.Fatalf("Create: %v", err)
		}

		cfg.ClientID = "new-cid"
		cfg.Enabled = false
		cfg.UpdatedAt = now()
		if err := h.SSOConfigs.Update(ctx, cfg); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := h.SSOConfigs.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ClientID != "new-cid" {
			t.Errorf("ClientID = %q, want %q", got.ClientID, "new-cid")
		}
		if got.Enabled {
			t.Error("Enabled = true, want false")
		}
	})

	t.Run("UpdateNotFound", func(t *testing.T) {
		cfg := model.SSOConfig{ID: "nonexistent", EnterpriseID: ent.ID, UpdatedAt: now()}
		err := h.SSOConfigs.Update(ctx, cfg)
		assertNotFound(t, err)
	})

	t.Run("Delete", func(t *testing.T) {
		ent4 := h.SeedEnterprise(t, ctx)
		id := uid("sso")
		n := now()
		cfg := model.SSOConfig{
			ID: id, EnterpriseID: ent4.ID,
			Provider: model.SSOProviderGoogle, ClientID: "cid",
			CreatedAt: n, UpdatedAt: n,
		}
		if err := h.SSOConfigs.Create(ctx, cfg); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := h.SSOConfigs.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		_, err := h.SSOConfigs.Get(ctx, id)
		assertNotFound(t, err)
	})

	t.Run("DeleteNotFound", func(t *testing.T) {
		err := h.SSOConfigs.Delete(ctx, "nonexistent")
		assertNotFound(t, err)
	})
}
