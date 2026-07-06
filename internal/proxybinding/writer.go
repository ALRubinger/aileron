package proxybinding

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Upsert idempotently merges entries into the user binding-descriptors file
// at path, writing the result as a strict, human-editable v1 descriptor with
// 0600 permissions. It is the programmatic counterpart to hand-editing
// ~/.aileron/binding-descriptors.yaml: callers pass [DefaultUserPath] (tests
// inject a temp path) and one or more entries to add or replace.
//
// The merge is order-preserving and last-write-wins per [Entry.dedupKey]: an
// incoming entry whose key matches an existing entry replaces it in place, and
// an incoming entry with a new key is appended. Existing entries the caller did
// not name are never touched or reordered, so repeatedly upserting the same
// entries is a no-op on the document's meaningful content. Among the incoming
// entries themselves, a later entry overrides an earlier one with the same key.
//
// Validation happens entirely before any filesystem mutation. The merged
// document is marshaled and re-parsed through [Parse] as the single pre-write
// gate: this reuses [Entry.Validate], the strict-decode check, and the
// duplicate-key check, and simultaneously proves the serialized bytes re-parse
// clean. A document that fails validation returns a non-nil error and writes
// nothing, so an invalid Upsert never leaves a partial or corrupt file (an
// existing file is left byte-identical, an absent file is not created).
//
// A missing file (or an empty one) is treated as an empty v1 document, so the
// first Upsert into a fresh path creates a valid descriptor. The parent
// directory is created 0700 if absent.
//
// A zero-entry Upsert is a no-op: it returns nil without reading, creating, or
// rewriting the file, so an absent path stays absent and an existing file is
// left byte-identical.
//
// The write itself is atomic: the merged document is written to a temp file in
// the same directory and renamed over path, so a crash or concurrent writer
// never leaves a truncated or partially-written descriptor.
//
// Upsert handles only [Entry] values, which carry a credential *reference*
// (a vault path) and never credential bytes. It never resolves or logs secret
// material, inheriting this package's secret-hygiene invariant.
func Upsert(path string, entries ...Entry) error {
	// A zero-entry Upsert is a no-op: with nothing to merge in, the only
	// possible effect would be to create or rewrite the file for no reason,
	// so return before touching the filesystem. This keeps an absent path
	// absent and an existing file byte-identical.
	if len(entries) == 0 {
		return nil
	}

	existing, err := readExisting(path)
	if err != nil {
		return fmt.Errorf("proxybinding: read existing descriptor %q: %w", path, err)
	}

	merged := mergeEntries(existing, entries)

	doc := Descriptor{Version: SchemaVersion, Bindings: merged}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("proxybinding: marshal descriptor: %w", err)
	}

	// Route the pre-write check through Parse so the serialized bytes must
	// re-parse clean through the same validation the loader applies. This is
	// the only mutation gate: nothing below runs if the document is invalid.
	if _, err := Parse(data); err != nil {
		return fmt.Errorf("proxybinding: merged descriptor is invalid: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("proxybinding: create descriptor dir: %w", err)
	}
	if err := atomicWrite(path, data); err != nil {
		return fmt.Errorf("proxybinding: write descriptor %q: %w", path, err)
	}
	return nil
}

// atomicWrite replaces path with data via a temp-file-plus-rename so a crash or
// concurrent writer can never observe a truncated or partially-written
// descriptor: readers see either the old file or the fully-written new one. The
// temp file is created in the same directory as path (so the final rename is a
// same-filesystem operation, which is atomic on POSIX) with 0600 permissions,
// matching the descriptor's secret-adjacent sensitivity. On any error before
// the rename the temp file is removed so a failed write leaves no litter.
func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".binding-descriptors-*.yaml.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	// From here on, ensure the temp file is cleaned up on any failure path.
	// After a successful rename tmpName no longer exists, so the deferred
	// Remove is a harmless no-op.
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// readExisting reads the current descriptor entries at path for an upsert. It
// tolerates two "empty document" cases that are legitimate starting points for
// a write: an absent file and an existing file that is empty or whitespace-only
// (both yield nil entries so the first Upsert seeds a fresh document). Unlike
// [parseLayerFile] — the loader's read path, which treats a 0-byte file as a
// parse error — the writer must be able to bootstrap an empty file into a valid
// v1 document. A present file with real content is parsed strictly through
// [Parse]; a malformed one is an error so an upsert never silently discards an
// unreadable existing document.
func readExisting(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	d, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return d.Bindings, nil
}

// mergeEntries overlays incoming onto existing, preserving order and applying
// last-write-wins per [Entry.dedupKey]. It is the pure, disk-free core of
// [Upsert], factored out so the merge semantics are independently unit-testable.
//
// The result begins as a copy of existing in its original order. For each
// incoming entry in order, if its dedup key matches a slot already in the
// result the slot is replaced in place (so an override never duplicates or
// reorders the entry); otherwise the entry is appended. Applying incoming in
// order means a later incoming entry overrides an earlier one with the same
// key, matching the loader's last-write-wins semantics while keeping the
// document's existing entries in their original positions.
func mergeEntries(existing, incoming []Entry) []Entry {
	merged := make([]Entry, len(existing))
	copy(merged, existing)

	// index maps a dedup key to its slot in merged so an override lands in
	// place rather than appending a duplicate. It is rebuilt from existing and
	// extended as new keys are appended.
	index := make(map[string]int, len(merged))
	for i := range merged {
		index[merged[i].dedupKey()] = i
	}

	for _, e := range incoming {
		key := e.dedupKey()
		if pos, ok := index[key]; ok {
			merged[pos] = e
			continue
		}
		index[key] = len(merged)
		merged = append(merged, e)
	}
	return merged
}
