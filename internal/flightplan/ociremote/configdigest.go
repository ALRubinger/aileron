package ociremote

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

// Docker media type / annotation constants not exported by image-spec. A
// containerd-backed `docker push` (Docker Desktop's default image store) emits
// an OCI image index wrapping the single-platform image manifest plus a
// buildkit attestation manifest; the attestation carries an
// unknown/unknown platform and the docker reference-type annotation below.
const (
	// dockerManifestListMediaType is the schema2 manifest-list media type, the
	// Docker equivalent of ocispec.MediaTypeImageIndex. Both wrap child manifests.
	dockerManifestListMediaType = "application/vnd.docker.distribution.manifest.list.v2+json"
	// dockerReferenceTypeAnnotation marks the role of a child manifest inside a
	// buildkit-produced index; attestation manifests carry the value below.
	dockerReferenceTypeAnnotation = "vnd.docker.reference.type"
	// dockerAttestationRefType is the dockerReferenceTypeAnnotation value that
	// flags a buildkit attestation (SBOM/provenance) manifest, not a runnable image.
	dockerAttestationRefType = "attestation-manifest"
	// unknownPlatform is the placeholder os/arch buildkit stamps on an attestation
	// manifest's platform so it is never selected for execution.
	unknownPlatform = "unknown"
)

// ConfigDigest resolves the config-blob digest of the image at desc, the value a
// composed-tools pin is bound to. It is store-agnostic: when desc is a classic
// image manifest it reads config.digest directly; when desc is an OCI image
// index (or a Docker manifest list), it unwraps the index to the single
// platform image manifest and reads THAT manifest's config.digest.
//
// The containerd image store behind Docker Desktop makes `docker push` emit an
// index wrapping the real single-platform image manifest plus a buildkit
// attestation manifest, so both publish's post-push read-back and pull's
// install/launch read-back must tolerate an index at the composed tag. The read
// is store-agnostic (it covers the classic single-manifest store and the
// containerd index store) rather than relying on push-side provenance flags.
//
// Selection inside an index skips attestation entries (platform os == "unknown"
// or the docker attestation reference-type annotation). For the single-platform
// composed image exactly one real manifest remains and is used; when several
// real manifests exist the host runtime.GOOS/GOARCH match is chosen; when none
// is usable an actionable error is returned rather than a silently-wrong digest.
func ConfigDigest(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) (string, error) {
	raw, err := content.FetchAll(ctx, fetcher, desc)
	if err != nil {
		return "", err
	}
	if isImageIndex(desc.MediaType) {
		child, err := selectPlatformManifest(raw)
		if err != nil {
			return "", err
		}
		raw, err = content.FetchAll(ctx, fetcher, child)
		if err != nil {
			return "", fmt.Errorf("fetch platform image manifest %s: %w", child.Digest, err)
		}
	}
	return configDigestFromManifest(raw)
}

// isImageIndex reports whether mediaType names a multi-manifest wrapper (an OCI
// image index or a Docker manifest list) rather than a single image manifest.
func isImageIndex(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageIndex || mediaType == dockerManifestListMediaType
}

// configDigestFromManifest decodes raw as a single image manifest (OCI or the
// wire-compatible docker schema2 manifest) and returns its config blob digest.
func configDigestFromManifest(raw []byte) (string, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("decode image manifest: %w", err)
	}
	if m.Config.Digest == "" {
		return "", fmt.Errorf("image manifest has no config digest")
	}
	return m.Config.Digest.String(), nil
}

// selectPlatformManifest picks the runnable image manifest from an index's raw
// bytes: it drops attestation/unknown-platform entries, then returns the sole
// remaining manifest, or the host-platform match when several remain. It returns
// an actionable error when no runnable manifest is present.
func selectPlatformManifest(raw []byte) (ocispec.Descriptor, error) {
	var idx ocispec.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("decode image index: %w", err)
	}
	candidates := make([]ocispec.Descriptor, 0, len(idx.Manifests))
	for _, m := range idx.Manifests {
		if isAttestationManifest(m) {
			continue
		}
		candidates = append(candidates, m)
	}
	switch len(candidates) {
	case 0:
		return ocispec.Descriptor{}, fmt.Errorf("image index has no runnable image manifest (found %d entr%s, all attestation or unknown-platform)", len(idx.Manifests), plural(len(idx.Manifests)))
	case 1:
		return candidates[0], nil
	}
	for _, m := range candidates {
		if m.Platform != nil && m.Platform.OS == runtime.GOOS && m.Platform.Architecture == runtime.GOARCH {
			return m, nil
		}
	}
	return ocispec.Descriptor{}, fmt.Errorf("image index has %d image manifests but none match host platform %s/%s", len(candidates), runtime.GOOS, runtime.GOARCH)
}

// isAttestationManifest reports whether a child descriptor is a buildkit
// attestation manifest rather than a runnable image: buildkit stamps it with an
// unknown/unknown platform and the docker attestation reference-type annotation.
func isAttestationManifest(m ocispec.Descriptor) bool {
	if m.Platform != nil && m.Platform.OS == unknownPlatform {
		return true
	}
	return m.Annotations[dockerReferenceTypeAnnotation] == dockerAttestationRefType
}

// plural returns "ies" for n != 1 and "y" for n == 1 so the count message reads
// naturally ("1 entry" / "2 entries").
func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
