package auth

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestKEKSessionCache_SetAndGet(t *testing.T) {
	cache := NewKEKSessionCache(5 * time.Minute)
	kek := []byte("0123456789abcdef0123456789abcdef") // 32 bytes

	cache.Set("ses_1", kek)

	got := cache.Get("ses_1")
	if got == nil {
		t.Fatal("expected non-nil KEK")
	}
	if !bytes.Equal(got, kek) {
		t.Fatal("retrieved KEK does not match")
	}
}

func TestKEKSessionCache_GetReturnsCopy(t *testing.T) {
	cache := NewKEKSessionCache(5 * time.Minute)
	kek := []byte("0123456789abcdef0123456789abcdef")

	cache.Set("ses_1", kek)

	got1 := cache.Get("ses_1")
	got2 := cache.Get("ses_1")

	// Mutating one should not affect the other.
	got1[0] = 0xFF
	if got2[0] == 0xFF {
		t.Fatal("Get should return independent copies")
	}
}

func TestKEKSessionCache_SetCopiesInput(t *testing.T) {
	cache := NewKEKSessionCache(5 * time.Minute)
	kek := []byte("0123456789abcdef0123456789abcdef")

	cache.Set("ses_1", kek)

	// Zero the caller's copy — should not affect the cached value.
	zeroBytes(kek)

	got := cache.Get("ses_1")
	if got == nil || got[0] == 0 {
		t.Fatal("Set should copy the KEK, not hold a reference")
	}
}

func TestKEKSessionCache_GetNotFound(t *testing.T) {
	cache := NewKEKSessionCache(5 * time.Minute)

	got := cache.Get("nonexistent")
	if got != nil {
		t.Fatal("expected nil for missing session")
	}
}

func TestKEKSessionCache_Expiry(t *testing.T) {
	now := time.Now()
	cache := NewKEKSessionCache(1 * time.Minute)
	cache.now = func() time.Time { return now }

	kek := []byte("0123456789abcdef0123456789abcdef")
	cache.Set("ses_1", kek)

	// Advance time past TTL.
	cache.now = func() time.Time { return now.Add(2 * time.Minute) }

	got := cache.Get("ses_1")
	if got != nil {
		t.Fatal("expected nil for expired session")
	}

	// Entry should be evicted.
	if cache.Len() != 0 {
		t.Fatalf("expected 0 entries after expiry, got %d", cache.Len())
	}
}

func TestKEKSessionCache_Clear(t *testing.T) {
	cache := NewKEKSessionCache(5 * time.Minute)
	kek := []byte("0123456789abcdef0123456789abcdef")

	cache.Set("ses_1", kek)
	cache.Clear("ses_1")

	got := cache.Get("ses_1")
	if got != nil {
		t.Fatal("expected nil after Clear")
	}
}

func TestKEKSessionCache_ClearZerosKEK(t *testing.T) {
	cache := NewKEKSessionCache(5 * time.Minute)
	kek := []byte("0123456789abcdef0123456789abcdef")

	cache.Set("ses_1", kek)

	// Get a reference to the internal entry before clearing.
	cache.mu.RLock()
	entry := cache.entries["ses_1"]
	internalKEK := entry.kek
	cache.mu.RUnlock()

	cache.Clear("ses_1")

	// The internal KEK bytes should be zeroed.
	for _, b := range internalKEK {
		if b != 0 {
			t.Fatal("Clear should zero KEK bytes")
		}
	}
}

func TestKEKSessionCache_EvictExpired(t *testing.T) {
	now := time.Now()
	cache := NewKEKSessionCache(1 * time.Minute)
	cache.now = func() time.Time { return now }

	cache.Set("ses_1", []byte("0123456789abcdef0123456789abcdef"))
	cache.Set("ses_2", []byte("fedcba9876543210fedcba9876543210"))

	// Advance time past TTL for ses_1 only by setting ses_2 with fresh time.
	cache.now = func() time.Time { return now.Add(30 * time.Second) }
	cache.Set("ses_2", []byte("fedcba9876543210fedcba9876543210")) // refresh ses_2

	// Advance past original TTL.
	cache.now = func() time.Time { return now.Add(90 * time.Second) }
	cache.EvictExpired()

	// ses_1 should be evicted, ses_2 should remain (set 30s ago, TTL 60s).
	if cache.Get("ses_1") != nil {
		t.Fatal("ses_1 should be evicted")
	}
	if cache.Get("ses_2") == nil {
		t.Fatal("ses_2 should still be valid")
	}
}

func TestKEKSessionCache_SetOverwrite(t *testing.T) {
	cache := NewKEKSessionCache(5 * time.Minute)

	kek1 := []byte("0123456789abcdef0123456789abcdef")
	kek2 := []byte("fedcba9876543210fedcba9876543210")

	cache.Set("ses_1", kek1)
	cache.Set("ses_1", kek2)

	got := cache.Get("ses_1")
	if !bytes.Equal(got, kek2) {
		t.Fatal("overwritten KEK should be the latest value")
	}
}

func TestKEKContextRoundTrip(t *testing.T) {
	kek := []byte("0123456789abcdef0123456789abcdef")
	ctx := ContextWithKEK(context.Background(), kek)

	got := KEKFromContext(ctx)
	if !bytes.Equal(got, kek) {
		t.Fatal("context round-trip failed")
	}
}

func TestKEKFromContext_Empty(t *testing.T) {
	got := KEKFromContext(context.Background())
	if got != nil {
		t.Fatal("expected nil from empty context")
	}
}

func TestKEKSessionCache_ExpiresAt(t *testing.T) {
	now := time.Now()
	cache := NewKEKSessionCache(5 * time.Minute)
	cache.now = func() time.Time { return now }

	kek := []byte("0123456789abcdef0123456789abcdef")
	cache.Set("ses_1", kek)

	// Should return the expected expiry.
	expiresAt := cache.ExpiresAt("ses_1")
	if expiresAt == nil {
		t.Fatal("expected non-nil expiry")
	}
	expected := now.Add(5 * time.Minute)
	if !expiresAt.Equal(expected) {
		t.Fatalf("expiry = %v, want %v", *expiresAt, expected)
	}
}

func TestKEKSessionCache_ExpiresAt_NotFound(t *testing.T) {
	cache := NewKEKSessionCache(5 * time.Minute)
	if cache.ExpiresAt("nonexistent") != nil {
		t.Fatal("expected nil for missing session")
	}
}

func TestKEKSessionCache_ExpiresAt_Expired(t *testing.T) {
	now := time.Now()
	cache := NewKEKSessionCache(1 * time.Minute)
	cache.now = func() time.Time { return now }

	cache.Set("ses_1", []byte("0123456789abcdef0123456789abcdef"))

	// Advance past TTL.
	cache.now = func() time.Time { return now.Add(2 * time.Minute) }

	if cache.ExpiresAt("ses_1") != nil {
		t.Fatal("expected nil for expired session")
	}
	// Should be evicted.
	if cache.Len() != 0 {
		t.Fatalf("expected 0 entries after expired ExpiresAt, got %d", cache.Len())
	}
}

func TestKEKSessionCache_TTL(t *testing.T) {
	cache := NewKEKSessionCache(42 * time.Minute)
	if cache.TTL() != 42*time.Minute {
		t.Fatalf("TTL = %v, want 42m", cache.TTL())
	}
}
