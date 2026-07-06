// Package imgconfig computes a serialization-agnostic content digest over an
// image's execution-relevant configuration, the identity a composed-tools
// Flight Plan pin binds to (ADR-0027).
//
// The problem it solves: a composed image built at freeze is pinned by an
// attested digest, and both publish and launch re-check the registry copy
// against it. Binding to the config BLOB's own sha256 breaks under Docker
// Desktop's containerd image store, which re-serializes the image config on
// `docker push` (OCI config media type / schema normalization). The re-encoded
// blob has a different sha256 even though every field that governs what the
// container runs is byte-identical, so the raw-digest check spuriously reports
// tampering (issue #2014).
//
// Instead we parse the config and hash a canonical projection of the fields
// that actually pin execution: the filesystem (rootfs.diff_ids) plus every
// runtime field (Env, Entrypoint, Cmd, User, WorkingDir, Volumes, ExposedPorts,
// Labels) and the platform (os/arch). Serialization-only noise
// (container/container_config, docker_version, created, history) is dropped
// before hashing, so the SAME image hashes identically whether the bytes come
// from a local `docker image inspect`, the classic docker config blob, or the
// containerd-re-serialized OCI config blob. Because every execution-affecting
// field is still covered, a swapped Entrypoint/Env/User/etc. changes the digest
// and is rejected: this is content-based verification, never a blanket skip of
// the integrity check.
package imgconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// CanonicalConfig is the serialization-agnostic projection of an image config
// that a composed pin's content digest is taken over. Map-valued config fields
// (Volumes, ExposedPorts) are reduced to sorted key slices, and Labels is a map
// (encoding/json sorts map keys), so the marshalled form is deterministic
// regardless of the source encoder's field or key ordering. Ordered fields
// (Env, Entrypoint, Cmd, DiffIDs) preserve their order because it is
// execution-significant.
type CanonicalConfig struct {
	DiffIDs      []string          `json:"diffIDs"`
	Env          []string          `json:"env"`
	Entrypoint   []string          `json:"entrypoint"`
	Cmd          []string          `json:"cmd"`
	User         string            `json:"user"`
	WorkingDir   string            `json:"workingDir"`
	Volumes      []string          `json:"volumes"`
	ExposedPorts []string          `json:"exposedPorts"`
	Labels       map[string]string `json:"labels"`
	OS           string            `json:"os"`
	Architecture string            `json:"architecture"`
}

