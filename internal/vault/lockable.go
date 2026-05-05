package vault

import (
	"context"
	"sync"
)

// LockableVault wraps a [Vault] and lets the daemon construct its
// dependency graph before the underlying vault has been unlocked.
//
// When the inner vault is nil, mutating operations (Get / Put /
// Delete) return [ErrCredentialUnavailable] — the canonical
// "credential cannot be retrieved, prompt the user to unlock" signal
// already plumbed through the binding-resolution path. List returns
// an empty slice with no error, mirroring the property documented on
// [Vault.List]: listing must succeed without decryption, and a locked
// vault legitimately has nothing to enumerate yet.
//
// Per ADR-0011 the daemon may run with the vault locked: Aileron
// accepts requests, vault-needing endpoints return 423 Locked, and
// the user unlocks via the webapp modal (#429). LockableVault is the
// piece that makes the apiServer's static dependency graph (binding
// store, executor, source registries) work in that state — every
// component holds the same [Vault] reference; on unlock, [Set] swaps
// the inner vault in atomically and operations resume.
//
// The receiver is safe for concurrent use.
type LockableVault struct {
	mu    sync.RWMutex
	inner Vault
}

// NewLockableVault returns a LockableVault with no inner vault. Until
// [Set] is called, all credential operations return
// [ErrCredentialUnavailable]; List returns an empty slice.
func NewLockableVault() *LockableVault {
	return &LockableVault{}
}

// Set installs the inner vault, transitioning the wrapper from
// "locked" to "unlocked." Subsequent calls overwrite the previous
// inner — used in tests; in production the daemon sets the inner
// exactly once per session.
func (l *LockableVault) Set(v Vault) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inner = v
}

// Inner returns the current inner vault, or nil if not yet unlocked.
// Exposed so the apiServer can determine "is the vault unlocked"
// without imitating the LockableVault contract elsewhere.
func (l *LockableVault) Inner() Vault {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.inner
}

// Get implements [Vault]. Returns [ErrCredentialUnavailable] when no
// inner vault is set.
func (l *LockableVault) Get(ctx context.Context, path string) (Secret, error) {
	v := l.Inner()
	if v == nil {
		return Secret{}, ErrCredentialUnavailable
	}
	return v.Get(ctx, path)
}

// Put implements [Vault]. Returns [ErrCredentialUnavailable] when no
// inner vault is set.
func (l *LockableVault) Put(ctx context.Context, path string, value []byte, meta Metadata) error {
	v := l.Inner()
	if v == nil {
		return ErrCredentialUnavailable
	}
	return v.Put(ctx, path, value, meta)
}

// Delete implements [Vault]. Returns [ErrCredentialUnavailable] when
// no inner vault is set.
func (l *LockableVault) Delete(ctx context.Context, path string) error {
	v := l.Inner()
	if v == nil {
		return ErrCredentialUnavailable
	}
	return v.Delete(ctx, path)
}

// List implements [Vault]. Returns an empty slice when no inner vault
// is set — listing is documented (per [Vault.List]) to succeed without
// decryption, and a locked wrapper has nothing to enumerate.
func (l *LockableVault) List(ctx context.Context) ([]Entry, error) {
	v := l.Inner()
	if v == nil {
		return nil, nil
	}
	return v.List(ctx)
}
