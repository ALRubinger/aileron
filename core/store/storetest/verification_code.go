package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
)

// TestVerificationCodeStore exercises the store.VerificationCodeStore contract.
func TestVerificationCodeStore(t *testing.T, h *Harness) {
	ctx := context.Background()
	ent := h.SeedEnterprise(t, ctx)
	user := h.SeedUser(t, ctx, ent.ID)

	t.Run("CreateAndGetActive", func(t *testing.T) {
		id := uid("vc")
		n := now()
		code := model.VerificationCode{
			ID:        id,
			UserID:    user.ID,
			CodeHash:  "sha256_hash_value",
			ExpiresAt: n.Add(10 * time.Minute),
			Used:      false,
			CreatedAt: n,
		}
		if err := h.VerificationCodes.Create(ctx, code); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.VerificationCodes.GetActiveByUserID(ctx, user.ID)
		if err != nil {
			t.Fatalf("GetActiveByUserID: %v", err)
		}
		if got.ID != id {
			t.Errorf("ID = %q, want %q", got.ID, id)
		}
		if got.CodeHash != "sha256_hash_value" {
			t.Errorf("CodeHash = %q, want %q", got.CodeHash, "sha256_hash_value")
		}
	})

	t.Run("GetActiveFiltersUsed", func(t *testing.T) {
		u2 := h.SeedUser(t, ctx, ent.ID)
		n := now()
		code := model.VerificationCode{
			ID: uid("vc"), UserID: u2.ID,
			CodeHash: "hash", ExpiresAt: n.Add(10 * time.Minute),
			Used: true, CreatedAt: n,
		}
		if err := h.VerificationCodes.Create(ctx, code); err != nil {
			t.Fatalf("Create: %v", err)
		}

		_, err := h.VerificationCodes.GetActiveByUserID(ctx, u2.ID)
		assertNotFound(t, err)
	})

	t.Run("GetActiveFiltersExpired", func(t *testing.T) {
		u3 := h.SeedUser(t, ctx, ent.ID)
		n := now()
		code := model.VerificationCode{
			ID: uid("vc"), UserID: u3.ID,
			CodeHash: "hash", ExpiresAt: n.Add(-time.Minute),
			Used: false, CreatedAt: n,
		}
		if err := h.VerificationCodes.Create(ctx, code); err != nil {
			t.Fatalf("Create: %v", err)
		}

		_, err := h.VerificationCodes.GetActiveByUserID(ctx, u3.ID)
		assertNotFound(t, err)
	})

	t.Run("MarkUsed", func(t *testing.T) {
		u4 := h.SeedUser(t, ctx, ent.ID)
		id := uid("vc")
		n := now()
		code := model.VerificationCode{
			ID: id, UserID: u4.ID,
			CodeHash: "hash", ExpiresAt: n.Add(10 * time.Minute),
			Used: false, CreatedAt: n,
		}
		if err := h.VerificationCodes.Create(ctx, code); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := h.VerificationCodes.MarkUsed(ctx, id); err != nil {
			t.Fatalf("MarkUsed: %v", err)
		}

		// Should no longer be active.
		_, err := h.VerificationCodes.GetActiveByUserID(ctx, u4.ID)
		assertNotFound(t, err)
	})

	t.Run("MarkUsedNotFound", func(t *testing.T) {
		err := h.VerificationCodes.MarkUsed(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("DeleteExpiredForUser", func(t *testing.T) {
		u5 := h.SeedUser(t, ctx, ent.ID)
		n := now()
		// Create an expired code and a used code.
		for _, code := range []model.VerificationCode{
			{ID: uid("vc"), UserID: u5.ID, CodeHash: "h1", ExpiresAt: n.Add(-time.Minute), Used: false, CreatedAt: n},
			{ID: uid("vc"), UserID: u5.ID, CodeHash: "h2", ExpiresAt: n.Add(10 * time.Minute), Used: true, CreatedAt: n},
		} {
			if err := h.VerificationCodes.Create(ctx, code); err != nil {
				t.Fatalf("Create: %v", err)
			}
		}

		if err := h.VerificationCodes.DeleteExpiredForUser(ctx, u5.ID); err != nil {
			t.Fatalf("DeleteExpiredForUser: %v", err)
		}

		// Both should be cleaned up (expired + used).
		_, err := h.VerificationCodes.GetActiveByUserID(ctx, u5.ID)
		assertNotFound(t, err)
	})
}
