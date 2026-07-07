package ociremote

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/ALRubinger/aileron/internal/flightplan/imgconfig"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

// hostGOARCH is the architecture a single-child platform selection targets. It
// mirrors freeze's hostGOARCH seam: a package var (not a direct runtime.GOARCH
// read) so tests can exercise the foreign-arch child-selection branch
// deterministically regardless of the arch the test binary runs on, matching
// freeze.hostPlatform()'s arch vocabulary so a consumer verifies the child it
// actually attests (#2025). Production initializes it to runtime.GOARCH; a
// genuinely foreign consumer (e.g. an arm64 binary under QEMU) reports its own
// arch here naturally.
var hostGOARCH = runtime.GOARCH

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

// ConfigContentDigest resolves the serialization-agnostic content digest of the
// image config at desc (see imgconfig), the value a composed-tools pin binds to
// (ADR-0027). It fetches the actual config blob and canonicalizes it, so the
// digest is stable across the config-blob re-serialization the containerd image
// store performs on `docker push` (issue #2014). Both publish's post-push
// read-back and pull's install/launch verify call it, so the write and read
// halves compute the same value.
//
// It is store-agnostic in the manifest shape too: when desc is a classic image
// manifest it reads config directly; when desc is an OCI image index (or a
// Docker manifest list, the shape Docker Desktop's containerd store emits from
// `docker push` — the single-platform image manifest plus a buildkit
// attestation manifest), it unwraps the index to the platform image manifest
// first.
//
// Selection inside an index skips attestation entries (platform os == "unknown"
// or the docker attestation reference-type annotation). For the single-platform
// composed image exactly one real manifest remains and is used; when several
// real manifests exist the host runtime.GOOS/GOARCH match is chosen; when none
// is usable an actionable error is returned rather than a silently-wrong digest.
func ConfigContentDigest(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) (string, error) {
	blob, err := configBlob(ctx, fetcher, desc)
	if err != nil {
		return "", err
	}
	cc, err := imgconfig.FromOCIImageConfig(blob)
	if err != nil {
		return "", err
	}
	return cc.ContentDigest()
}

// PlatformConfigDigest is the serialization-agnostic config content digest of one
// platform image inside a multi-arch artifact, keyed by the `(OS, Arch)` the
// image config declares. AllPlatformConfigContentDigests returns one per runnable
// platform; the freeze producer maps these into the lock's per-arch pin set so a
// consumer selects the entry matching its host platform (ADR-0027).
type PlatformConfigDigest struct {
	OS     string
	Arch   string
	Digest string
}

// AllPlatformConfigContentDigests returns the serialization-agnostic config
// content digest for every runnable platform reachable from rootDesc. When
// rootDesc names an OCI image index / docker manifest list it enumerates all
// platform children (attestation/unknown-platform entries filtered out); when it
// names a single image manifest it yields exactly one entry. Each returned
// `(OS, Arch)` is read from the parsed config's own os/architecture, so the
// platform key ties to the exact config the consumer compares against, and the
// Digest is imgconfig.FromOCIImageConfig(config).ContentDigest() for that child.
// It is the all-arch generalization of ConfigContentDigest, used by the local
// OCI-layout reader the freeze multi-arch build feeds.
func AllPlatformConfigContentDigests(ctx context.Context, fetcher content.Fetcher, rootDesc ocispec.Descriptor) ([]PlatformConfigDigest, error) {
	var children []ocispec.Descriptor
	if isImageIndex(rootDesc.MediaType) {
		raw, err := content.FetchAll(ctx, fetcher, rootDesc)
		if err != nil {
			return nil, err
		}
		children, err = enumeratePlatformManifests(raw)
		if err != nil {
			return nil, err
		}
	} else {
		children = []ocispec.Descriptor{rootDesc}
	}
	out := make([]PlatformConfigDigest, 0, len(children))
	for _, child := range children {
		raw, err := content.FetchAll(ctx, fetcher, child)
		if err != nil {
			return nil, fmt.Errorf("fetch platform image manifest %s: %w", child.Digest, err)
		}
		cfg, err := configFromManifestBytes(ctx, fetcher, raw)
		if err != nil {
			return nil, err
		}
		cc, err := imgconfig.FromOCIImageConfig(cfg)
		if err != nil {
			return nil, err
		}
		digest, err := cc.ContentDigest()
		if err != nil {
			return nil, err
		}
		out = append(out, PlatformConfigDigest{OS: cc.OS, Arch: cc.Architecture, Digest: digest})
	}
	return out, nil
}

