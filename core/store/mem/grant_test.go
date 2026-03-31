package mem

import (
	"context"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
)

func TestGrantStore_CreateGetUpdate(t *testing.T) {
	s := NewGrantStore()
	ctx := context.Background()

	grant := api.ExecutionGrant{
		GrantId:   "grt_1",
		IntentId:  "int_1",
		Status:    api.ExecutionGrantStatusActive,
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}

	if err := s.Create(ctx, grant); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "grt_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != api.ExecutionGrantStatusActive {
		t.Errorf("Status = %q, want %q", got.Status, api.ExecutionGrantStatusActive)
	}

	got.Status = api.ExecutionGrantStatusConsumed
	if err := s.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ = s.Get(ctx, "grt_1")
	if got.Status != api.ExecutionGrantStatusConsumed {
		t.Errorf("Status = %q, want %q", got.Status, api.ExecutionGrantStatusConsumed)
	}
}

func TestGrantStore_GetNotFound(t *testing.T) {
	s := NewGrantStore()
	_, err := s.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}
