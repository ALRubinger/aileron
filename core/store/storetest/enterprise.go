//go:build integration

package storetest

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
)

// TestEnterpriseStore exercises the store.EnterpriseStore contract.
func TestEnterpriseStore(t *testing.T, h *Harness) {
	ctx := context.Background()

	t.Run("CreateAndGet", func(t *testing.T) {
		id := uid("ent")
		n := now()
		e := model.Enterprise{
			ID:                   id,
			Name:                 "Acme Corp",
			Slug:                 id,
			Plan:                 model.EnterprisePlanPro,
			BillingEmail:         id + "@acme.com",
			Personal:             false,
			SSORequired:          true,
			AllowedAuthProviders: []string{"google", "okta"},
			AllowedEmailDomains:  []string{"acme.com"},
			CreatedAt:            n,
			UpdatedAt:            n,
		}
		if err := h.Enterprises.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.Enterprises.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Name != "Acme Corp" {
			t.Errorf("Name = %q, want %q", got.Name, "Acme Corp")
		}
		if got.Plan != model.EnterprisePlanPro {
			t.Errorf("Plan = %q, want %q", got.Plan, model.EnterprisePlanPro)
		}
		if !got.SSORequired {
			t.Error("SSORequired = false, want true")
		}
		if len(got.AllowedAuthProviders) != 2 || got.AllowedAuthProviders[0] != "google" {
			t.Errorf("AllowedAuthProviders = %v, want [google okta]", got.AllowedAuthProviders)
		}
		if len(got.AllowedEmailDomains) != 1 || got.AllowedEmailDomains[0] != "acme.com" {
			t.Errorf("AllowedEmailDomains = %v, want [acme.com]", got.AllowedEmailDomains)
		}
	})

	t.Run("GetBySlug", func(t *testing.T) {
		e := h.SeedEnterprise(t, ctx)
		got, err := h.Enterprises.GetBySlug(ctx, e.Slug)
		if err != nil {
			t.Fatalf("GetBySlug: %v", err)
		}
		if got.ID != e.ID {
			t.Errorf("ID = %q, want %q", got.ID, e.ID)
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		_, err := h.Enterprises.Get(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("Update", func(t *testing.T) {
		e := h.SeedEnterprise(t, ctx)
		e.Name = "Updated Name"
		e.Plan = model.EnterprisePlanEnterprise
		e.UpdatedAt = now()
		if err := h.Enterprises.Update(ctx, e); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := h.Enterprises.Get(ctx, e.ID)
		if err != nil {
			t.Fatalf("Get after Update: %v", err)
		}
		if got.Name != "Updated Name" {
			t.Errorf("Name = %q, want %q", got.Name, "Updated Name")
		}
		if got.Plan != model.EnterprisePlanEnterprise {
			t.Errorf("Plan = %q, want %q", got.Plan, model.EnterprisePlanEnterprise)
		}
	})

	t.Run("UpdateNotFound", func(t *testing.T) {
		e := model.Enterprise{ID: "nonexistent", Slug: uid("slug"), BillingEmail: "x@x.com", UpdatedAt: now()}
		err := h.Enterprises.Update(ctx, e)
		assertNotFound(t, err)
	})
}
