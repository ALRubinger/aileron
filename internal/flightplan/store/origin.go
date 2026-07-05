package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// originFile is the non-signed sidecar recording where an OCI-installed frozen
// version was pulled from. It lives in the version directory alongside the four
// signed artifacts but is deliberately NOT part of them: it is never covered by
// the signature and never trusted for verification. It only records WHERE to
// fetch the published image at launch (#1903); every fetched byte is still
// verified against the signed lock's pin digest. A locally-frozen version has
// no origin sidecar, which is exactly the signal launch uses to stay on the
// local-tag boot path.
const originFile = "origin.json"

// Origin records the install source of an OCI-installed frozen version: the
// registry+repository the signed artifact was pulled from and the version tag it
// was published under. Launch derives the published composed image tag from the
// version id (freeze.ComposedImageTag) and pulls it from Registry, then verifies
// the pulled bytes against the signed lock pin. Neither field is a trust anchor
// for verification; they answer only "where to fetch".
type Origin struct {
	// Registry is the source registry+repository the signed artifact was pulled
	// from (e.g. "ghcr.io/acme/plan"), without tag or digest. Launch resolves the
	// published image in this repository.
	Registry string `json:"registry"`
	// VersionTag is the tag the signed artifact was published under (the freeze
	// slug that is also the store version id). Recorded for provenance and so a
	// future consumer that needs the artifact tag has it without re-deriving.
	VersionTag string `json:"versionTag"`
}

// WriteOrigin writes the install-origin sidecar for a frozen version into its
// version directory. It is idempotent: an identical rewrite is a no-op, and a
// differing rewrite overwrites the sidecar (the origin is non-signed provenance,
// not immutable content, so re-installing the same version from a different
// registry legitimately updates where launch pulls from). The write is atomic
// (temp file + rename) so a crash never leaves a partially written sidecar a
// later launch would misparse.
//
// WriteOrigin does NOT participate in the signed-artifact immutability check
// (verifyFrozenIdentical): re-installing or re-freezing a version stays a
// verified no-op on the signed files while the sidecar tracks the latest source.
func (s *Store) WriteOrigin(name, id string, o Origin) error {
	if name == "" {
		return fmt.Errorf("store: origin write needs a skill name")
	}
	if err := validVersionID(id); err != nil {
		return err
	}
	dir := s.FrozenDir(name, id)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("store: cannot write origin for missing frozen version %s/%s", name, id)
		}
		return fmt.Errorf("store: stat frozen version dir: %w", err)
	}

	data, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("store: encode origin: %w", err)
	}
	dst := filepath.Join(dir, originFile)
	// An identical rewrite is a no-op so a re-install of the same version from
	// the same source does not churn the file (and its mtime).
	if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, data) {
		return nil
	}

	tmp, err := os.CreateTemp(dir, originFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("store: create temp origin file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("store: write origin: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close origin: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("store: finalize origin: %w", err)
	}
	return nil
}

// ReadOrigin returns the install-origin sidecar for a frozen version. ok is
// false with a nil error when the version has no sidecar (a locally-frozen
// version), which is the signal launch uses to stay on the local-tag boot path.
// A malformed sidecar is a hard error rather than a silent local-path fallback:
// a version that recorded an origin but cannot report it must not be treated as
// locally frozen.
func (s *Store) ReadOrigin(name, id string) (Origin, bool, error) {
	if err := validVersionID(id); err != nil {
		return Origin{}, false, err
	}
	data, err := os.ReadFile(filepath.Join(s.FrozenDir(name, id), originFile))
	if err != nil {
		if os.IsNotExist(err) {
			return Origin{}, false, nil
		}
		return Origin{}, false, fmt.Errorf("store: read origin %s/%s: %w", name, id, err)
	}
	var o Origin
	if err := json.Unmarshal(data, &o); err != nil {
		return Origin{}, false, fmt.Errorf("store: decode origin %s/%s: %w", name, id, err)
	}
	return o, true, nil
}
