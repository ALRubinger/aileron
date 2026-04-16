package mem_test

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
)

func TestFeedbackStore_CreateAndList(t *testing.T) {
	s := mem.NewDraftFeedbackStore()
	ctx := context.Background()

	s.Create(ctx, model.DraftFeedback{
		ID: "fb_1", UserID: "usr_1", DraftID: "dft_1",
		Signal: model.FeedbackSignalApproved, Channel: "#backend",
		DraftBody: "draft text", SentBody: "draft text",
		CreatedAt: time.Now().UTC(),
	})
	s.Create(ctx, model.DraftFeedback{
		ID: "fb_2", UserID: "usr_1", DraftID: "dft_2",
		Signal: model.FeedbackSignalEdited, Channel: "#backend",
		DraftBody: "original", SentBody: "revised",
		CreatedAt: time.Now().UTC(),
	})
	s.Create(ctx, model.DraftFeedback{
		ID: "fb_3", UserID: "usr_2", DraftID: "dft_3",
		Signal: model.FeedbackSignalDiscarded, Channel: "#incidents",
		DraftBody: "bad draft", CreatedAt: time.Now().UTC(),
	})

	// All for user 1.
	all, err := s.List(ctx, store.DraftFeedbackFilter{UserID: "usr_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}

	// Filter by signal.
	edited := model.FeedbackSignalEdited
	filtered, err := s.List(ctx, store.DraftFeedbackFilter{UserID: "usr_1", Signal: &edited})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1 edited, got %d", len(filtered))
	}
	if filtered[0].DraftBody != "original" {
		t.Errorf("expected original, got %s", filtered[0].DraftBody)
	}
	if filtered[0].SentBody != "revised" {
		t.Errorf("expected revised, got %s", filtered[0].SentBody)
	}

	// All signals (no filter).
	everything, _ := s.List(ctx, store.DraftFeedbackFilter{})
	if len(everything) != 3 {
		t.Fatalf("expected 3 total, got %d", len(everything))
	}
}

func TestFeedbackStore_Empty(t *testing.T) {
	s := mem.NewDraftFeedbackStore()
	result, err := s.List(context.Background(), store.DraftFeedbackFilter{UserID: "usr_1"})
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}
