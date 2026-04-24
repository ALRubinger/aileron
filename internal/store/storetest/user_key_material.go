//go:build integration

package storetest

import (
	"bytes"
	"context"
	"testing"

	"github.com/ALRubinger/aileron/internal/model"
)

// TestUserKeyMaterialStore exercises the store.UserKeyMaterialStore contract.
func TestUserKeyMaterialStore(t *testing.T, h *Harness) {
	ctx := context.Background()
	ent := h.SeedEnterprise(t, ctx)

	t.Run("CreateAndGet", func(t *testing.T) {
		user := h.SeedUser(t, ctx, ent.ID)
		n := now()
		m := model.UserKeyMaterial{
			UserID:          user.ID,
			Salt:            []byte("random-salt-bytes"),
			KEKVerification: []byte("encrypted-verification"),
			CreatedAt:       n,
			UpdatedAt:       n,
		}
		if err := h.UserKeyMaterials.Create(ctx, m); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.UserKeyMaterials.Get(ctx, user.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !bytes.Equal(got.Salt, []byte("random-salt-bytes")) {
			t.Errorf("Salt = %v, want %v", got.Salt, []byte("random-salt-bytes"))
		}
		if !bytes.Equal(got.KEKVerification, []byte("encrypted-verification")) {
			t.Errorf("KEKVerification mismatch")
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		_, err := h.UserKeyMaterials.Get(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("Update", func(t *testing.T) {
		user := h.SeedUser(t, ctx, ent.ID)
		n := now()
		m := model.UserKeyMaterial{
			UserID:          user.ID,
			Salt:            []byte("old-salt"),
			KEKVerification: []byte("old-kek"),
			CreatedAt:       n,
			UpdatedAt:       n,
		}
		if err := h.UserKeyMaterials.Create(ctx, m); err != nil {
			t.Fatalf("Create: %v", err)
		}

		m.Salt = []byte("new-salt")
		m.KEKVerification = []byte("new-kek")
		m.UpdatedAt = now()
		if err := h.UserKeyMaterials.Update(ctx, m); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := h.UserKeyMaterials.Get(ctx, user.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !bytes.Equal(got.Salt, []byte("new-salt")) {
			t.Errorf("Salt = %v, want %v", got.Salt, []byte("new-salt"))
		}
	})

	t.Run("UpdateNotFound", func(t *testing.T) {
		m := model.UserKeyMaterial{
			UserID:          "nonexistent",
			Salt:            []byte("s"),
			KEKVerification: []byte("k"),
			UpdatedAt:       now(),
		}
		err := h.UserKeyMaterials.Update(ctx, m)
		assertNotFound(t, err)
	})
}
