package mem

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store"
)

func TestUserKeyMaterialStore_CreateAndGet(t *testing.T) {
	s := NewUserKeyMaterialStore()
	ctx := context.Background()
	now := time.Now().UTC()

	material := model.UserKeyMaterial{
		UserID:          "usr_1",
		Salt:            []byte("salt-bytes-here!"),
		KEKVerification: []byte("verification-blob"),
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := s.Create(ctx, material); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "usr_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.UserID != material.UserID {
		t.Fatalf("UserID = %q, want %q", got.UserID, material.UserID)
	}
	if string(got.Salt) != string(material.Salt) {
		t.Fatal("Salt mismatch")
	}
	if string(got.KEKVerification) != string(material.KEKVerification) {
		t.Fatal("KEKVerification mismatch")
	}
}

func TestUserKeyMaterialStore_GetNotFound(t *testing.T) {
	s := NewUserKeyMaterialStore()
	_, err := s.Get(context.Background(), "usr_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing user")
	}
	if _, ok := err.(*store.ErrNotFound); !ok {
		t.Fatalf("expected ErrNotFound, got %T", err)
	}
}

func TestUserKeyMaterialStore_Update(t *testing.T) {
	s := NewUserKeyMaterialStore()
	ctx := context.Background()
	now := time.Now().UTC()

	material := model.UserKeyMaterial{
		UserID:          "usr_1",
		Salt:            []byte("original-salt!!!"),
		KEKVerification: []byte("original-verification"),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.Create(ctx, material)

	material.Salt = []byte("rotated-salt!!!!") // same length
	material.KEKVerification = []byte("rotated-verification")
	material.UpdatedAt = now.Add(time.Hour)

	if err := s.Update(ctx, material); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := s.Get(ctx, "usr_1")
	if string(got.Salt) != "rotated-salt!!!!" {
		t.Fatalf("Salt = %q, want rotated", got.Salt)
	}
}

func TestUserKeyMaterialStore_UpdateNotFound(t *testing.T) {
	s := NewUserKeyMaterialStore()
	err := s.Update(context.Background(), model.UserKeyMaterial{UserID: "usr_missing"})
	if err == nil {
		t.Fatal("expected error for missing user")
	}
	if _, ok := err.(*store.ErrNotFound); !ok {
		t.Fatalf("expected ErrNotFound, got %T", err)
	}
}
