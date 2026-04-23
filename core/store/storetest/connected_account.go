//go:build integration

package storetest

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// TestConnectedAccountStore exercises the store.ConnectedAccountStore contract.
func TestConnectedAccountStore(t *testing.T, h *Harness) {
	ctx := context.Background()
	ent := h.SeedEnterprise(t, ctx)
	user := h.SeedUser(t, ctx, ent.ID)

	t.Run("CreateAndGet", func(t *testing.T) {
		id := uid("conn")
		n := now()
		a := model.ConnectedAccount{
			ID:             id,
			UserID:         user.ID,
			Provider:       model.ConnectedAccountProviderSlack,
			Scopes:         []string{"chat:write", "channels:read"},
			Status:         model.ConnectedAccountStatusActive,
			ExternalUserID: "U123",
			ExternalTeamID: "T456",
			CreatedAt:      n,
			UpdatedAt:      n,
		}
		if err := h.ConnectedAccounts.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.ConnectedAccounts.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Provider != model.ConnectedAccountProviderSlack {
			t.Errorf("Provider = %q, want %q", got.Provider, model.ConnectedAccountProviderSlack)
		}
		if len(got.Scopes) != 2 || got.Scopes[0] != "chat:write" {
			t.Errorf("Scopes = %v, want [chat:write channels:read]", got.Scopes)
		}
		if got.ExternalUserID != "U123" {
			t.Errorf("ExternalUserID = %q, want %q", got.ExternalUserID, "U123")
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		_, err := h.ConnectedAccounts.Get(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("UpsertNew", func(t *testing.T) {
		u2 := h.SeedUser(t, ctx, ent.ID)
		n := now()
		a := model.ConnectedAccount{
			ID:       uid("conn"),
			UserID:   u2.ID,
			Provider: model.ConnectedAccountProviderGmail,
			Scopes:   []string{"gmail.readonly"},
			Status:   model.ConnectedAccountStatusActive,
			CreatedAt: n, UpdatedAt: n,
		}
		got, err := h.ConnectedAccounts.Upsert(ctx, a)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if got.Provider != model.ConnectedAccountProviderGmail {
			t.Errorf("Provider = %q, want %q", got.Provider, model.ConnectedAccountProviderGmail)
		}
	})

	t.Run("UpsertExisting", func(t *testing.T) {
		u3 := h.SeedUser(t, ctx, ent.ID)
		n := now()
		a := model.ConnectedAccount{
			ID: uid("conn"), UserID: u3.ID,
			Provider: model.ConnectedAccountProviderGitHub,
			Scopes:   []string{"repo"},
			Status:   model.ConnectedAccountStatusActive,
			CreatedAt: n, UpdatedAt: n,
		}
		first, err := h.ConnectedAccounts.Upsert(ctx, a)
		if err != nil {
			t.Fatalf("first Upsert: %v", err)
		}

		// Upsert again with updated scopes.
		a.ID = uid("conn") // new ID should be ignored; existing ID kept
		a.Scopes = []string{"repo", "read:org"}
		a.UpdatedAt = now()
		second, err := h.ConnectedAccounts.Upsert(ctx, a)
		if err != nil {
			t.Fatalf("second Upsert: %v", err)
		}
		if second.ID != first.ID {
			t.Errorf("ID changed from %q to %q on upsert", first.ID, second.ID)
		}

		got, err := h.ConnectedAccounts.Get(ctx, first.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(got.Scopes) != 2 || got.Scopes[1] != "read:org" {
			t.Errorf("Scopes = %v, want [repo read:org]", got.Scopes)
		}
	})

	t.Run("ListWithFilters", func(t *testing.T) {
		u4 := h.SeedUser(t, ctx, ent.ID)
		n := now()
		for _, p := range []model.ConnectedAccountProvider{
			model.ConnectedAccountProviderSlack,
			model.ConnectedAccountProviderGmail,
		} {
			a := model.ConnectedAccount{
				ID: uid("conn"), UserID: u4.ID,
				Provider: p, Status: model.ConnectedAccountStatusActive,
				CreatedAt: n, UpdatedAt: n,
			}
			if err := h.ConnectedAccounts.Create(ctx, a); err != nil {
				t.Fatalf("Create %s: %v", p, err)
			}
		}

		// Filter by provider.
		slack := model.ConnectedAccountProviderSlack
		accounts, err := h.ConnectedAccounts.List(ctx, store.ConnectedAccountFilter{
			UserID:   u4.ID,
			Provider: &slack,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(accounts) != 1 {
			t.Errorf("got %d accounts, want 1", len(accounts))
		}

		// All for user.
		all, err := h.ConnectedAccounts.List(ctx, store.ConnectedAccountFilter{UserID: u4.ID})
		if err != nil {
			t.Fatalf("List all: %v", err)
		}
		if len(all) != 2 {
			t.Errorf("got %d accounts, want 2", len(all))
		}
	})

	t.Run("Delete", func(t *testing.T) {
		u5 := h.SeedUser(t, ctx, ent.ID)
		id := uid("conn")
		n := now()
		a := model.ConnectedAccount{
			ID: id, UserID: u5.ID,
			Provider: model.ConnectedAccountProviderSlack,
			Status:   model.ConnectedAccountStatusActive,
			CreatedAt: n, UpdatedAt: n,
		}
		if err := h.ConnectedAccounts.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := h.ConnectedAccounts.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		_, err := h.ConnectedAccounts.Get(ctx, id)
		assertNotFound(t, err)
	})

	t.Run("DeleteNotFound", func(t *testing.T) {
		err := h.ConnectedAccounts.Delete(ctx, "nonexistent")
		assertNotFound(t, err)
	})
}
