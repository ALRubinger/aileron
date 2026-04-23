package storetest

import (
	"context"
	"testing"
	"time"
)

// TestEscrowIndexStore exercises the store.EscrowIndexStore contract.
func TestEscrowIndexStore(t *testing.T, h *Harness) {
	ctx := context.Background()

	t.Run("Upsert", func(t *testing.T) {
		path := uid("vault/path")
		if err := h.EscrowIndex.Upsert(ctx, path, "esc_001", "usr_1", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		entries, err := h.EscrowIndex.LoadAll(ctx)
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		if got, want := entries[path], "esc_001"; got != want {
			t.Errorf("LoadAll[%s] = %q, want %q", path, got, want)
		}
	})

	t.Run("UpsertOverwrite", func(t *testing.T) {
		path := uid("vault/path")
		if err := h.EscrowIndex.Upsert(ctx, path, "esc_001", "usr_1", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("first Upsert: %v", err)
		}
		if err := h.EscrowIndex.Upsert(ctx, path, "esc_002", "usr_1", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("second Upsert: %v", err)
		}

		entries, err := h.EscrowIndex.LoadAll(ctx)
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		if got, want := entries[path], "esc_002"; got != want {
			t.Errorf("after overwrite: %q, want %q", got, want)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		path := uid("vault/path")
		if err := h.EscrowIndex.Upsert(ctx, path, "esc_001", "usr_1", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := h.EscrowIndex.Delete(ctx, path); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		entries, err := h.EscrowIndex.LoadAll(ctx)
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		if _, ok := entries[path]; ok {
			t.Error("deleted entry still in LoadAll")
		}
	})

	t.Run("DeleteNonexistent", func(t *testing.T) {
		if err := h.EscrowIndex.Delete(ctx, "vault/does-not-exist"); err != nil {
			t.Fatalf("Delete non-existent: %v", err)
		}
	})

	t.Run("LoadAllFiltersExpired", func(t *testing.T) {
		expired := uid("vault/expired")
		valid := uid("vault/valid")
		if err := h.EscrowIndex.Upsert(ctx, expired, "esc_old", "usr_1", time.Now().Add(-time.Hour)); err != nil {
			t.Fatalf("Upsert expired: %v", err)
		}
		if err := h.EscrowIndex.Upsert(ctx, valid, "esc_new", "usr_1", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("Upsert valid: %v", err)
		}

		entries, err := h.EscrowIndex.LoadAll(ctx)
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		if _, ok := entries[expired]; ok {
			t.Error("LoadAll should not return expired entries")
		}
		if got, want := entries[valid], "esc_new"; got != want {
			t.Errorf("LoadAll[valid] = %q, want %q", got, want)
		}
	})

	t.Run("DeleteExpired", func(t *testing.T) {
		exp1 := uid("vault/exp")
		exp2 := uid("vault/exp")
		valid := uid("vault/valid")
		if err := h.EscrowIndex.Upsert(ctx, exp1, "e1", "usr_1", time.Now().Add(-time.Hour)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := h.EscrowIndex.Upsert(ctx, exp2, "e2", "usr_1", time.Now().Add(-2*time.Hour)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := h.EscrowIndex.Upsert(ctx, valid, "e3", "usr_1", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		removed, err := h.EscrowIndex.DeleteExpired(ctx)
		if err != nil {
			t.Fatalf("DeleteExpired: %v", err)
		}
		// At least 2 expired entries should be removed (may be more from other subtests).
		if removed < 2 {
			t.Errorf("DeleteExpired removed %d, want >= 2", removed)
		}

		entries, err := h.EscrowIndex.LoadAll(ctx)
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		if _, ok := entries[valid]; !ok {
			t.Error("valid entry should survive DeleteExpired")
		}
	})

	t.Run("DeleteExpiredReturnsZeroWhenNoneExpired", func(t *testing.T) {
		// Clean slate: delete all expired first.
		if _, err := h.EscrowIndex.DeleteExpired(ctx); err != nil {
			t.Fatalf("cleanup DeleteExpired: %v", err)
		}

		path := uid("vault/valid")
		if err := h.EscrowIndex.Upsert(ctx, path, "esc_1", "usr_1", time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		removed, err := h.EscrowIndex.DeleteExpired(ctx)
		if err != nil {
			t.Fatalf("DeleteExpired: %v", err)
		}
		if removed != 0 {
			t.Errorf("DeleteExpired removed %d, want 0", removed)
		}
	})
}
