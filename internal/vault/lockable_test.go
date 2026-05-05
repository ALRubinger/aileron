package vault_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

// LockableVault contract:
//
//   - Constructed empty: Get/Put/Delete return ErrCredentialUnavailable.
//   - Constructed empty: List returns an empty slice with no error
//     (consistent with Vault.List succeeding without decryption).
//   - After Set: operations forward to the inner vault.
//   - Inner reports the currently-set inner (or nil when empty).
//   - Set is concurrency-safe — two goroutines racing on Set / Get
//     converge on a single inner without panic or partial state.

func TestLockableVault_LockedReturnsCredentialUnavailable(t *testing.T) {
	lv := vault.NewLockableVault()

	if _, err := lv.Get(context.Background(), "foo"); !errors.Is(err, vault.ErrCredentialUnavailable) {
		t.Errorf("Get on locked: err = %v, want ErrCredentialUnavailable", err)
	}
	if err := lv.Put(context.Background(), "foo", []byte("v"), vault.Metadata{}); !errors.Is(err, vault.ErrCredentialUnavailable) {
		t.Errorf("Put on locked: err = %v, want ErrCredentialUnavailable", err)
	}
	if err := lv.Delete(context.Background(), "foo"); !errors.Is(err, vault.ErrCredentialUnavailable) {
		t.Errorf("Delete on locked: err = %v, want ErrCredentialUnavailable", err)
	}
}

func TestLockableVault_LockedListReturnsEmpty(t *testing.T) {
	lv := vault.NewLockableVault()

	entries, err := lv.List(context.Background())
	if err != nil {
		t.Fatalf("List on locked: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("List on locked: %d entries, want 0", len(entries))
	}
}

func TestLockableVault_AfterSetForwards(t *testing.T) {
	lv := vault.NewLockableVault()
	lv.Set(vault.NewMemVault())

	ctx := context.Background()
	if err := lv.Put(ctx, "foo", []byte("bar"), vault.Metadata{Type: "api_key"}); err != nil {
		t.Fatalf("Put after Set: %v", err)
	}
	got, err := lv.Get(ctx, "foo")
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if string(got.Value) != "bar" {
		t.Errorf("Get value = %q, want %q", got.Value, "bar")
	}
	entries, err := lv.List(ctx)
	if err != nil {
		t.Fatalf("List after Set: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "foo" {
		t.Errorf("List entries = %+v, want exactly [{Path:foo}]", entries)
	}
	if err := lv.Delete(ctx, "foo"); err != nil {
		t.Fatalf("Delete after Set: %v", err)
	}
}

func TestLockableVault_InnerReturnsCurrentInner(t *testing.T) {
	lv := vault.NewLockableVault()
	if lv.Inner() != nil {
		t.Error("Inner on empty: want nil")
	}
	mv := vault.NewMemVault()
	lv.Set(mv)
	if lv.Inner() != mv {
		t.Error("Inner after Set: want supplied MemVault")
	}
}

func TestLockableVault_ConcurrentSetGet(t *testing.T) {
	lv := vault.NewLockableVault()
	const goroutines = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			lv.Set(vault.NewMemVault())
			_, _ = lv.Get(context.Background(), "x")
			_, _ = lv.List(context.Background())
		}()
	}
	wg.Wait()

	// Final state should be consistent: inner is non-nil and
	// operations succeed. Any panics during the race are caught by
	// the test runtime.
	if lv.Inner() == nil {
		t.Fatal("Inner is nil after concurrent Sets")
	}
}
