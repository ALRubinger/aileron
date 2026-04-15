package mem_test

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
)

func newTestInstruction(id, userID, body string, active bool) model.UserInstruction {
	return model.UserInstruction{
		ID:        id,
		UserID:    userID,
		Body:      body,
		Active:    active,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
}

func TestInstructionStore_CreateAndGet(t *testing.T) {
	s := mem.NewUserInstructionStore()
	ctx := context.Background()
	ins := newTestInstruction("ins_1", "usr_1", "Always reference PR numbers", true)

	if err := s.Create(ctx, ins); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "ins_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "Always reference PR numbers" {
		t.Errorf("unexpected body: %s", got.Body)
	}
}

func TestInstructionStore_GetNotFound(t *testing.T) {
	s := mem.NewUserInstructionStore()
	_, err := s.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestInstructionStore_List(t *testing.T) {
	s := mem.NewUserInstructionStore()
	ctx := context.Background()

	s.Create(ctx, newTestInstruction("ins_1", "usr_1", "Rule 1", true))
	s.Create(ctx, newTestInstruction("ins_2", "usr_1", "Rule 2", false))
	s.Create(ctx, newTestInstruction("ins_3", "usr_2", "Rule 3", true))

	// All for user 1.
	all, _ := s.List(ctx, store.UserInstructionFilter{UserID: "usr_1"})
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}

	// Active only.
	active := true
	filtered, _ := s.List(ctx, store.UserInstructionFilter{UserID: "usr_1", Active: &active})
	if len(filtered) != 1 {
		t.Fatalf("expected 1 active, got %d", len(filtered))
	}

	// Inactive only.
	inactive := false
	filtered, _ = s.List(ctx, store.UserInstructionFilter{UserID: "usr_1", Active: &inactive})
	if len(filtered) != 1 {
		t.Fatalf("expected 1 inactive, got %d", len(filtered))
	}
}

func TestInstructionStore_Update(t *testing.T) {
	s := mem.NewUserInstructionStore()
	ctx := context.Background()

	ins := newTestInstruction("ins_1", "usr_1", "Original", true)
	s.Create(ctx, ins)

	ins.Body = "Updated"
	ins.Active = false
	if err := s.Update(ctx, ins); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(ctx, "ins_1")
	if got.Body != "Updated" {
		t.Errorf("expected Updated, got %s", got.Body)
	}
	if got.Active {
		t.Error("expected inactive")
	}
}

func TestInstructionStore_UpdateNotFound(t *testing.T) {
	s := mem.NewUserInstructionStore()
	err := s.Update(context.Background(), model.UserInstruction{ID: "nonexistent"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestInstructionStore_Delete(t *testing.T) {
	s := mem.NewUserInstructionStore()
	ctx := context.Background()

	s.Create(ctx, newTestInstruction("ins_1", "usr_1", "Rule", true))
	if err := s.Delete(ctx, "ins_1"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get(ctx, "ins_1")
	if err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestInstructionStore_DeleteNotFound(t *testing.T) {
	s := mem.NewUserInstructionStore()
	err := s.Delete(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}