// configBlob fetches the raw image config blob bytes for the image at desc,
// unwrapping an OCI image index / docker manifest list to the platform image
// manifest first. It is the shared read the content-digest verification is
// computed over.
func configBlob(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) ([]byte, error) {
	raw, err := content.FetchAll(ctx, fetcher, desc)
	if err != nil {
		return nil, err
	}
	if isImageIndex(desc.MediaType) {
		child, err := selectPlatformManifest(raw)
		if err != nil {
			return nil, err
		}
		raw, err = content.FetchAll(ctx, fetcher, child)
		if err != nil {
			return nil, fmt.Errorf("fetch platform image manifest %s: %w", child.Digest, err)
		}
	}
	return configFromManifestBytes(ctx, fetcher, raw)
}

// configFromManifestBytes decodes a single image manifest's raw bytes and
// fetches its config blob. It is shared by the single-select configBlob path and
// the all-arch AllPlatformConfigContentDigests walk so both compute the config
// read the same way.
func configFromManifestBytes(ctx context.Context, fetcher content.Fetcher, raw []byte) ([]byte, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode image manifest: %w", err)
	}
	if m.Config.Digest == "" {
		return nil, fmt.Errorf("image manifest has no config digest")
	}
	cfg, err := content.FetchAll(ctx, fetcher, m.Config)
	if err != nil {
		return nil, fmt.Errorf("fetch image config blob %s: %w", m.Config.Digest, err)
	}
	return cfg, nil
}

// isImageIndex reports whether mediaType names a multi-manifest wrapper (an OCI
// image index or a Docker manifest list) rather than a single image manifest.
func isImageIndex(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageIndex || mediaType == dockerManifestListMediaType
}

// enumeratePlatformManifests returns every runnable image manifest descriptor in
// an index's raw bytes, dropping attestation/unknown-platform children. Unlike
// selectPlatformManifest it does not pick one: the all-arch config-digest walk
// wants the full per-platform set. It returns an actionable error when the index
// carries no runnable image manifest at all.
func enumeratePlatformManifests(raw []byte) ([]ocispec.Descriptor, error) {
	var idx ocispec.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("decode image index: %w", err)
	}
	candidates := make([]ocispec.Descriptor, 0, len(idx.Manifests))
	for _, m := range idx.Manifests {
		if isAttestationManifest(m) {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("image index has no runnable image manifest (found %d entr%s, all attestation or unknown-platform)", len(idx.Manifests), plural(len(idx.Manifests)))
	}
	return candidates, nil
}

// selectPlatformManifest picks the single runnable image manifest from an index's
// raw bytes for the host-only config read: it enumerates the runnable children,
// then returns the sole remaining manifest, or the host-platform match when
// several remain. It shares enumeratePlatformManifests with the all-arch walk so
// the two can never diverge on which children count as runnable.
func selectPlatformManifest(raw []byte) (ocispec.Descriptor, error) {
	candidates, err := enumeratePlatformManifests(raw)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	// Select the child for the consumer's target arch (the hostGOARCH seam,
	// runtime.GOARCH in production), matching freeze.hostPlatform()'s arch
	// vocabulary so a cross-arch consumer verifies the child it actually attests
	// (#2025).
	wantArch := hostGOARCH
	for _, m := range candidates {
		if m.Platform != nil && m.Platform.OS == runtime.GOOS && m.Platform.Architecture == wantArch {
			return m, nil
		}
	}
	return ocispec.Descriptor{}, fmt.Errorf("image index has %d image manifests but none match host platform %s/%s", len(candidates), runtime.GOOS, wantArch)
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
