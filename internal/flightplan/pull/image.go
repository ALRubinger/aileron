package pull

import (
	"context"
	"errors"
	"fmt"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/ociremote"

	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/errdef"
)

// Image-pull errors. Every one is a fail-closed refusal: a mismatch, a missing
// published image, or a pull/network failure never yields a bootable reference.
var (
	// ErrImageMissing is returned when the published image is absent from the
	// source repository (the composed-image tag or the pinned digest does not
	// resolve). A clean machine that installed the artifact but whose registry no
	// longer serves the image cannot boot.
	ErrImageMissing = errors.New("pull: published image is missing from the source registry")
	// ErrImageDigestMismatch is returned when the pulled image does not verify
	// against the signed lock pin under the pin's binding kind (config content
	// digest for a composed pin, manifest digest for an image-only/foreign-base
	// pin). The registry served a different image than the signature attested.
	ErrImageDigestMismatch = errors.New("pull: pulled image does not match the signed lock digest")
	// ErrImagePullFailed wraps any other pull/network/fetch failure so the caller
	// refuses to boot rather than proceed on a partial pull.
	ErrImagePullFailed = errors.New("pull: could not pull the published image")
)

// ImagePullOptions configures a published-image pull+verify (#1903). The source
// coordinate answers WHERE to pull; the verified Pin answers WHAT to verify. The
// two roles stay distinct: the coordinate comes from the operator's own install
// ref (recorded at install), the digest from the signature-covered lock.
type ImagePullOptions struct {
	// Source is the content store to resolve+fetch the image from. Nil builds a
	// remote.Repository from Registry over the operator's docker credentials.
	// Tests inject an in-memory store.
	Source oras.ReadOnlyTarget
	// Registry is the registry+repository the published image lives in (the
	// install origin, e.g. "ghcr.io/acme/plan"), without tag or digest. It is
	// the fetch coordinate and, for a composed pin, the base of the returned
	// bootable reference.
	Registry string
	// VersionTag is the store version id (freeze slug). The composed image was
	// published under freeze.ComposedImageTag(VersionTag); launch resolves it
	// back through that tag.
	VersionTag string
	// Pin is the verified image pin from the signed lock. freeze.BindingKind(Pin)
	// governs verification; Pin.Digest is the attested digest. It is authoritative
	// from the signature, never from any registry annotation.
	Pin freeze.ImagePin
}

// ImagePullResult reports the verified, bootable reference for the published
// image.
type ImagePullResult struct {
	// BootRef is the reference the runner boots verbatim, content-addressed to a
	// manifest digest in both cases so a mutable tag can never be booted after
	// verification. For a composed pin it is "<registry>@<manifest-digest>" where
	// the manifest was config-content-verified against the signed lock; for an
	// image-only/foreign-base pin it is "<registry>@<manifest-digest>" where the
	// manifest digest itself is the signed lock pin. The value derives only from
	// the verified digests and the recorded coordinate, never from a re-parse of
	// untrusted manifest bytes beyond the digest comparison itself.
	BootRef string
	// BindingKind is the binding the verification used
	// (freeze.BindingConfigContentDigest or freeze.BindingManifestDigest),
	// surfaced for provenance/logging.
	BindingKind string
	// ImageDigest is the verified digest the boot reference is anchored to: the
	// image config content digest for a composed pin, the manifest digest for an
	// image-only/foreign-base pin. Both equal Pin.Digest by construction.
	ImageDigest string
}

// PullImage pulls the published image named by the recorded coordinate and
// verifies it against the signed lock pin under the pin's binding kind, returning
// a bootable reference. It never boots: a mismatch, a missing image, or a pull
// failure returns a typed error and no reference.
//
// The binding kind is derived purely from freeze.BindingKind(opts.Pin), rooted
// in the signature-covered lock, so a tampered registry annotation cannot change
// what launch verifies. This mirrors publish's publishComposed/publishForeignBase
// split exactly, the two halves of the same contract.
func PullImage(ctx context.Context, opts ImagePullOptions) (ImagePullResult, error) {
	if opts.Registry == "" {
		return ImagePullResult{}, fmt.Errorf("%w: no source registry recorded for the install", ErrImagePullFailed)
	}
	if opts.Pin.Digest == "" {
		return ImagePullResult{}, fmt.Errorf("%w: the signed lock pins no image digest", ErrImageDigestMismatch)
	}

	src := opts.Source
	if src == nil {
		repo, err := ociremote.NewRepository(opts.Registry)
		if err != nil {
			return ImagePullResult{}, fmt.Errorf("%w: source registry %q: %w", ErrImagePullFailed, opts.Registry, err)
		}
		src = repo
	}

	switch freeze.BindingKind(opts.Pin) {
	case freeze.BindingConfigContentDigest:
		return pullComposedImage(ctx, src, opts)
	default:
		return pullManifestDigestImage(ctx, src, opts)
	}
}

