package ociremote

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/oci"
)

// ConfigContentDigestsFromOCILayout reads a local OCI image-layout directory (the
// artifact a `docker buildx build --output type=oci,dest=<dir>` multi-arch build
// writes) and returns the serialization-agnostic config content digest for every
// runnable platform it carries. It opens dir as a read-only OCI store, resolves
// the root manifest-list descriptor from the layout's index.json, and delegates
// to AllPlatformConfigContentDigests, so the freeze producer fills the lock's
// per-arch pin set straight from the just-built layout with no registry push and
// no registry auth. The layout is read only; the caller owns its lifetime (freeze
// keeps it for the publish handoff, it does not delete it here).
func ConfigContentDigestsFromOCILayout(ctx context.Context, dir string) ([]PlatformConfigDigest, error) {
	store, err := oci.NewFromFS(ctx, os.DirFS(dir))
	if err != nil {
		return nil, fmt.Errorf("open OCI layout %s: %w", dir, err)
	}
	root, err := ociLayoutRootDescriptor(dir)
	if err != nil {
		return nil, err
	}
	return AllPlatformConfigContentDigests(ctx, store, root)
}

// ociLayoutRootDescriptor reads the layout's top-level index.json and returns the
// single root descriptor it points at (the multi-arch manifest list buildx writes
// for a `type=oci` multi-platform build, or a single image manifest for a one-arch
// build). A layout whose index.json does not carry exactly one root is rejected
// with an actionable error rather than silently picking one.
func ociLayoutRootDescriptor(dir string) (ocispec.Descriptor, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ocispec.ImageIndexFile))
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("read OCI layout %s: %w", ocispec.ImageIndexFile, err)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("decode OCI layout %s: %w", ocispec.ImageIndexFile, err)
	}
	if len(idx.Manifests) != 1 {
		return ocispec.Descriptor{}, fmt.Errorf("OCI layout %s has %d root manifests, want exactly 1", ocispec.ImageIndexFile, len(idx.Manifests))
	}
	return idx.Manifests[0], nil
}
