package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Frozen-version artifact file names. A frozen version directory holds the
// frozen SKILL.md (lock injected), the standalone lockfile, the detached
// signature, and the author public key. The signing private key is NEVER
// among them.
const (
	frozenSkillFile     = "SKILL.md"
	frozenLockFile      = "aileron.lock"
	frozenSignatureFile = "signature.sig"
	frozenPublicKeyFile = "signing-key.pub"

	// versionsSubdir is the per-skill subdirectory holding immutable frozen
	// versions, `<root>/<name>/versions/<id>/`. The pre-freeze installed
	// SKILL.md at the name root is never touched.
	versionsSubdir = "versions"
)

// FrozenVersion is the content of one immutable frozen skill version: the
// freeze artifacts plus the version id the store keys the directory on.
type FrozenVersion struct {
	// ID is the version directory name, `<root>/<name>/versions/<ID>/`. It
	// must be a single path segment (no separators). Freeze uses a short
	// contentHash slug so distinct content lands in distinct directories.
	ID string
	// SkillMD is the frozen SKILL.md bytes (lock block injected).
	SkillMD []byte
	// Lockfile is the standalone aileron.lock bytes.
	Lockfile []byte
	// Signature is the detached ed25519 signature bytes.
	Signature []byte
	// PublicKey is the PEM-encoded author public key bytes.
	PublicKey []byte
}

// FrozenDir returns the on-disk directory for a named skill's frozen
// version, `<root>/<name>/versions/<id>`. It does not create the directory.
func (s *Store) FrozenDir(name, id string) string {
	return filepath.Join(s.Dir(name), versionsSubdir, id)
}

// WriteFrozen writes an immutable frozen version into the canonical store
// under `<root>/<name>/versions/<id>/`. Prior versions are never mutated or
// deleted: a new version id always creates a new directory.
//
// Re-writing an id that already exists is a verified no-op when the bytes
// are identical (idempotent re-freeze of the same content), and a hard
// error when the bytes differ (immutability: a version id never silently
// overwrites differing content). The pre-freeze installed SKILL.md at the
// name root is untouched.
func (s *Store) WriteFrozen(name string, v FrozenVersion) error {
	if name == "" {
		return fmt.Errorf("store: frozen write needs a skill name")
	}
	if v.ID == "" {
		return fmt.Errorf("store: frozen version needs an id")
	}
	if err := validVersionID(v.ID); err != nil {
		return err
	}

	dir := s.FrozenDir(name, v.ID)
	files := map[string][]byte{
		frozenSkillFile:     v.SkillMD,
		frozenLockFile:      v.Lockfile,
		frozenSignatureFile: v.Signature,
		frozenPublicKeyFile: v.PublicKey,
	}

	if _, err := os.Stat(dir); err == nil {
		// The version already exists: accept only a byte-identical rewrite,
		// reject differing content (never overwrite in place).
		if err := verifyFrozenIdentical(dir, files); err != nil {
			return err
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: stat frozen version dir: %w", err)
	}

	// Write atomically: stage every artifact in a sibling temp directory,
	// then rename it into place only after all writes succeed. A mid-write
	// failure therefore never leaves a partially populated version directory
	// that a later retry's identical-content check would trip over.
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("store: create frozen versions dir: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, v.ID+".tmp-*")
	if err != nil {
		return fmt.Errorf("store: create temp frozen dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	for fname, data := range files {
		if err := copyBytes(filepath.Join(tmp, fname), data, 0o644); err != nil {
			return fmt.Errorf("store: write frozen %s: %w", fname, err)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		// A concurrent writer may have created dir between the stat above and
		// this rename. Fall back to the identical-content check so a racing
		// duplicate freeze of the same content is still a no-op.
		if _, statErr := os.Stat(dir); statErr == nil {
			return verifyFrozenIdentical(dir, files)
		}
		return fmt.Errorf("store: finalize frozen version dir: %w", err)
	}
	return nil
}

// validVersionID rejects any id that is not a single, safe path segment so
// a crafted id can never escape the versions directory through FrozenDir.
func validVersionID(id string) error {
	if id == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return fmt.Errorf("store: frozen version id %q must be a single path segment", id)
	}
	return nil
}

// verifyFrozenIdentical confirms every artifact in an existing version
// directory matches the supplied bytes, returning an error naming the first
// mismatch. This is what makes a re-freeze of identical content a no-op
// while a content change to the same id is rejected.
func verifyFrozenIdentical(dir string, files map[string][]byte) error {
	for fname, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, fname))
		if err != nil {
			return fmt.Errorf("store: frozen version exists but %s is unreadable: %w", fname, err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("store: frozen version already exists with different %s; a version id is immutable", fname)
		}
	}
	return nil
}

// FrozenVersions lists the frozen version ids for a skill in sorted order.
// A skill with no frozen versions lists as empty (not an error).
func (s *Store) FrozenVersions(name string) ([]string, error) {
	dir := filepath.Join(s.Dir(name), versionsSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read frozen versions: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), frozenSkillFile)); err != nil {
			continue
		}
		ids = append(ids, e.Name())
	}
	sort.Strings(ids)
	return ids, nil
}

