package mem_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
	"github.com/ALRubinger/aileron/internal/store/mem"
)

func TestUserStore_CreateAndGet(t *testing.T) {
	s := mem.NewUserStore()
	user := model.User{
		ID:           "usr_1",
		EnterpriseID: "ent_1",
		Email:        "test@example.com",
		DisplayName:  "Test User",
		Role:         model.UserRoleAdmin,
		Status:       model.UserStatusActive,
	}

	if err := s.Create(context.Background(), user); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := s.Get(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Email != "test@example.com" {
		t.Errorf("expected test@example.com, got %s", got.Email)
	}
}

func TestUserStore_Get_NotFound(t *testing.T) {
	s := mem.NewUserStore()
	_, err := s.Get(context.Background(), "usr_missing")
	if err == nil {
		t.Fatal("expected error")
	}
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}

func TestUserStore_GetByEmail(t *testing.T) {
	s := mem.NewUserStore()
	s.Create(context.Background(), model.User{
		ID:    "usr_1",
		Email: "found@example.com",
	})

	got, err := s.GetByEmail(context.Background(), "found@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "usr_1" {
		t.Errorf("expected usr_1, got %s", got.ID)
	}
}

func TestUserStore_GetByEmail_NotFound(t *testing.T) {
	s := mem.NewUserStore()
	_, err := s.GetByEmail(context.Background(), "missing@example.com")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestUserStore_List(t *testing.T) {
	s := mem.NewUserStore()
	s.Create(context.Background(), model.User{ID: "usr_1", EnterpriseID: "ent_1", Email: "a@x.com"})
	s.Create(context.Background(), model.User{ID: "usr_2", EnterpriseID: "ent_2", Email: "b@x.com"})

	// List all.
	all, err := s.List(context.Background(), store.UserFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}

	// Filter by enterprise.
	filtered, err := s.List(context.Background(), store.UserFilter{EnterpriseID: "ent_1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1, got %d", len(filtered))
	}
}

func TestUserStore_List_FilterByStatus(t *testing.T) {
	s := mem.NewUserStore()
	active := model.UserStatusActive
	s.Create(context.Background(), model.User{ID: "usr_1", Email: "a@x.com", Status: model.UserStatusActive})
	s.Create(context.Background(), model.User{ID: "usr_2", Email: "b@x.com", Status: model.UserStatusSuspended})

	filtered, err := s.List(context.Background(), store.UserFilter{Status: &active})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1, got %d", len(filtered))
	}
}

func TestUserStore_Update(t *testing.T) {
	s := mem.NewUserStore()
	s.Create(context.Background(), model.User{ID: "usr_1", Email: "old@x.com"})

	err := s.Update(context.Background(), model.User{ID: "usr_1", Email: "new@x.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _ := s.Get(context.Background(), "usr_1")
	if got.Email != "new@x.com" {
		t.Errorf("expected new@x.com, got %s", got.Email)
	}
}

func TestUserStore_Update_NotFound(t *testing.T) {
	s := mem.NewUserStore()
	err := s.Update(context.Background(), model.User{ID: "usr_missing"})
	if err == nil {
		t.Fatal("expected error")
	}
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}
