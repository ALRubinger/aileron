package storetest

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// TestUserInstructionStore exercises the store.UserInstructionStore contract.
func TestUserInstructionStore(t *testing.T, h *Harness) {
	ctx := context.Background()
	ent := h.SeedEnterprise(t, ctx)
	user := h.SeedUser(t, ctx, ent.ID)

	t.Run("CreateAndGet", func(t *testing.T) {
		id := uid("ins")
		n := now()
		ins := model.UserInstruction{
			ID:        id,
			UserID:    user.ID,
			Body:      "Always reply formally",
			Scope:     "slack:#important",
			Active:    true,
			CreatedAt: n,
			UpdatedAt: n,
		}
		if err := h.UserInstructions.Create(ctx, ins); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.UserInstructions.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Body != "Always reply formally" {
			t.Errorf("Body = %q, want %q", got.Body, "Always reply formally")
		}
		if got.Scope != "slack:#important" {
			t.Errorf("Scope = %q, want %q", got.Scope, "slack:#important")
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		_, err := h.UserInstructions.Get(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("ListByUserAndActive", func(t *testing.T) {
		u2 := h.SeedUser(t, ctx, ent.ID)
		n := now()
		for _, active := range []bool{true, true, false} {
			ins := model.UserInstruction{
				ID: uid("ins"), UserID: u2.ID,
				Body: "instruction", Active: active,
				CreatedAt: n, UpdatedAt: n,
			}
			if err := h.UserInstructions.Create(ctx, ins); err != nil {
				t.Fatalf("Create: %v", err)
			}
		}

		// All instructions.
		all, err := h.UserInstructions.List(ctx, store.UserInstructionFilter{UserID: u2.ID})
		if err != nil {
			t.Fatalf("List all: %v", err)
		}
		if len(all) != 3 {
			t.Errorf("got %d, want 3", len(all))
		}

		// Active only.
		active := true
		activeList, err := h.UserInstructions.List(ctx, store.UserInstructionFilter{UserID: u2.ID, Active: &active})
		if err != nil {
			t.Fatalf("List active: %v", err)
		}
		if len(activeList) != 2 {
			t.Errorf("got %d active, want 2", len(activeList))
		}
	})

	t.Run("Update", func(t *testing.T) {
		id := uid("ins")
		n := now()
		ins := model.UserInstruction{
			ID: id, UserID: user.ID,
			Body: "old body", Active: true,
			CreatedAt: n, UpdatedAt: n,
		}
		if err := h.UserInstructions.Create(ctx, ins); err != nil {
			t.Fatalf("Create: %v", err)
		}

		ins.Body = "new body"
		ins.Active = false
		ins.UpdatedAt = now()
		if err := h.UserInstructions.Update(ctx, ins); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := h.UserInstructions.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Body != "new body" {
			t.Errorf("Body = %q, want %q", got.Body, "new body")
		}
		if got.Active {
			t.Error("Active = true, want false")
		}
	})

	t.Run("UpdateNotFound", func(t *testing.T) {
		ins := model.UserInstruction{ID: "nonexistent", UpdatedAt: now()}
		err := h.UserInstructions.Update(ctx, ins)
		assertNotFound(t, err)
	})

	t.Run("Delete", func(t *testing.T) {
		id := uid("ins")
		n := now()
		ins := model.UserInstruction{
			ID: id, UserID: user.ID,
			Body: "to delete", Active: true,
			CreatedAt: n, UpdatedAt: n,
		}
		if err := h.UserInstructions.Create(ctx, ins); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := h.UserInstructions.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		_, err := h.UserInstructions.Get(ctx, id)
		assertNotFound(t, err)
	})

	t.Run("DeleteNotFound", func(t *testing.T) {
		err := h.UserInstructions.Delete(ctx, "nonexistent")
		assertNotFound(t, err)
	})
}