// ContentDigest returns the `sha256:<hex>` content digest of the canonical
// config: sha256 over the deterministic JSON encoding of the normalized value.
// The same CanonicalConfig always yields the same digest, and any change to a
// covered field changes it.
func (c CanonicalConfig) ContentDigest() (string, error) {
	raw, err := json.Marshal(c.normalized())
	if err != nil {
		return "", fmt.Errorf("imgconfig: marshal canonical config: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// normalized returns a copy with nil slices/maps replaced by empty ones, so a
// field that one encoder emits as null and another omits (or emits as [] / {})
// hashes identically. This is what lets a local docker-inspect read and a
// registry OCI-config read of the same image agree.
func (c CanonicalConfig) normalized() CanonicalConfig {
	if c.DiffIDs == nil {
		c.DiffIDs = []string{}
	}
	if c.Env == nil {
		c.Env = []string{}
	}
	if c.Entrypoint == nil {
		c.Entrypoint = []string{}
	}
	if c.Cmd == nil {
		c.Cmd = []string{}
	}
	if c.Volumes == nil {
		c.Volumes = []string{}
	}
	if c.ExposedPorts == nil {
		c.ExposedPorts = []string{}
	}
	if c.Labels == nil {
		c.Labels = map[string]string{}
	}
	return c
}

// FromOCIImageConfig parses an OCI image config blob (the
// application/vnd.oci.image.config.v1+json document, or the wire-compatible
// docker schema2 config) into a CanonicalConfig. This is the registry-side
// reader: publish's post-push read-back and pull's verify fetch the config blob
// and canonicalize it here.
func FromOCIImageConfig(raw []byte) (CanonicalConfig, error) {
	if len(raw) == 0 {
		return CanonicalConfig{}, fmt.Errorf("imgconfig: empty image config")
	}
	var img ocispec.Image
	if err := json.Unmarshal(raw, &img); err != nil {
		return CanonicalConfig{}, fmt.Errorf("imgconfig: decode image config: %w", err)
	}
	diffIDs := make([]string, 0, len(img.RootFS.DiffIDs))
	for _, d := range img.RootFS.DiffIDs {
		diffIDs = append(diffIDs, d.String())
	}
	return fromParts(img.OS, img.Architecture, diffIDs, img.Config), nil
}

// dockerInspect is the subset of `docker image inspect --format '{{json .}}'`
// output the canonical projection needs. Docker's inspect `.Config` reuses the
// same capitalized JSON keys as the OCI image config (User, Env, Entrypoint,
// Cmd, Volumes, ExposedPorts, WorkingDir, Labels), so ocispec.ImageConfig
// decodes it directly. The layer diff_ids live under `.RootFS.Layers` in
// docker's inspect shape (vs `rootfs.diff_ids` in the OCI config), so they are
// pulled from there.
type dockerInspect struct {
	OS           string              `json:"Os"`
	Architecture string              `json:"Architecture"`
	Config       ocispec.ImageConfig `json:"Config"`
	RootFS       struct {
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

// FromDockerInspect parses `docker image inspect --format '{{json .}}'` output
// into a CanonicalConfig. This is the local-side reader: freeze attests the
// composed image's content digest from it, publish re-checks the local image
// before pushing, and local (unpublished) launch re-checks the daemon image.
// It tolerates both a single JSON object (the `--format '{{json .}}'` shape) and
// a one-element JSON array (the default `docker inspect` shape).
func FromDockerInspect(raw []byte) (CanonicalConfig, error) {
	trimmed := trimSpace(raw)
	if len(trimmed) == 0 {
		return CanonicalConfig{}, fmt.Errorf("imgconfig: empty docker inspect output")
	}
	if trimmed[0] == '[' {
		var arr []dockerInspect
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return CanonicalConfig{}, fmt.Errorf("imgconfig: decode docker inspect array: %w", err)
		}
		if len(arr) == 0 {
			return CanonicalConfig{}, fmt.Errorf("imgconfig: docker inspect returned no image")
		}
		di := arr[0]
		return fromParts(di.OS, di.Architecture, di.RootFS.Layers, di.Config), nil
	}
	var di dockerInspect
	if err := json.Unmarshal(trimmed, &di); err != nil {
		return CanonicalConfig{}, fmt.Errorf("imgconfig: decode docker inspect object: %w", err)
	}
	return fromParts(di.OS, di.Architecture, di.RootFS.Layers, di.Config), nil
}

// fromParts assembles a CanonicalConfig from the os/arch, diff_ids, and the
// shared OCI ImageConfig shape both readers land on, reducing the two map fields
// to sorted key slices.
func fromParts(os, arch string, diffIDs []string, cfg ocispec.ImageConfig) CanonicalConfig {
	return CanonicalConfig{
		DiffIDs:      diffIDs,
		Env:          cfg.Env,
		Entrypoint:   cfg.Entrypoint,
		Cmd:          cfg.Cmd,
		User:         cfg.User,
		WorkingDir:   cfg.WorkingDir,
		Volumes:      sortedKeys(cfg.Volumes),
		ExposedPorts: sortedKeys(cfg.ExposedPorts),
		Labels:       cfg.Labels,
		OS:           os,
		Architecture: arch,
	}
}

// sortedKeys returns the keys of a set-shaped map (map[string]struct{}) in
// sorted order so the projection is order-independent.
func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// trimSpace trims ASCII whitespace from both ends without allocating a string.
func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
