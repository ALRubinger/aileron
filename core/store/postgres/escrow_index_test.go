//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/store/pgtest"
	"github.com/ALRubinger/aileron/core/store/postgres"
)

func newEscrowStore(t *testing.T) *postgres.EscrowIndexStore {
	t.Helper()
	db := pgtest.New(t, pgtest.EscrowIndexSchema)
	return postgres.NewEscrowIndexStore(db)
}

func TestEscrowIndexUpsert(t *testing.T) {
	store := newEscrowStore(t)
	ctx := context.Background()

	err := store.Upsert(ctx, "vault/path/1", "esc_001", "usr_1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Verify the entry was created.
	entries, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if got, want := entries["vault/path/1"], "esc_001"; got != want {
		t.Errorf("LoadAll[vault/path/1] = %q, want %q", got, want)
	}
}

func TestEscrowIndexUpsertOverwrite(t *testing.T) {
	store := newEscrowStore(t)
	ctx := context.Background()

	// Insert initial entry.
	if err := store.Upsert(ctx, "vault/path/1", "esc_001", "usr_1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// Upsert with new escrow ID — should overwrite.
	if err := store.Upsert(ctx, "vault/path/1", "esc_002", "usr_1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	entries, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if got, want := entries["vault/path/1"], "esc_002"; got != want {
		t.Errorf("after overwrite: LoadAll[vault/path/1] = %q, want %q", got, want)
	}
	if got := len(entries); got != 1 {
		t.Errorf("expected 1 entry after upsert, got %d", got)
	}
}

func TestEscrowIndexDelete(t *testing.T) {
	store := newEscrowStore(t)
	ctx := context.Background()

	if err := store.Upsert(ctx, "vault/path/1", "esc_001", "usr_1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := store.Delete(ctx, "vault/path/1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	entries, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after Delete, got %d", len(entries))
	}
}

func TestEscrowIndexDeleteNonexistent(t *testing.T) {
	store := newEscrowStore(t)
	ctx := context.Background()

	// Deleting a path that doesn't exist should not error.
	if err := store.Delete(ctx, "vault/does-not-exist"); err != nil {
		t.Fatalf("Delete non-existent: %v", err)
	}
}

func TestEscrowIndexLoadAllFiltersExpired(t *testing.T) {
	store := newEscrowStore(t)
	ctx := context.Background()

	// Insert one expired and one valid entry.
	if err := store.Upsert(ctx, "vault/expired", "esc_old", "usr_1", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("Upsert expired: %v", err)
	}
	if err := store.Upsert(ctx, "vault/valid", "esc_new", "usr_1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Upsert valid: %v", err)
	}

	entries, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	if _, ok := entries["vault/expired"]; ok {
		t.Error("LoadAll should not return expired entries")
	}
	if got, want := entries["vault/valid"], "esc_new"; got != want {
		t.Errorf("LoadAll[vault/valid] = %q, want %q", got, want)
	}
}

func TestEscrowIndexDeleteExpired(t *testing.T) {
	store := newEscrowStore(t)
	ctx := context.Background()

	// Insert two expired entries and one valid entry.
	for _, e := range []struct {
		path, id string
		ttl      time.Duration
	}{
		{"vault/exp1", "esc_1", -time.Hour},
		{"vault/exp2", "esc_2", -2 * time.Hour},
		{"vault/valid", "esc_3", time.Hour},
	} {
		if err := store.Upsert(ctx, e.path, e.id, "usr_1", time.Now().Add(e.ttl)); err != nil {
			t.Fatalf("Upsert %s: %v", e.path, err)
		}
	}

	removed, err := store.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if removed != 2 {
		t.Errorf("DeleteExpired removed %d, want 2", removed)
	}

	// The valid entry should still exist.
	entries, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry after DeleteExpired, got %d", len(entries))
	}
	if _, ok := entries["vault/valid"]; !ok {
		t.Error("valid entry should survive DeleteExpired")
	}
}

func TestEscrowIndexLoadAllEmpty(t *testing.T) {
	store := newEscrowStore(t)
	ctx := context.Background()

	entries, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll on empty table: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries on empty table, got %d", len(entries))
	}
}

func TestEscrowIndexDeleteExpiredReturnsZeroWhenNoneExpired(t *testing.T) {
	store := newEscrowStore(t)
	ctx := context.Background()

	if err := store.Upsert(ctx, "vault/valid", "esc_1", "usr_1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	removed, err := store.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if removed != 0 {
		t.Errorf("DeleteExpired removed %d, want 0", removed)
	}
}