// LatestFrozen returns the id of the most-recently-frozen version of a skill
// and the total number of frozen versions. "Most recent" is ordered by the
// version directory's modification time, which reflects freeze order because
// WriteFrozen finalizes each new version by os.Rename-ing a staged temp dir
// into place (a fresh mtime), and an idempotent re-freeze of byte-identical
// content is a no-op that leaves the existing directory (and its mtime)
// untouched. Ties on mtime are broken by the lexicographically-highest id so
// the selection is deterministic. A skill with no frozen versions returns
// ("", 0, nil), not an error, mirroring FrozenVersions.
//
// This exists because version ids are content-hash slugs, so FrozenVersions'
// sorted-ids order is lexicographic, not chronological: picking the sorted max
// can launch an older version over a newer one (issue #1880). Callers that want
// the newest frozen version by freeze time use this instead.
func (s *Store) LatestFrozen(name string) (string, int, error) {
	dir := filepath.Join(s.Dir(name), versionsSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, nil
		}
		return "", 0, fmt.Errorf("store: read frozen versions: %w", err)
	}
	var (
		latestID  string
		latestSet bool
		latestMod time.Time
		count     int
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if _, err := os.Stat(filepath.Join(dir, id, frozenSkillFile)); err != nil {
			// Not a valid frozen version directory (no SKILL.md); skip it, matching
			// FrozenVersions' filtering.
			continue
		}
		info, err := os.Stat(filepath.Join(dir, id))
		if err != nil {
			continue
		}
		count++
		mod := info.ModTime()
		// Select the newest mtime; on a tie prefer the lexicographically-higher id
		// so the result is deterministic regardless of ReadDir ordering.
		if !latestSet || mod.After(latestMod) || (mod.Equal(latestMod) && id > latestID) {
			latestID = id
			latestMod = mod
			latestSet = true
		}
	}
	if !latestSet {
		return "", 0, nil
	}
	return latestID, count, nil
}

// ReadFrozen returns the artifacts of a named skill's frozen version.
func (s *Store) ReadFrozen(name, id string) (FrozenVersion, error) {
	if err := validVersionID(id); err != nil {
		return FrozenVersion{}, err
	}
	dir := s.FrozenDir(name, id)
	read := func(fname string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, fname))
		if err != nil {
			return nil, fmt.Errorf("store: read frozen %s: %w", fname, err)
		}
		return b, nil
	}
	v := FrozenVersion{ID: id}
	var err error
	if v.SkillMD, err = read(frozenSkillFile); err != nil {
		return FrozenVersion{}, err
	}
	if v.Lockfile, err = read(frozenLockFile); err != nil {
		return FrozenVersion{}, err
	}
	if v.Signature, err = read(frozenSignatureFile); err != nil {
		return FrozenVersion{}, err
	}
	if v.PublicKey, err = read(frozenPublicKeyFile); err != nil {
		return FrozenVersion{}, err
	}
	return v, nil
}

// copyBytes writes data to dst with perm, creating parent directories. It
// is the byte-level analogue of source.go copyFile for in-memory artifacts.
func copyBytes(dst string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, perm)
}