// pullComposedImage resolves the composed image by its published tag, reads its
// config CONTENT digest through the shared ociremote.ConfigContentDigest helper
// (the same index-aware, serialization-agnostic read publish uses post-push, so
// the read and write halves compute the same value), and requires it to equal
// the signed lock pin. Binding to the config content digest (not the config
// blob's own sha256) is what lets a clean-machine install verify an image
// published through the containerd store, which re-serializes the config blob on
// push (issue #2014). When the published ref is an OCI image index (the
// containerd store behind Docker Desktop), the helper unwraps it to the platform
// image manifest before reading config; BootRef stays anchored to the resolved
// (index) manifest digest, which the runner/daemon boot correctly. The bootable
// reference is content-addressed to the resolved MANIFEST digest
// ("<registry>@<manifest-digest>"), not the mutable tag: the config-content
// check proved this exact manifest carries the attested config, so anchoring the
// boot to that manifest digest closes the TOCTOU window a tag would leave open
// (the tag could be repointed between this verification and the runner's boot).
// The runner boots the same manifest bytes verified here, and the daemon
// re-checks the digest itself on pull.
func pullComposedImage(ctx context.Context, src oras.ReadOnlyTarget, opts ImagePullOptions) (ImagePullResult, error) {
	imageTag := freeze.ComposedImageTag(opts.VersionTag)
	desc, err := src.Resolve(ctx, imageTag)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return ImagePullResult{}, fmt.Errorf("%w: composed image tag %q in %q", ErrImageMissing, imageTag, opts.Registry)
		}
		return ImagePullResult{}, fmt.Errorf("%w: resolve composed image %q: %w", ErrImagePullFailed, imageTag, err)
	}
	if desc.Digest == "" {
		return ImagePullResult{}, fmt.Errorf("%w: composed image %q resolved to an empty manifest digest", ErrImagePullFailed, imageTag)
	}

	config, err := ociremote.ConfigContentDigest(ctx, src, desc)
	if err != nil {
		return ImagePullResult{}, fmt.Errorf("%w: read composed image config: %w", ErrImagePullFailed, err)
	}
	if config != opts.Pin.Digest {
		return ImagePullResult{}, fmt.Errorf("%w: composed image config %s, lock attested %s", ErrImageDigestMismatch, config, opts.Pin.Digest)
	}

	return ImagePullResult{
		// Boot by the verified manifest digest, not the tag: content-addressed so
		// the runner cannot boot a repointed tag, while the config-content check
		// above is the identity binding to the signed lock.
		BootRef:     opts.Registry + "@" + desc.Digest.String(),
		BindingKind: freeze.BindingConfigContentDigest,
		ImageDigest: config,
	}, nil
}

// pullManifestDigestImage resolves the published image by the attested manifest
// digest and requires the resolved descriptor's digest to equal the signed lock
// pin (publish copied the exact bytes with oras.Copy, which preserves the
// manifest digest). The bootable reference is the content-addressed
// "<registry>@<digest>", which the daemon resolves and verifies itself.
func pullManifestDigestImage(ctx context.Context, src oras.ReadOnlyTarget, opts ImagePullOptions) (ImagePullResult, error) {
	desc, err := src.Resolve(ctx, opts.Pin.Digest)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return ImagePullResult{}, fmt.Errorf("%w: image %s@%s", ErrImageMissing, opts.Registry, opts.Pin.Digest)
		}
		return ImagePullResult{}, fmt.Errorf("%w: resolve image %s@%s: %w", ErrImagePullFailed, opts.Registry, opts.Pin.Digest, err)
	}
	// A registry that resolves a digest reference to a divergent descriptor is
	// misbehaving; assert to fail closed rather than boot the served bytes.
	if desc.Digest.String() != opts.Pin.Digest {
		return ImagePullResult{}, fmt.Errorf("%w: resolved manifest %s, lock attested %s", ErrImageDigestMismatch, desc.Digest, opts.Pin.Digest)
	}

	return ImagePullResult{
		BootRef:     opts.Registry + "@" + opts.Pin.Digest,
		BindingKind: freeze.BindingManifestDigest,
		ImageDigest: opts.Pin.Digest,
	}, nil
}
