package ociremote

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/imgconfig"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

// writeBlob writes data into the layout's content-addressed blob store and
// returns its descriptor.
func writeBlob(t *testing.T, dir, mediaType string, data []byte) ocispec.Descriptor {
	t.Helper()
	desc := content.NewDescriptorFromBytes(mediaType, data)
	path := filepath.Join(dir, "blobs", desc.Digest.Algorithm().String(), desc.Digest.Encoded())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir blob dir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	return desc
}

// archImageManifest writes a config blob (declaring os/arch), a layer, and an
// image manifest into dir, returning the platform-stamped manifest descriptor and
// the config bytes.
func archImageManifest(t *testing.T, dir, os, arch, marker string) (ocispec.Descriptor, []byte) {
	t.Helper()
	img := ocispec.Image{
		Platform: ocispec.Platform{OS: os, Architecture: arch},
		Config: ocispec.ImageConfig{
			Env:        []string{"PATH=/usr/bin"},
			Entrypoint: []string{"/entry", marker},
			Cmd:        []string{"bash"},
			WorkingDir: "/work",
		},
	}
	img.RootFS.Type = "layers"
	configBody, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	cfg := writeBlob(t, dir, ocispec.MediaTypeImageConfig, configBody)
	layer := writeBlob(t, dir, ocispec.MediaTypeImageLayerGzip, []byte("layer-"+marker))
	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfg,
		Layers:    []ocispec.Descriptor{layer},
	}
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	md := writeBlob(t, dir, ocispec.MediaTypeImageManifest, mb)
	md.Platform = &ocispec.Platform{OS: os, Architecture: arch}
	return md, configBody
}

// writeOCILayoutRoot writes the oci-layout marker and the top-level index.json
// pointing at the single root descriptor, completing a valid OCI image layout.
func writeOCILayoutRoot(t *testing.T, dir string, root ocispec.Descriptor) {
	t.Helper()
	layout, err := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err != nil {
		t.Fatalf("marshal oci-layout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ocispec.ImageLayoutFile), layout, 0o644); err != nil {
		t.Fatalf("write oci-layout: %v", err)
	}
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{root},
	}
	ib, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ocispec.ImageIndexFile), ib, 0o644); err != nil {
		t.Fatalf("write index.json: %v", err)
	}
}

func wantOCIContentDigest(t *testing.T, body []byte) string {
	t.Helper()
	cc, err := imgconfig.FromOCIImageConfig(body)
	if err != nil {
		t.Fatalf("FromOCIImageConfig: %v", err)
	}
	d, err := cc.ContentDigest()
	if err != nil {
		t.Fatalf("ContentDigest: %v", err)
	}
	return d
}

// TestConfigContentDigestsFromOCILayout_TwoArch proves the reader yields the
// correct per-arch content digest for each platform in a synthetic two-arch OCI
// layout (index.json -> manifest list -> two image manifests -> two configs).
func TestConfigContentDigestsFromOCILayout_TwoArch(t *testing.T) {
	dir := t.TempDir()
	amd, amdBody := archImageManifest(t, dir, "linux", "amd64", "amd")
	arm, armBody := archImageManifest(t, dir, "linux", "arm64", "arm")
	list := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd, arm},
	}
	lb, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal manifest list: %v", err)
	}
	root := writeBlob(t, dir, ocispec.MediaTypeImageIndex, lb)
	writeOCILayoutRoot(t, dir, root)

	got, err := ConfigContentDigestsFromOCILayout(context.Background(), dir)
	if err != nil {
		t.Fatalf("ConfigContentDigestsFromOCILayout: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 per-arch digests, got %d: %+v", len(got), got)
	}
	byArch := map[string]string{}
	for _, p := range got {
		if p.OS != "linux" {
			t.Errorf("os = %q, want linux", p.OS)
		}
		byArch[p.Arch] = p.Digest
	}
	if want := wantOCIContentDigest(t, amdBody); byArch["amd64"] != want {
		t.Errorf("amd64 digest = %q, want %q", byArch["amd64"], want)
	}
	if want := wantOCIContentDigest(t, armBody); byArch["arm64"] != want {
		t.Errorf("arm64 digest = %q, want %q", byArch["arm64"], want)
	}
}

// TestConfigContentDigestsFromOCILayout_NoRunnableManifestErrors proves a layout
// whose manifest list carries no runnable image (only an unknown-platform child)
// fails closed rather than yielding an empty set.
func TestConfigContentDigestsFromOCILayout_NoRunnableManifestErrors(t *testing.T) {
	dir := t.TempDir()
	// A single unknown/unknown child stands in for a buildkit attestation-only list.
	child, _ := archImageManifest(t, dir, "unknown", "unknown", "att")
	child.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}
	list := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{child},
	}
	lb, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal manifest list: %v", err)
	}
	root := writeBlob(t, dir, ocispec.MediaTypeImageIndex, lb)
	writeOCILayoutRoot(t, dir, root)

	if _, err := ConfigContentDigestsFromOCILayout(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "runnable image manifest") {
		t.Fatalf("err = %v, want a no-runnable-manifest error", err)
	}
}

// TestConfigContentDigestsFromOCILayout_MissingLayoutErrors proves opening a
// directory that is not an OCI layout fails with a clear error, not a panic.
func TestConfigContentDigestsFromOCILayout_MissingLayoutErrors(t *testing.T) {
	if _, err := ConfigContentDigestsFromOCILayout(context.Background(), t.TempDir()); err == nil {
		t.Fatal("want an error for a directory that is not an OCI layout")
	}
}
