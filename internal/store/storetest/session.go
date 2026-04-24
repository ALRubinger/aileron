//go:build integration

package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/model"
)

// TestSessionStore exercises the store.SessionStore contract.
func TestSessionStore(t *testing.T, h *Harness) {
	ctx := context.Background()
	ent := h.SeedEnterprise(t, ctx)
	user := h.SeedUser(t, ctx, ent.ID)

	t.Run("CreateAndGetByTokenHash", func(t *testing.T) {
		id := uid("sess")
		n := now()
		sess := model.Session{
			ID:               id,
			UserID:           user.ID,
			TokenHash:        uid("tok"),
			RefreshTokenHash: uid("ref"),
			ExpiresAt:        n.Add(time.Hour),
			CreatedAt:        n,
		}
		if err := h.Sessions.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.Sessions.GetByTokenHash(ctx, sess.TokenHash)
		if err != nil {
			t.Fatalf("GetByTokenHash: %v", err)
		}
		if got.ID != id {
			t.Errorf("ID = %q, want %q", got.ID, id)
		}
		if got.UserID != user.ID {
			t.Errorf("UserID = %q, want %q", got.UserID, user.ID)
		}
	})

	t.Run("GetByRefreshTokenHash", func(t *testing.T) {
		id := uid("sess")
		n := now()
		sess := model.Session{
			ID:               id,
			UserID:           user.ID,
			TokenHash:        uid("tok"),
			RefreshTokenHash: uid("ref"),
			ExpiresAt:        n.Add(time.Hour),
			CreatedAt:        n,
		}
		if err := h.Sessions.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.Sessions.GetByRefreshTokenHash(ctx, sess.RefreshTokenHash)
		if err != nil {
			t.Fatalf("GetByRefreshTokenHash: %v", err)
		}
		if got.ID != id {
			t.Errorf("ID = %q, want %q", got.ID, id)
		}
	})

	t.Run("GetByTokenHashNotFound", func(t *testing.T) {
		_, err := h.Sessions.GetByTokenHash(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("Delete", func(t *testing.T) {
		id := uid("sess")
		n := now()
		sess := model.Session{
			ID: id, UserID: user.ID,
			TokenHash: uid("tok"), RefreshTokenHash: uid("ref"),
			ExpiresAt: n.Add(time.Hour), CreatedAt: n,
		}
		if err := h.Sessions.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := h.Sessions.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		_, err := h.Sessions.GetByTokenHash(ctx, sess.TokenHash)
		assertNotFound(t, err)
	})

	t.Run("DeleteNotFound", func(t *testing.T) {
		err := h.Sessions.Delete(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("DeleteAllForUser", func(t *testing.T) {
		u2 := h.SeedUser(t, ctx, ent.ID)
		for i := 0; i < 3; i++ {
			n := now()
			sess := model.Session{
				ID: uid("sess"), UserID: u2.ID,
				TokenHash: uid("tok"), RefreshTokenHash: uid("ref"),
				ExpiresAt: n.Add(time.Hour), CreatedAt: n,
			}
			if err := h.Sessions.Create(ctx, sess); err != nil {
				t.Fatalf("Create[%d]: %v", i, err)
			}
		}

		if err := h.Sessions.DeleteAllForUser(ctx, u2.ID); err != nil {
			t.Fatalf("DeleteAllForUser: %v", err)
		}
	})
}
