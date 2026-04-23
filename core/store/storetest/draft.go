package storetest

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// TestDraftStore exercises the store.DraftStore contract.
func TestDraftStore(t *testing.T, h *Harness) {
	ctx := context.Background()
	ent := h.SeedEnterprise(t, ctx)
	user := h.SeedUser(t, ctx, ent.ID)

	t.Run("CreateAndGet", func(t *testing.T) {
		id := uid("dft")
		n := now()
		d := model.Draft{
			ID:          id,
			UserID:      user.ID,
			Status:      model.DraftStatusPending,
			Service:     "slack",
			Channel:     "#general",
			Author:      "bot",
			MessageBody: "original message",
			MessageTS:   "1234567890.123456",
			DraftBody:   "draft reply",
			SentBody:    "",
			CreatedAt:   n,
			UpdatedAt:   n,
		}
		if err := h.Drafts.Create(ctx, d); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.Drafts.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != model.DraftStatusPending {
			t.Errorf("Status = %q, want %q", got.Status, model.DraftStatusPending)
		}
		if got.Service != "slack" {
			t.Errorf("Service = %q, want %q", got.Service, "slack")
		}
		if got.DraftBody != "draft reply" {
			t.Errorf("DraftBody = %q, want %q", got.DraftBody, "draft reply")
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		_, err := h.Drafts.Get(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("ListByUser", func(t *testing.T) {
		u2 := h.SeedUser(t, ctx, ent.ID)
		n := now()
		for i := 0; i < 3; i++ {
			d := model.Draft{
				ID: uid("dft"), UserID: u2.ID,
				Status: model.DraftStatusPending, Service: "slack",
				Channel: "#ch", Author: "bot",
				MessageBody: "msg", MessageTS: uid("ts"),
				DraftBody: "body",
				CreatedAt: n, UpdatedAt: n,
			}
			if err := h.Drafts.Create(ctx, d); err != nil {
				t.Fatalf("Create[%d]: %v", i, err)
			}
		}

		drafts, err := h.Drafts.List(ctx, store.DraftFilter{UserID: u2.ID})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(drafts) != 3 {
			t.Errorf("got %d drafts, want 3", len(drafts))
		}
	})

	t.Run("ListByStatus", func(t *testing.T) {
		u3 := h.SeedUser(t, ctx, ent.ID)
		n := now()
		for _, s := range []model.DraftStatus{model.DraftStatusPending, model.DraftStatusApproved} {
			d := model.Draft{
				ID: uid("dft"), UserID: u3.ID,
				Status: s, Service: "slack",
				Channel: "#ch", Author: "bot",
				MessageBody: "msg", MessageTS: uid("ts"),
				DraftBody: "body",
				CreatedAt: n, UpdatedAt: n,
			}
			if err := h.Drafts.Create(ctx, d); err != nil {
				t.Fatalf("Create: %v", err)
			}
		}

		pending := model.DraftStatusPending
		drafts, err := h.Drafts.List(ctx, store.DraftFilter{UserID: u3.ID, Status: &pending})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(drafts) != 1 {
			t.Errorf("got %d pending drafts, want 1", len(drafts))
		}
	})

	t.Run("Update", func(t *testing.T) {
		id := uid("dft")
		n := now()
		d := model.Draft{
			ID: id, UserID: user.ID,
			Status: model.DraftStatusPending, Service: "slack",
			Channel: "#ch", Author: "bot",
			MessageBody: "msg", MessageTS: uid("ts"),
			DraftBody: "original draft",
			CreatedAt: n, UpdatedAt: n,
		}
		if err := h.Drafts.Create(ctx, d); err != nil {
			t.Fatalf("Create: %v", err)
		}

		d.Status = model.DraftStatusApproved
		d.SentBody = "approved text"
		d.UpdatedAt = now()
		if err := h.Drafts.Update(ctx, d); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := h.Drafts.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != model.DraftStatusApproved {
			t.Errorf("Status = %q, want %q", got.Status, model.DraftStatusApproved)
		}
		if got.SentBody != "approved text" {
			t.Errorf("SentBody = %q, want %q", got.SentBody, "approved text")
		}
	})

	t.Run("UpdateNotFound", func(t *testing.T) {
		d := model.Draft{ID: "nonexistent", UserID: user.ID, UpdatedAt: now()}
		err := h.Drafts.Update(ctx, d)
		assertNotFound(t, err)
	})
}
