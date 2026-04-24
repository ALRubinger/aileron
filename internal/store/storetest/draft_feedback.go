//go:build integration

package storetest

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
)

// TestDraftFeedbackStore exercises the store.DraftFeedbackStore contract.
func TestDraftFeedbackStore(t *testing.T, h *Harness) {
	ctx := context.Background()
	ent := h.SeedEnterprise(t, ctx)
	user := h.SeedUser(t, ctx, ent.ID)

	t.Run("CreateAndList", func(t *testing.T) {
		n := now()
		fb := model.DraftFeedback{
			ID:        uid("fb"),
			UserID:    user.ID,
			DraftID:   uid("dft"),
			Signal:    model.FeedbackSignalApproved,
			Service:   "slack",
			Channel:   "#general",
			DraftBody: "draft text",
			SentBody:  "sent text",
			ToolsUsed: []string{"gmail", "calendar"},
			CreatedAt: n,
		}
		if err := h.DraftFeedback.Create(ctx, fb); err != nil {
			t.Fatalf("Create: %v", err)
		}

		list, err := h.DraftFeedback.List(ctx, store.DraftFeedbackFilter{UserID: user.ID})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		found := false
		for _, f := range list {
			if f.ID == fb.ID {
				found = true
				if f.Signal != model.FeedbackSignalApproved {
					t.Errorf("Signal = %q, want %q", f.Signal, model.FeedbackSignalApproved)
				}
				if len(f.ToolsUsed) != 2 || f.ToolsUsed[0] != "gmail" {
					t.Errorf("ToolsUsed = %v, want [gmail calendar]", f.ToolsUsed)
				}
			}
		}
		if !found {
			t.Error("created feedback not found in List results")
		}
	})

	t.Run("ListBySignal", func(t *testing.T) {
		u2 := h.SeedUser(t, ctx, ent.ID)
		n := now()
		for _, sig := range []model.FeedbackSignal{
			model.FeedbackSignalApproved,
			model.FeedbackSignalEdited,
			model.FeedbackSignalDiscarded,
		} {
			fb := model.DraftFeedback{
				ID: uid("fb"), UserID: u2.ID,
				DraftID: uid("dft"), Signal: sig,
				Service: "slack", Channel: "#ch",
				DraftBody: "body",
				CreatedAt: n,
			}
			if err := h.DraftFeedback.Create(ctx, fb); err != nil {
				t.Fatalf("Create: %v", err)
			}
		}

		edited := model.FeedbackSignalEdited
		list, err := h.DraftFeedback.List(ctx, store.DraftFeedbackFilter{
			UserID: u2.ID,
			Signal: &edited,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("got %d edited feedbacks, want 1", len(list))
		}
	})

	t.Run("ListEmpty", func(t *testing.T) {
		u3 := h.SeedUser(t, ctx, ent.ID)
		list, err := h.DraftFeedback.List(ctx, store.DraftFeedbackFilter{UserID: u3.ID})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("got %d feedbacks, want 0", len(list))
		}
	})
}
