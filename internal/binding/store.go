package binding

import (
	"context"
	"errors"
	"strings"

	"github.com/ALRubinger/aileron/internal/credential"
)

// Sentinel errors callers can compare with errors.Is.
var (
	// ErrNotFound is returned by Get and Resolve when no binding
	// matches the request.
	ErrNotFound = errors.New("binding: not found")

	// ErrAmbiguous is returned by Resolve when more than one binding
	// matches the requested (connector_fqn, kind) tuple. Per ADR-0006
	// the runtime never picks silently — the caller must surface the
	// candidate names to the user (or the failure envelope) so the
	// user can rebind explicitly.
	ErrAmbiguous = errors.New("binding: ambiguous")

	// ErrAlreadyExists is returned by Put when a binding with the
	// same name already exists and the caller asked for create-only.
	ErrAlreadyExists = errors.New("binding: already exists")
)

// AmbiguousError carries the candidate binding names so callers can
// surface them to the user. Wraps ErrAmbiguous.
type AmbiguousError struct {
	ConnectorFQN string
	Kind         string
	Candidates   []Name
}

// Error implements error.
func (e *AmbiguousError) Error() string {
	names := make([]string, 0, len(e.Candidates))
	for _, n := range e.Candidates {
		names = append(names, n.String())
	}
	return "binding: ambiguous resolution for connector=" + e.ConnectorFQN +
		" kind=" + e.Kind + ": candidates=[" + strings.Join(names, ", ") + "]"
}

// Unwrap reports ErrAmbiguous so errors.Is(err, ErrAmbiguous) succeeds.
func (e *AmbiguousError) Unwrap() error { return ErrAmbiguous }

// PutMode controls whether Put may overwrite an existing binding.
type PutMode int

const (
	// PutCreate fails with ErrAlreadyExists when the binding already
	// exists. Used by setup to avoid clobbering work.
	PutCreate PutMode = iota
	// PutUpsert overwrites an existing binding's value and metadata.
	// Used by rebind.
	PutUpsert
)

// Store is the binding lifecycle SPI. Implementations layer over
// vault.Vault and translate per-operation semantics — listing reads
// plaintext metadata, Resolve walks the vault for matching
// (connector, kind) entries, Put writes the credential value
// encrypted under the user's KEK.
type Store interface {
	// List returns every binding the user has created. Does not
	// require an unlocked vault. Order is unspecified.
	List(ctx context.Context) ([]Binding, error)

	// Get returns a single binding by name. Does not require an
	// unlocked vault. Returns ErrNotFound when the binding does not
	// exist.
	Get(ctx context.Context, name Name) (Binding, error)

	// Put creates or replaces a binding. Requires an unlocked vault
	// because the credential bytes are encrypted before storage. The
	// mode argument controls whether an existing binding is allowed
	// to be overwritten.
	Put(ctx context.Context, b Binding, value []byte, mode PutMode) error

	// Delete removes the binding's vault entry. Returns ErrNotFound
	// when the binding does not exist.
	Delete(ctx context.Context, name Name) error

	// UpdateMetadata replaces the binding's metadata labels in place,
	// preserving the credential bytes. Used by scope-drift detection to
	// stamp Status / StaleReason / GrantedScopes / MissingScopes after
	// an install reveals an existing binding no longer satisfies the
	// connector's manifest. Returns ErrNotFound when the binding does
	// not exist. The implementation may need the vault to be unlocked
	// (round-tripping the encrypted value) and propagates whatever
	// lock-state error the underlying vault returns.
	UpdateMetadata(ctx context.Context, b Binding) error

	// Resolve returns the single binding matching the given
	// (connector FQN, kind) tuple. Returns ErrNotFound when none
	// match and *AmbiguousError (wrapping ErrAmbiguous) when more
	// than one matches.
	Resolve(ctx context.Context, connectorFQN, kind string) (Binding, error)

	// ResolverFor returns a credential.Resolver wired to the bound
	// vault entry for the given (connector FQN, kind) tuple, or nil
	// when no usable binding exists. The sandbox surfaces
	// `binding_required` on the first credential reference when this
	// returns nil. The returned resolver is single-use semantically:
	// the sandbox per-invocation host state holds the reference for
	// the duration of one Invoke and drops it on completion.
	ResolverFor(ctx context.Context, connectorFQN, kind string) credential.Resolver
}
