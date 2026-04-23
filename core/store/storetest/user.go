//go:build integration

package storetest

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// TestUserStore exercises the store.UserStore contract.
func TestUserStore(t *testing.T, h *Harness) {
	ctx := context.Background()
	ent := h.SeedEnterprise(t, ctx)

	t.Run("CreateAndGet", func(t *testing.T) {
		id := uid("usr")
		n := now()
		u := model.User{
			ID:           id,
			EnterpriseID: ent.ID,
			Email:        id + "@test.com",
			DisplayName:  "Alice",
			AvatarURL:    "https://example.com/alice.png",
			Role:         model.UserRoleAdmin,
			Status:       model.UserStatusActive,
			PasswordHash: "hash123",
			CreatedAt:    n,
			UpdatedAt:    n,
		}
		if err := h.Users.Create(ctx, u); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.Users.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.DisplayName != "Alice" {
			t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Alice")
		}
		if got.Role != model.UserRoleAdmin {
			t.Errorf("Role = %q, want %q", got.Role, model.UserRoleAdmin)
		}
		if got.PasswordHash != "hash123" {
			t.Errorf("PasswordHash = %q, want %q", got.PasswordHash, "hash123")
		}
	})

	t.Run("GetByEmail", func(t *testing.T) {
		u := h.SeedUser(t, ctx, ent.ID)
		got, err := h.Users.GetByEmail(ctx, u.Email)
		if err != nil {
			t.Fatalf("GetByEmail: %v", err)
		}
		if got.ID != u.ID {
			t.Errorf("ID = %q, want %q", got.ID, u.ID)
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		_, err := h.Users.Get(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("ListByEnterprise", func(t *testing.T) {
		ent2 := h.SeedEnterprise(t, ctx)
		h.SeedUser(t, ctx, ent2.ID)
		h.SeedUser(t, ctx, ent2.ID)

		users, err := h.Users.List(ctx, store.UserFilter{EnterpriseID: ent2.ID})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(users) != 2 {
			t.Errorf("got %d users, want 2", len(users))
		}
	})

	t.Run("ListByStatus", func(t *testing.T) {
		ent3 := h.SeedEnterprise(t, ctx)
		u := h.SeedUser(t, ctx, ent3.ID)
		// update one user to suspended
		u.Status = model.UserStatusSuspended
		u.UpdatedAt = now()
		if err := h.Users.Update(ctx, u); err != nil {
			t.Fatalf("Update: %v", err)
		}
		h.SeedUser(t, ctx, ent3.ID) // active user

		status := model.UserStatusActive
		users, err := h.Users.List(ctx, store.UserFilter{EnterpriseID: ent3.ID, Status: &status})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(users) != 1 {
			t.Errorf("got %d active users, want 1", len(users))
		}
	})

	t.Run("Update", func(t *testing.T) {
		u := h.SeedUser(t, ctx, ent.ID)
		u.DisplayName = "Bob"
		u.Role = model.UserRoleOwner
		u.UpdatedAt = now()
		if err := h.Users.Update(ctx, u); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := h.Users.Get(ctx, u.ID)
		if err != nil {
			t.Fatalf("Get after Update: %v", err)
		}
		if got.DisplayName != "Bob" {
			t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Bob")
		}
		if got.Role != model.UserRoleOwner {
			t.Errorf("Role = %q, want %q", got.Role, model.UserRoleOwner)
		}
	})

	t.Run("UpdateNotFound", func(t *testing.T) {
		u := model.User{ID: "nonexistent", EnterpriseID: ent.ID, Email: uid("x") + "@x.com", UpdatedAt: now()}
		err := h.Users.Update(ctx, u)
		assertNotFound(t, err)
	})
}
