package imgconfig

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ociConfigBytes marshals an OCI image config with the given execution fields
// plus serialization-only noise (created/history), the shape a registry serves.
func ociConfigBytes(t *testing.T, cfg ocispec.ImageConfig, diffIDs []string, os, arch string, withNoise bool) []byte {
	t.Helper()
	img := ocispec.Image{
		Platform: ocispec.Platform{OS: os, Architecture: arch},
		Config:   cfg,
	}
	for _, d := range diffIDs {
		img.RootFS.DiffIDs = append(img.RootFS.DiffIDs, digest.Digest(d))
	}
	img.RootFS.Type = "layers"
	if withNoise {
		now := time.Unix(1234567890, 0).UTC()
		img.Created = &now
		img.Author = "noise-author"
		img.History = []ocispec.History{{Created: &now, CreatedBy: "RUN noise", Comment: "layer"}}
	}
	b, err := json.Marshal(img)
	if err != nil {
		t.Fatalf("marshal oci config: %v", err)
	}
	return b
}

// dockerInspectBytes marshals the docker `image inspect --format '{{json .}}'`
// shape (capitalized top-level keys, diff_ids under RootFS.Layers) for the same
// logical image, so a test can prove the two readers agree.
func dockerInspectBytes(t *testing.T, cfg ocispec.ImageConfig, diffIDs []string, os, arch string) []byte {
	t.Helper()
	obj := map[string]any{
		"Id":           "sha256:" + strings.Repeat("0", 64),
		"Os":           os,
		"Architecture": arch,
		"Config":       cfg,
		"RootFS": map[string]any{
			"Type":   "layers",
			"Layers": diffIDs,
		},
		// Docker-only noise that must not affect the canonical digest.
		"DockerVersion": "27.0.0",
		"Created":       "2024-01-02T03:04:05Z",
	}
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal docker inspect: %v", err)
	}
	return b
}

func sampleConfig() (ocispec.ImageConfig, []string) {
	cfg := ocispec.ImageConfig{
		Env:        []string{"PATH=/usr/local/bin:/usr/bin", "LANG=C.UTF-8"},
		Entrypoint: []string{"/entry"},
		Cmd:        []string{"bash", "-lc", "run"},
		User:       "app",
		WorkingDir: "/work",
		Volumes:    map[string]struct{}{"/data": {}, "/cache": {}},
		ExposedPorts: map[string]struct{}{
			"8080/tcp": {},
			"53/udp":   {},
		},
		Labels: map[string]string{"org.opencontainers.image.title": "demo", "k": "v"},
	}
	diffIDs := []string{
		"sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("b", 64),
	}
	return cfg, diffIDs
}

func digestOf(t *testing.T, c CanonicalConfig) string {
	t.Helper()
	d, err := c.ContentDigest()
	if err != nil {
		t.Fatalf("ContentDigest: %v", err)
	}
	if !strings.HasPrefix(d, "sha256:") || len(d) != len("sha256:")+64 {
		t.Fatalf("content digest %q is not a sha256: value", d)
	}
	return d
}

// TestOCIAndDockerReadersAgree is the core contract: the same logical image
// canonicalizes to the same content digest whether read from a registry OCI
// config blob (with serialization noise) or a local `docker image inspect`.
// This is what makes the containerd re-serialization benign.
func TestOCIAndDockerReadersAgree(t *testing.T) {
	cfg, diffIDs := sampleConfig()

	ociNoNoise, err := FromOCIImageConfig(ociConfigBytes(t, cfg, diffIDs, "linux", "arm64", false))
	if err != nil {
		t.Fatalf("FromOCIImageConfig: %v", err)
	}
	ociWithNoise, err := FromOCIImageConfig(ociConfigBytes(t, cfg, diffIDs, "linux", "arm64", true))
	if err != nil {
		t.Fatalf("FromOCIImageConfig (noise): %v", err)
	}
	docker, err := FromDockerInspect(dockerInspectBytes(t, cfg, diffIDs, "linux", "arm64"))
	if err != nil {
		t.Fatalf("FromDockerInspect: %v", err)
	}

	dOCI := digestOf(t, ociNoNoise)
	dOCINoise := digestOf(t, ociWithNoise)
	dDocker := digestOf(t, docker)

	if dOCI != dOCINoise {
		t.Errorf("serialization noise changed the digest: %q vs %q", dOCI, dOCINoise)
	}
	if dOCI != dDocker {
		t.Errorf("OCI and docker readers disagree: %q vs %q", dOCI, dDocker)
	}
}

// TestReserializedConfigStable proves the exact #2014 scenario: re-encoding the
// OCI config (reordered map keys, added noise) does not change the digest.
func TestReserializedConfigStable(t *testing.T) {
	cfg, diffIDs := sampleConfig()
	a, err := FromOCIImageConfig(ociConfigBytes(t, cfg, diffIDs, "linux", "amd64", false))
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	// Re-serialize with a shuffled Labels/Env map and extra noise: same fields,
	// different bytes. The map reduction + json key sort must absorb it.
	cfg2 := cfg
	cfg2.Labels = map[string]string{"k": "v", "org.opencontainers.image.title": "demo"}
	b, err := FromOCIImageConfig(ociConfigBytes(t, cfg2, diffIDs, "linux", "amd64", true))
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if digestOf(t, a) != digestOf(t, b) {
		t.Error("re-serialized config produced a different content digest")
	}
}

