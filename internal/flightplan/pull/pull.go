// Package pull implements the read half of cross-machine Flight Plan sharing
// (umbrella #1898): `aileron skill install <oci-ref>` resolves the signed
// artifact published by `skill publish` (#1901), pulls its four layers, and
// returns them as a store.FrozenVersion ready to write into the local store
// as a frozen version indistinguishable from a local freeze.
//
// pull is the read analogue of the publish package: it holds all oras/registry
// read code so cmd/aileron and internal/flightplan/store stay oras-free. It
// pulls ONLY the signed artifact (SKILL.md + aileron.lock + signature.sig +
// signing-key.pub), never the image subject. The image is fetched lazily at
// launch (#1903); the digest pin in the lock is carried into the store
// untouched.
//
// The artifact contract (artifactType, per-file media types) is imported from
// freeze.ociartifact so publish and pull never drift. Verification (signature +
// content hash, then publisher trust) is the SAME gate the runtime runs at
// launch, so a tampered artifact or an untrusted publisher fails closed before
// any bytes reach the store.
package pull

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"
	"github.com/ALRubinger/aileron/internal/flightplan/store"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
)

// Errors returned for the documented failure modes. Unreachable / anonymous /
// unauthenticated registry access surfaces as the wrapped oras Resolve/Fetch
// error, which the CLI maps to a precise message.
var (
	// ErrMissingVersionTag is returned when the install ref carries no tag (or
	// only a digest). The store version id must be a single path segment (the
	// freeze content-hash slug that publish tagged the artifact under), so an
	// untagged ref cannot yield a version id.
	ErrMissingVersionTag = errors.New("pull: OCI reference has no version tag")
	// ErrNotAnArtifact is returned when the resolved manifest is not a Flight
	// Plan signed artifact (its artifactType is not freeze.ArtifactType).
	ErrNotAnArtifact = errors.New("pull: reference does not resolve to a Flight Plan signed artifact")
	// ErrMissingArtifact is returned when the artifact manifest is missing one
	// of the four signed-artifact layers (by media type).
	ErrMissingArtifact = errors.New("pull: signed artifact is missing a required layer")
	// ErrNoName is returned when the pulled SKILL.md declares no name, so the
	// store has no directory to key the version under.
	ErrNoName = errors.New("pull: pulled SKILL.md declares no skill name")
)

// PublisherVerifier gates publisher trust at install exactly as the runtime
// gates it at launch (ADR-0013, #1900). The CLI passes its existing
// keyring-backed verifier (cmd/aileron/skill_launch_publisher.go) so install
// and launch resolve trust identically. VerifyPublisher returns a non-nil error
// when the declared publisher does not trust the signing key (fail-closed).
//
// A nil verifier, or a plan that declares no publisher, skips the gate (a
// publisher-less signed plan still installs, matching launch's gatePublisher);
// the signature is always still required.
type PublisherVerifier interface {
	VerifyPublisher(publisher string, signingKey ed25519.PublicKey) error
}

// Options configures a pull run.
type Options struct {
	// Ref is the OCI reference to install, e.g. "ghcr.io/acme/plan:v1abc". Its
	// tag is the frozen version id.
	Ref string
	// Source is the content store to resolve+fetch the artifact from. Nil builds
	// a remote.Repository from Ref over the operator's docker credentials. Tests
	// inject an in-memory store.
	Source oras.ReadOnlyTarget
	// Verifier gates publisher trust at install. Nil skips the trust gate (the
	// signature is still verified); the CLI wires the keyring-backed verifier.
	Verifier PublisherVerifier

	Stdout io.Writer
	Stderr io.Writer
}

// Result reports what a pull run resolved and verified.
type Result struct {
	// Frozen is the four artifact bytes as a store.FrozenVersion. Its ID is the
	// ref's tag (the freeze content-hash slug), so store.WriteFrozen lands it in
	// the same content-addressed directory a local freeze would.
	Frozen store.FrozenVersion
	// Lock is the parsed standalone lockfile (the image pin carried into the
	// store untouched, resolved lazily at launch).
	Lock freeze.Lockfile
	// Name is the skill name parsed from the pulled SKILL.md; the store keys the
	// version directory on it.
	Name string
	// Verified carries the signature-and-hash verification result (publisher,
	// signer identity). The trust gate has already passed by the time Run
	// returns.
	Verified freeze.VerifiedFrozen
}

