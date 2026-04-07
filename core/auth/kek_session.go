package auth

import (
	"context"
	"sync"
	"time"
)

type kekContextKey struct{}

// KEKFromContext returns the KEK stored in the context by the KEK session
// middleware, or nil if no KEK session is active.
func KEKFromContext(ctx context.Context) []byte {
	kek, _ := ctx.Value(kekContextKey{}).([]byte)
	return kek
}

// ContextWithKEK returns a new context with the KEK attached.
func ContextWithKEK(ctx context.Context, kek []byte) context.Context {
	return context.WithValue(ctx, kekContextKey{}, kek)
}

// KEKSessionCache holds per-session KEKs in memory with a TTL.
// When a user verifies their passphrase, the derived KEK is stored here
// so that subsequent requests in the same session can decrypt vault secrets
// without re-prompting for the passphrase.
//
// The cache zeros KEK bytes on eviction to minimize the window during which
// plaintext key material exists in process memory.
type KEKSessionCache struct {
	mu      sync.RWMutex
	entries map[string]*kekEntry
	ttl     time.Duration
	now     func() time.Time // injectable for testing
}

type kekEntry struct {
	kek       []byte
	expiresAt time.Time
}

// NewKEKSessionCache creates a new cache with the given default TTL.
func NewKEKSessionCache(ttl time.Duration) *KEKSessionCache {
	return &KEKSessionCache{
		entries: make(map[string]*kekEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// Set stores a KEK for the given session ID. The KEK bytes are copied
// so the caller can safely zero their copy.
func (c *KEKSessionCache) Set(sessionID string, kek []byte) {
	copied := make([]byte, len(kek))
	copy(copied, kek)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Zero any existing entry for this session.
	if old, ok := c.entries[sessionID]; ok {
		zeroBytes(old.kek)
	}

	c.entries[sessionID] = &kekEntry{
		kek:       copied,
		expiresAt: c.now().Add(c.ttl),
	}
}

// Get returns a copy of the KEK for the given session, or nil if not found
// or expired. Expired entries are evicted on access.
func (c *KEKSessionCache) Get(sessionID string) []byte {
	c.mu.RLock()
	entry, ok := c.entries[sessionID]
	c.mu.RUnlock()

	if !ok {
		return nil
	}

	if c.now().After(entry.expiresAt) {
		c.Clear(sessionID)
		return nil
	}

	// Return a copy so the caller doesn't hold a reference to our internal slice.
	copied := make([]byte, len(entry.kek))
	copy(copied, entry.kek)
	return copied
}

// Clear removes and zeros the KEK for the given session.
func (c *KEKSessionCache) Clear(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries[sessionID]; ok {
		zeroBytes(entry.kek)
		delete(c.entries, sessionID)
	}
}

// EvictExpired removes all expired entries, zeroing their KEK bytes.
// Call this periodically from a background goroutine.
func (c *KEKSessionCache) EvictExpired() {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	for id, entry := range c.entries {
		if now.After(entry.expiresAt) {
			zeroBytes(entry.kek)
			delete(c.entries, id)
		}
	}
}

// ExpiresAt returns the expiry time for the given session, or nil if the
// session is not found or has expired. Unlike Get, it does not return the
// KEK bytes — safe for status checks.
func (c *KEKSessionCache) ExpiresAt(sessionID string) *time.Time {
	c.mu.RLock()
	entry, ok := c.entries[sessionID]
	c.mu.RUnlock()

	if !ok {
		return nil
	}

	if c.now().After(entry.expiresAt) {
		c.Clear(sessionID)
		return nil
	}

	t := entry.expiresAt
	return &t
}

// TTL returns the configured session TTL.
func (c *KEKSessionCache) TTL() time.Duration {
	return c.ttl
}

// Len returns the number of entries in the cache (for testing).
func (c *KEKSessionCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