// TestTamperedFieldRejected proves the integrity guarantee: changing any
// execution-relevant field changes the digest, so a substitution is caught.
func TestTamperedFieldRejected(t *testing.T) {
	cfg, diffIDs := sampleConfig()
	base := digestOf(t, mustParse(t, ociConfigBytes(t, cfg, diffIDs, "linux", "amd64", false)))

	cases := map[string]func(*ocispec.ImageConfig, *[]string, *string, *string){
		"entrypoint": func(c *ocispec.ImageConfig, _ *[]string, _, _ *string) { c.Entrypoint = []string{"/evil"} },
		"cmd":        func(c *ocispec.ImageConfig, _ *[]string, _, _ *string) { c.Cmd = []string{"rm", "-rf", "/"} },
		"env":        func(c *ocispec.ImageConfig, _ *[]string, _, _ *string) { c.Env = append(c.Env, "SECRET=leak") },
		"user":       func(c *ocispec.ImageConfig, _ *[]string, _, _ *string) { c.User = "root" },
		"workingdir": func(c *ocispec.ImageConfig, _ *[]string, _, _ *string) { c.WorkingDir = "/evil" },
		"volumes":    func(c *ocispec.ImageConfig, _ *[]string, _, _ *string) { c.Volumes = map[string]struct{}{"/x": {}} },
		"ports": func(c *ocispec.ImageConfig, _ *[]string, _, _ *string) {
			c.ExposedPorts = map[string]struct{}{"9/tcp": {}}
		},
		"labels":  func(c *ocispec.ImageConfig, _ *[]string, _, _ *string) { c.Labels = map[string]string{"k": "tampered"} },
		"diffids": func(_ *ocispec.ImageConfig, d *[]string, _, _ *string) { (*d)[0] = "sha256:" + strings.Repeat("c", 64) },
		"os":      func(_ *ocispec.ImageConfig, _ *[]string, os, _ *string) { *os = "windows" },
		"arch":    func(_ *ocispec.ImageConfig, _ *[]string, _, arch *string) { *arch = "386" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c2, d2 := sampleConfig()
			os, arch := "linux", "amd64"
			mutate(&c2, &d2, &os, &arch)
			got := digestOf(t, mustParse(t, ociConfigBytes(t, c2, d2, os, arch, false)))
			if got == base {
				t.Errorf("mutating %s did not change the content digest (tamper would pass)", name)
			}
		})
	}
}

func mustParse(t *testing.T, raw []byte) CanonicalConfig {
	t.Helper()
	c, err := FromOCIImageConfig(raw)
	if err != nil {
		t.Fatalf("FromOCIImageConfig: %v", err)
	}
	return c
}

// TestNormalizeNilVsEmpty proves nil and empty slices/maps hash identically, so
// a reader that omits an absent field and one that emits null/[]/{} agree.
func TestNormalizeNilVsEmpty(t *testing.T) {
	nilCfg := CanonicalConfig{OS: "linux", Architecture: "amd64"}
	emptyCfg := CanonicalConfig{
		DiffIDs: []string{}, Env: []string{}, Entrypoint: []string{},
		Cmd: []string{}, Volumes: []string{}, ExposedPorts: []string{},
		Labels: map[string]string{}, OS: "linux", Architecture: "amd64",
	}
	if digestOf(t, nilCfg) != digestOf(t, emptyCfg) {
		t.Error("nil and empty collections produced different digests")
	}
}

// TestFromDockerInspectArrayShape proves the default `docker inspect` array
// shape is accepted, not just the `--format '{{json .}}'` object shape.
func TestFromDockerInspectArrayShape(t *testing.T) {
	cfg, diffIDs := sampleConfig()
	obj := dockerInspectBytes(t, cfg, diffIDs, "linux", "amd64")
	arr := append(append([]byte("[\n"), obj...), []byte("\n]")...)
	fromArr, err := FromDockerInspect(arr)
	if err != nil {
		t.Fatalf("FromDockerInspect array: %v", err)
	}
	fromObj, err := FromDockerInspect(obj)
	if err != nil {
		t.Fatalf("FromDockerInspect object: %v", err)
	}
	if digestOf(t, fromArr) != digestOf(t, fromObj) {
		t.Error("array and object inspect shapes disagreed")
	}
}

func TestFromOCIImageConfigErrors(t *testing.T) {
	if _, err := FromOCIImageConfig(nil); err == nil {
		t.Error("want error for empty OCI config")
	}
	if _, err := FromOCIImageConfig([]byte("not json")); err == nil {
		t.Error("want error for invalid OCI config JSON")
	}
}

func TestFromDockerInspectErrors(t *testing.T) {
	if _, err := FromDockerInspect([]byte("   \n\t ")); err == nil {
		t.Error("want error for empty docker inspect output")
	}
	if _, err := FromDockerInspect([]byte("{bad")); err == nil {
		t.Error("want error for invalid docker inspect object")
	}
	if _, err := FromDockerInspect([]byte("[]")); err == nil {
		t.Error("want error for empty docker inspect array")
	}
	if _, err := FromDockerInspect([]byte("[bad")); err == nil {
		t.Error("want error for invalid docker inspect array")
	}
}