// Run resolves the signed artifact at opts.Ref, pulls its four layers, verifies
// the signature + content hash and the publisher trust gate, and returns the
// artifact as a store.FrozenVersion. Verification runs BEFORE Run returns, so a
// caller that writes the result to the store never lands an unverified or
// untrusted artifact on disk.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}

	ref, err := registry.ParseReference(opts.Ref)
	if err != nil {
		return Result{}, fmt.Errorf("pull: parse reference %q: %w", opts.Ref, err)
	}
	tag := ref.Reference
	// A digest-only reference (@sha256:…) is not a version tag; a valid version
	// id must be a single path segment (the freeze slug publish tagged under).
	if tag == "" || isDigestReference(tag) {
		return Result{}, fmt.Errorf("%w: %q", ErrMissingVersionTag, opts.Ref)
	}

	src := opts.Source
	if src == nil {
		repo, err := ociremote.NewRepository(opts.Ref)
		if err != nil {
			return Result{}, fmt.Errorf("pull: source registry %q: %w", opts.Ref, err)
		}
		src = repo
	}

	fv, lock, err := fetchArtifact(ctx, src, tag)
	if err != nil {
		return Result{}, err
	}

	name, err := skillName(fv.SkillMD)
	if err != nil {
		return Result{}, err
	}

	// Verify signature + content hash, then the publisher-trust gate, BEFORE the
	// caller writes anything to the store. A tampered artifact fails VerifyFrozen;
	// an untrusted publisher fails the gate; either aborts the install.
	verified, err := freeze.VerifyFrozen(fv.SkillMD, fv.Lockfile, fv.Signature, fv.PublicKey)
	if err != nil {
		return Result{}, fmt.Errorf("pull: verify %q: %w", opts.Ref, err)
	}
	if opts.Verifier != nil && verified.Publisher != "" {
		if err := opts.Verifier.VerifyPublisher(verified.Publisher, verified.SignerKey); err != nil {
			return Result{}, err
		}
	}

	return Result{Frozen: fv, Lock: lock, Name: name, Verified: verified}, nil
}

// fetchArtifact resolves the referrers manifest at tag, asserts it is a Flight
// Plan signed artifact with all four layers, and pulls each layer by media type
// into a store.FrozenVersion (ID = tag). It does not touch the image subject.
func fetchArtifact(ctx context.Context, src oras.ReadOnlyTarget, tag string) (store.FrozenVersion, freeze.Lockfile, error) {
	desc, err := src.Resolve(ctx, tag)
	if err != nil {
		// Distinguish a genuinely absent referrer from a registry that is
		// unreachable or refuses the request. Only a not-found resolve means
		// "no published artifact at this tag" (ErrMissingArtifact); every other
		// failure (network, auth, TLS) is surfaced verbatim so the CLI reports
		// the real cause rather than a misleading "missing artifact". Both keep
		// the original error inspectable via errors.Unwrap.
		if errors.Is(err, errdef.ErrNotFound) {
			return store.FrozenVersion{}, freeze.Lockfile{}, fmt.Errorf("%w: resolve %q: %w", ErrMissingArtifact, tag, err)
		}
		return store.FrozenVersion{}, freeze.Lockfile{}, fmt.Errorf("pull: resolve artifact %q: %w", tag, err)
	}
	raw, err := content.FetchAll(ctx, src, desc)
	if err != nil {
		return store.FrozenVersion{}, freeze.Lockfile{}, fmt.Errorf("pull: fetch artifact manifest %q: %w", tag, err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return store.FrozenVersion{}, freeze.Lockfile{}, fmt.Errorf("pull: decode artifact manifest %q: %w", tag, err)
	}
	if m.ArtifactType != freeze.ArtifactType {
		return store.FrozenVersion{}, freeze.Lockfile{}, fmt.Errorf("%w: artifactType %q, want %q", ErrNotAnArtifact, m.ArtifactType, freeze.ArtifactType)
	}

	// Pull each layer by descriptor media type (order-independent, exactly as
	// publish writes them). A missing media type is a malformed artifact.
	byType := map[string][]byte{}
	for _, layer := range m.Layers {
		blob, err := content.FetchAll(ctx, src, layer)
		if err != nil {
			return store.FrozenVersion{}, freeze.Lockfile{}, fmt.Errorf("pull: fetch artifact layer %s: %w", layer.MediaType, err)
		}
		byType[layer.MediaType] = blob
	}

	get := func(mediaType string) ([]byte, error) {
		b, ok := byType[mediaType]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrMissingArtifact, mediaType)
		}
		return b, nil
	}
	fv := store.FrozenVersion{ID: tag}
	if fv.SkillMD, err = get(freeze.MediaTypeSkillMD); err != nil {
		return store.FrozenVersion{}, freeze.Lockfile{}, err
	}
	if fv.Lockfile, err = get(freeze.MediaTypeLock); err != nil {
		return store.FrozenVersion{}, freeze.Lockfile{}, err
	}
	if fv.Signature, err = get(freeze.MediaTypeSignature); err != nil {
		return store.FrozenVersion{}, freeze.Lockfile{}, err
	}
	if fv.PublicKey, err = get(freeze.MediaTypePublicKey); err != nil {
		return store.FrozenVersion{}, freeze.Lockfile{}, err
	}

	lock, err := freeze.ParseLockfile(fv.Lockfile)
	if err != nil {
		return store.FrozenVersion{}, freeze.Lockfile{}, fmt.Errorf("pull: %w", err)
	}
	return fv, lock, nil
}

// skillName parses the skill name from the pulled SKILL.md so the store keys
// the version directory on it. A name-less manifest cannot be installed.
func skillName(skillMD []byte) (string, error) {
	m, err := manifest.Parse(skillMD)
	if err != nil {
		return "", fmt.Errorf("pull: parse pulled SKILL.md: %w", err)
	}
	if m.Name == "" {
		return "", ErrNoName
	}
	return m.Name, nil
}

// isDigestReference reports whether ref is a digest (algo:hex) rather than a
// tag. registry.ParseReference accepts either in the Reference field; only a
// tag can serve as a frozen version id.
func isDigestReference(ref string) bool {
	for i := 0; i < len(ref); i++ {
		if ref[i] == ':' {
			return i > 0 && i < len(ref)-1
		}
	}
	return false
}
