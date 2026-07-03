// Package freeze is the author-time step that seals an installed Aileron
// skill into a Flight Plan (ADR-0027). Freeze resolves every image
// reference to a content-addressed `@sha256:` digest, emits a lockfile,
// content-addresses the unit, produces a detached ed25519 signature, and
// runs the no-LLM lint. The result is written as an immutable version into
// the canonical skill store (internal/flightplan/store).
//
// Freeze is deterministic and runtime-free at its core: digest resolution
// is a seam (DigestResolver / FeatureComposer) so the lockfile,
// content-addressing, lint, and signing logic are unit-testable without a
// container runtime. The CLI (cmd/aileron) wires the concrete container
// runtime resolvers.
//
// Freeze never carries a credential value. The signing private key is read
// from disk or the environment for signing only and is never copied into
// the frozen artifact; only the detached signature and the author public
// key are persisted (ADR-0005, ADR-0019).
package freeze

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
)

// Result is the outcome of a freeze. It carries the frozen artifacts the
// store writer persists and the identifying contentHash + version.
type Result struct {
	// Name is the skill name from the manifest frontmatter.
	Name string
	// Version is the human-facing semver label for this frozen version.
	Version string
	// ContentHash is the `sha256:<hex>` content hash identifying the exact
	// frozen manifest bytes. It is also the store version id source.
	ContentHash string
	// FrozenManifest is the SKILL.md with the populated `lock` block
	// injected (contentHash included). This is the durable, signed artifact.
	FrozenManifest []byte
	// Lockfile is the standalone `aileron.lock` artifact bytes (contentHash
	// included), the durable pin record.
	Lockfile []byte
	// Signature is the detached ed25519 signature over the canonical
	// content-hash bytes.
	Signature []byte
	// PublicKey is the PEM-encoded author public key the signature verifies
	// against. Stored alongside the signature; the private key never is.
	PublicKey []byte
	// Lock is the structured lockfile record (contentHash included).
	Lock Lockfile
}

// Options configures a Run.
type Options struct {
	// Version is the semver label recorded in the lock. Optional; empty
	// records no version label.
	Version string
	// CLIVersion is the Aileron CLI build version. It drives the default
	// composition base: a tools-declaring skill with no custom image composes
	// onto the Aileron runner base for this version (composition.BaseImage).
	// It is distinct from Version (the skill's semver label): CLIVersion
	// picks the floating sandbox-base tag the default resolves from. An empty
	// value behaves as a dev build (resolves the :edge tag), matching the
	// CLI's version default.
	CLIVersion string
	// SigningKeyPath is the path to the PEM ed25519 private key. Empty falls
	// back to $AILERON_SIGNING_KEY (see LoadSigningKey).
	SigningKeyPath string
	// Resolver resolves a base image reference (the runner base or an
	// environment custom image) to a digest. Required for any
	// environment-declaring skill; may be nil for instruction-only /
	// no-environment skills.
	Resolver DigestResolver
	// Composer composes catalog-resolved Feature references onto the
	// digest-pinned base and pins the built image's digest. May be nil when
	// no tools-declaring skill is frozen.
	Composer FeatureComposer
}

// Run executes the freeze pipeline over a SKILL.md document. It sequences:
// parse → lint → resolve digests → build lockfile → contentHash → sign.
// It does NOT write to the store; the caller persists Result through the
// store's frozen-version writer so the store stays the single writer.
//
// Lint runs before any image resolution so an unmarked-LLM skill fails fast
// without touching a container runtime.
func Run(ctx context.Context, raw []byte, opts Options) (Result, error) {
	m, err := manifest.Parse(raw)
	if err != nil {
		return Result{}, fmt.Errorf("freeze: parse skill: %w", err)
	}
	if m.Name == "" {
		return Result{}, fmt.Errorf("freeze: skill has no name in its frontmatter")
	}

	if err := Lint(m); err != nil {
		return Result{}, err
	}

	// Load the signing key before doing expensive resolution so a missing
	// or malformed key fails fast.
	priv, err := LoadSigningKey(opts.SigningKeyPath)
	if err != nil {
		return Result{}, err
	}

	pins, capSet, err := resolveImages(ctx, m, opts.Resolver, opts.Composer, opts.CLIVersion)
	if err != nil {
		return Result{}, err
	}

	// Seal each contracted tool step's declared reach into the step-keyed
	// lock section, so the content hash and signature cover it.
	var stepTrust map[string]StepReach
	if !m.InstructionOnly {
		stepTrust, err = sealStepTrust(m.Aileron.Steps)
		if err != nil {
			return Result{}, err
		}
	}

	lock := Lockfile{
		ResolvedImages:        pins,
		ResolvedCapabilitySet: capSet,
		StepTrust:             stepTrust,
		Version:               opts.Version,
	}

	// Compute the content hash over the canonical bytes, with the
	// self-referential contentHash field excluded from the hashed input.
	lockNoHash := lock.withoutContentHash()
	manifestNoHash, err := injectLockMaybe(raw, lockNoHash, m.InstructionOnly)
	if err != nil {
		return Result{}, err
	}
	lockfileNoHash, err := MarshalLockfile(lockNoHash)
	if err != nil {
		return Result{}, err
	}
	hash := contentHash(manifestNoHash, lockfileNoHash)
	lock.ContentHash = hash

	// Re-emit the final artifacts WITH the contentHash present.
	frozenManifest, err := injectLockMaybe(raw, lock, m.InstructionOnly)
	if err != nil {
		return Result{}, err
	}
	lockfile, err := MarshalLockfile(lock)
	if err != nil {
		return Result{}, err
	}

	// Sign the canonical content bytes (the same bytes the hash is over) so
	// the signature covers the exact frozen unit and a verifier needs only
	// the public key + signature.
	sig, pubPEM, err := Sign(priv, canonicalContent(manifestNoHash, lockfileNoHash))
	if err != nil {
		return Result{}, err
	}

	return Result{
		Name:           m.Name,
		Version:        opts.Version,
		ContentHash:    hash,
		FrozenManifest: frozenManifest,
		Lockfile:       lockfile,
		Signature:      sig,
		PublicKey:      pubPEM,
		Lock:           lock,
	}, nil
}
