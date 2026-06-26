package binding

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/vault"
)

// VaultStore is the reference Store implementation. The vault is the
// binding store: the binding name is the vault path, the credential
// bytes are the encrypted vault Value, and the binding metadata
// (connector FQN, scope, timestamps, status) is encoded into
// vault.Metadata.Labels.
type VaultStore struct {
	// Vault is the underlying credential vault. Listing and metadata
	// reads do not require it to be unlocked; Put and Resolve do
	// because they read/write the encrypted credential bytes.
	Vault vault.Vault

	// Now is the time source used for created_at / last_used_at /
	// last_refreshed_at stamps. Defaults to time.Now when nil so
	// production callers don't need to wire it; tests inject a clock.
	Now func() time.Time
}

// now returns the current time using the injected clock or wall time.
func (s *VaultStore) now() time.Time {
	if s == nil || s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// List implements [Store].
func (s *VaultStore) List(ctx context.Context) ([]Binding, error) {
	if s == nil || s.Vault == nil {
		return nil, fmt.Errorf("binding: vault is not configured")
	}
	entries, err := s.Vault.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("binding: list vault: %w", err)
	}
	out := make([]Binding, 0, len(entries))
	for _, e := range entries {
		// Skip non-binding entries (e.g. vault verification blobs).
		if !IsBindingPath(e.Path) {
			continue
		}
		b, err := fromEntry(e)
		if err != nil {
			// Skip malformed entries silently — listing must succeed
			// even when the vault contains keys this package doesn't
			// recognise. Higher layers can surface a warning if needed.
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// Get implements [Store].
func (s *VaultStore) Get(ctx context.Context, name Name) (Binding, error) {
	if s == nil || s.Vault == nil {
		return Binding{}, fmt.Errorf("binding: vault is not configured")
	}
	// Listing is the right primitive here: it returns plaintext
	// metadata without unlocking the vault. We filter to the named
	// entry to preserve the no-decrypt-to-inspect property.
	entries, err := s.Vault.List(ctx)
	if err != nil {
		return Binding{}, fmt.Errorf("binding: list vault: %w", err)
	}
	for _, e := range entries {
		if e.Path != string(name) {
			continue
		}
		return fromEntry(e)
	}
	return Binding{}, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// Put implements [Store]. Encrypts and stores the credential bytes,
// stamps created_at, and writes the binding metadata.
func (s *VaultStore) Put(ctx context.Context, b Binding, value []byte, mode PutMode) error {
	if s == nil || s.Vault == nil {
		return fmt.Errorf("binding: vault is not configured")
	}
	if b.Name == "" {
		return fmt.Errorf("binding: name is required")
	}
	if _, _, _, _, err := Parse(string(b.Name)); err != nil {
		return err
	}
	if mode == PutCreate {
		if _, err := s.Get(ctx, b.Name); err == nil {
			return fmt.Errorf("%w: %s", ErrAlreadyExists, b.Name)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = s.now()
	}
	if b.Status == "" {
		b.Status = StatusActive
	}
	if err := s.Vault.Put(ctx, string(b.Name), value, b.toMetadata()); err != nil {
		return fmt.Errorf("binding: vault put: %w", err)
	}
	return nil
}

// UpdateMetadata implements [Store]. Reads the existing entry's
// secret bytes from the vault, then re-puts with the same bytes plus
// the binding's current metadata fields. The round-trip requires an
// unlocked vault when the underlying vault encrypts at rest (the
// EncryptedVault decorator) — callers that need lock-tolerant updates
// must adapt the underlying vault, not this method.
func (s *VaultStore) UpdateMetadata(ctx context.Context, b Binding) error {
	if s == nil || s.Vault == nil {
		return fmt.Errorf("binding: vault is not configured")
	}
	if b.Name == "" {
		return fmt.Errorf("binding: name is required")
	}
	if _, _, _, _, err := Parse(string(b.Name)); err != nil {
		return err
	}
	// Confirm existence with a metadata-only List read so the error
	// shape is the stable ErrNotFound rather than a vault-specific
	// surprise.
	if _, err := s.Get(ctx, b.Name); err != nil {
		return err
	}
	secret, err := s.Vault.Get(ctx, string(b.Name))
	if err != nil {
		return fmt.Errorf("binding: read vault entry %q: %w", b.Name, err)
	}
	if err := s.Vault.Put(ctx, string(b.Name), secret.Value, b.toMetadata()); err != nil {
		return fmt.Errorf("binding: vault put: %w", err)
	}
	return nil
}

// Delete implements [Store].
func (s *VaultStore) Delete(ctx context.Context, name Name) error {
	if s == nil || s.Vault == nil {
		return fmt.Errorf("binding: vault is not configured")
	}
	// Confirm the binding exists before deleting so callers get a
	// stable ErrNotFound (the underlying vault's Delete error shape
	// varies across implementations).
	if _, err := s.Get(ctx, name); err != nil {
		return err
	}
	if err := s.Vault.Delete(ctx, string(name)); err != nil {
		return fmt.Errorf("binding: vault delete: %w", err)
	}
	return nil
}

// ResolverFor implements [Store]. Returns nil when no usable binding
// exists (no match or ambiguous) — the sandbox surfaces
// `binding_required` on the first credential reference. Branches by
// kind to return the right resolver:
//
//   - api_key: [credential.VaultResolver] reads the secret bytes
//     verbatim and validates the entry's metadata kind.
//   - oauth2: [credential.OAuth2VaultResolver] parses the JSON token
//     envelope, refreshes transparently when the access token nears
//     expiry, persists the new envelope, and returns the access
//     token as the credential bytes.
//
// Unknown kinds return nil — the host then surfaces
// `binding_required` even though a binding metadata entry exists,
// which is the correct fail-closed behavior for a kind the runtime
// doesn't know how to use.
func (s *VaultStore) ResolverFor(ctx context.Context, connectorFQN, kind string) credential.Resolver {
	if s == nil || s.Vault == nil {
		return nil
	}
	switch kind {
	case "api_key":
		b, err := s.Resolve(ctx, connectorFQN, kind)
		if err != nil {
			return nil
		}
		return &credential.VaultResolver{
			Vault:        s.Vault,
			VaultPath:    string(b.Name),
			ExpectedKind: kind,
		}
	case "oauth2":
		b, err := s.Resolve(ctx, connectorFQN, kind)
		if err != nil {
			return nil
		}
		return &credential.OAuth2VaultResolver{
			Vault:     s.Vault,
			VaultPath: string(b.Name),
		}
	case kindAWSSigV4:
		// aws_sigv4 deliberately does NOT gate on the single-match Resolve:
		// a connector install may hold several region-scoped bindings, which
		// Resolve reports as *AmbiguousError. The region-aware resolver below
		// picks the right one at request time. Only the genuine no-binding
		// case (zero matches) must surface as binding_required, so wire the
		// resolver whenever at least one aws_sigv4 binding exists.
		if !s.hasBinding(ctx, connectorFQN, kindAWSSigV4) {
			return nil
		}
		return &awsSigV4Resolver{store: s, connectorFQN: connectorFQN}
	}
	return nil
}

// Compile-time assertion: awsSigV4Resolver satisfies the regional resolver.
var _ credential.RegionalResolver = (*awsSigV4Resolver)(nil)

// kindAWSSigV4 mirrors cstore.CredentialKindAWSSigV4. It is duplicated as
// a local constant so the binding package does not import cstore (the
// other kinds are likewise referenced as bare strings here).
const kindAWSSigV4 = "aws_sigv4"

// hasBinding reports whether at least one binding exists for the given
// (connectorFQN, kind). Used by ResolverFor to decide whether to wire a
// resolver for kinds whose Resolve may legitimately be ambiguous.
func (s *VaultStore) hasBinding(ctx context.Context, connectorFQN, kind string) bool {
	all, err := s.List(ctx)
	if err != nil {
		return false
	}
	for _, b := range all {
		if b.ConnectorFQN == connectorFQN && b.Kind == kind {
			return true
		}
	}
	return false
}

// resolveSigV4 selects the aws_sigv4 binding for connectorFQN, keyed by
// (connectorFQN, kind, region). With a single binding it returns that one
// regardless of region (the single-binding fallback). With several it
// requires region to disambiguate and returns *AmbiguousError when region
// is empty or matches none. Returns ErrNotFound when no binding exists.
func (s *VaultStore) resolveSigV4(ctx context.Context, connectorFQN, region string) (Binding, error) {
	all, err := s.List(ctx)
	if err != nil {
		return Binding{}, err
	}
	var matches []Binding
	for _, b := range all {
		if b.ConnectorFQN == connectorFQN && b.Kind == kindAWSSigV4 {
			matches = append(matches, b)
		}
	}
	switch len(matches) {
	case 0:
		return Binding{}, fmt.Errorf("%w: connector=%s kind=%s", ErrNotFound, connectorFQN, kindAWSSigV4)
	case 1:
		return matches[0], nil
	default:
		if region != "" {
			for _, b := range matches {
				if b.Region == region {
					return b, nil
				}
			}
		}
		names := make([]Name, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return Binding{}, &AmbiguousError{
			ConnectorFQN: connectorFQN,
			Kind:         kindAWSSigV4,
			Candidates:   names,
		}
	}
}

// awsSigV4Resolver is the [credential.RegionalResolver] for aws_sigv4
// bindings. It defers binding selection until Resolve time so the host can
// supply the request's target region. The secret access key is read from
// the vault only for the selected binding; the non-secret region and
// access key id ride along on the returned [credential.Credential] so the
// host's signer can prefer them over the connector manifest's values.
type awsSigV4Resolver struct {
	store        *VaultStore
	connectorFQN string
}

// Resolve implements [credential.Resolver]. With no region hint it
// succeeds only when a single binding exists (the fallback); ambiguity
// surfaces as an error rather than a silent pick.
func (r *awsSigV4Resolver) Resolve(ctx context.Context) (credential.Credential, error) {
	return r.resolve(ctx, "")
}

// ResolveForRegion implements [credential.RegionalResolver].
func (r *awsSigV4Resolver) ResolveForRegion(ctx context.Context, region string) (credential.Credential, error) {
	return r.resolve(ctx, region)
}

func (r *awsSigV4Resolver) resolve(ctx context.Context, region string) (credential.Credential, error) {
	if r == nil || r.store == nil || r.store.Vault == nil {
		return credential.Credential{}, credential.ErrNoBindingResolver
	}
	b, err := r.store.resolveSigV4(ctx, r.connectorFQN, region)
	if err != nil {
		// ErrNotFound maps to binding_required at the host; AmbiguousError
		// propagates verbatim so the host surfaces the candidate names.
		if errors.Is(err, ErrNotFound) {
			return credential.Credential{}, credential.FormatBindingMissing(
				kindAWSSigV4 + " binding for " + r.connectorFQN)
		}
		return credential.Credential{}, err
	}
	secret, err := r.store.Vault.Get(ctx, string(b.Name))
	if err != nil {
		if vault.IsNotFound(err) {
			return credential.Credential{}, credential.FormatBindingMissing(string(b.Name))
		}
		return credential.Credential{}, fmt.Errorf("binding: read vault entry %q: %w", b.Name, err)
	}
	if secret.Metadata.Type != kindAWSSigV4 {
		return credential.Credential{}, credential.FormatKindMismatch(kindAWSSigV4, secret.Metadata.Type)
	}
	return credential.Credential{
		Kind:        kindAWSSigV4,
		Value:       secret.Value,
		Region:      b.Region,
		AccessKeyID: b.AccessKeyID,
	}, nil
}

// Resolve implements [Store]. Walks the vault listing for entries
// whose connector_fqn label matches the requested FQN and whose kind
// matches the requested kind. Returns ErrNotFound on no match and
// *AmbiguousError on more than one.
func (s *VaultStore) Resolve(ctx context.Context, connectorFQN, kind string) (Binding, error) {
	if s == nil || s.Vault == nil {
		return Binding{}, fmt.Errorf("binding: vault is not configured")
	}
	all, err := s.List(ctx)
	if err != nil {
		return Binding{}, err
	}
	var matches []Binding
	for _, b := range all {
		if b.ConnectorFQN == connectorFQN && b.Kind == kind {
			matches = append(matches, b)
		}
	}
	switch len(matches) {
	case 0:
		return Binding{}, fmt.Errorf("%w: connector=%s kind=%s", ErrNotFound, connectorFQN, kind)
	case 1:
		return matches[0], nil
	default:
		names := make([]Name, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return Binding{}, &AmbiguousError{
			ConnectorFQN: connectorFQN,
			Kind:         kind,
			Candidates:   names,
		}
	}
}
