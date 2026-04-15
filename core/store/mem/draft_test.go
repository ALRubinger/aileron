package mem_test

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
)

func newTestDraft(id, userID string, status model.DraftStatus) model.Draft {
	return model.Draft{
		ID:          id,
		UserID:      userID,
		Status:      status,
		Service:     "slack",
		Channel:     "#backend",
		Author:      "Sarah",
		MessageBody: "Does the JWT refactor change the claims?",
		DraftBody:   "No, the claims stay the same.",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
}

func TestDraftStore_CreateAndGet(t *testing.T) {
	s := mem.NewDraftStore()
	ctx := context.Background()
	d := newTestDraft("dft_1", "usr_1", model.DraftStatusPending)

	if err := s.Create(ctx, d); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "dft_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "dft_1" {
		t.Errorf("expected dft_1, got %s", got.ID)
	}
	if got.DraftBody != "No, the claims stay the same." {
		t.Errorf("unexpected draft body: %s", got.DraftBody)
	}
}

func TestDraftStore_GetNotFound(t *testing.T) {
	s := mem.NewDraftStore()
	_, err := s.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing draft")
	}
}

func TestDraftStore_List(t *testing.T) {
	s := mem.NewDraftStore()
	ctx := context.Background()

	s.Create(ctx, newTestDraft("dft_1", "usr_1", model.DraftStatusPending))
	s.Create(ctx, newTestDraft("dft_2", "usr_1", model.DraftStatusApproved))
	s.Create(ctx, newTestDraft("dft_3", "usr_2", model.DraftStatusPending))

	// List all for user 1.
	drafts, err := s.List(ctx, store.DraftFilter{UserID: "usr_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 2 {
		t.Fatalf("expected 2 drafts for usr_1, got %d", len(drafts))
	}

	// Filter by status.
	pending := model.DraftStatusPending
	drafts, err = s.List(ctx, store.DraftFilter{UserID: "usr_1", Status: &pending})
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 {
		t.Fatalf("expected 1 pending draft, got %d", len(drafts))
	}
}

func TestDraftStore_Update(t *testing.T) {
	s := mem.NewDraftStore()
	ctx := context.Background()

	d := newTestDraft("dft_1", "usr_1", model.DraftStatusPending)
	s.Create(ctx, d)

	d.Status = model.DraftStatusApproved
	d.SentBody = d.DraftBody
	if err := s.Update(ctx, d); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(ctx, "dft_1")
	if got.Status != model.DraftStatusApproved {
		t.Errorf("expected approved, got %s", got.Status)
	}
	if got.SentBody != "No, the claims stay the same." {
		t.Errorf("unexpected sent body: %s", got.SentBody)
	}
}

func TestDraftStore_UpdateNotFound(t *testing.T) {
	s := mem.NewDraftStore()
	err := s.Update(context.Background(), model.Draft{ID: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for missing draft")
	}
}
