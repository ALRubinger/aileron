package proxybinding

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// bearerEntry is a minimal, valid bearer-scheme host entry for writer tests.
func bearerEntry(host, ref string) Entry {
	return Entry{Host: host, CredentialRef: ref, Scheme: "bearer"}
}

// parseFile reads and parses a descriptor file, failing the test on error.
func parseFile(t *testing.T, path string) Descriptor {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	d, err := Parse(data)
	if err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return d
}

// entryByKey returns the entry with the given dedup key, or false.
func entryByKey(entries []Entry, key string) (Entry, bool) {
	for _, e := range entries {
		if e.dedupKey() == key {
			return e, true
		}
	}
	return Entry{}, false
}

// Round-trip: Upsert of multiple entries into a fresh path yields, on re-read,
// exactly those entries with their fields intact.
func TestUpsert_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
	e1 := bearerEntry("api.example.com", "user/example")
	e2 := Entry{
		Host:          "api.linear.app",
		CredentialRef: "user/linear",
		Scheme:        "header-template",
		Header:        "Authorization",
		Template:      "{token}",
	}

	if err := Upsert(path, e1, e2); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	d := parseFile(t, path)
	if d.Version != SchemaVersion {
		t.Errorf("version = %q, want %q", d.Version, SchemaVersion)
	}
	if len(d.Bindings) != 2 {
		t.Fatalf("bindings = %d, want 2", len(d.Bindings))
	}
	for _, want := range []Entry{e1, e2} {
		got, ok := entryByKey(d.Bindings, want.dedupKey())
		if !ok {
			t.Fatalf("missing entry for key %q", want.dedupKey())
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("entry %q = %+v, want %+v", want.dedupKey(), got, want)
		}
	}
}

// Merge preserves unrelated entries: upserting a new entry onto a seeded file
// leaves the existing entries in place, in order, with a new entry appended.
func TestUpsert_PreservesUnrelatedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
	a := bearerEntry("a.example.com", "user/a")
	b := bearerEntry("b.example.com", "user/b")
	if err := Upsert(path, a, b); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	c := bearerEntry("c.example.com", "user/c")
	if err := Upsert(path, c); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	d := parseFile(t, path)
	if len(d.Bindings) != 3 {
		t.Fatalf("bindings = %d, want 3", len(d.Bindings))
	}
	// Original relative order preserved (a, b), c appended last.
	wantOrder := []Entry{a, b, c}
	for i, want := range wantOrder {
		if !reflect.DeepEqual(d.Bindings[i], want) {
			t.Errorf("bindings[%d] = %+v, want %+v", i, d.Bindings[i], want)
		}
	}
}

// Override by key: upserting an entry whose dedup key matches an existing one
// replaces it in place with the new fields; no duplicate is produced.
func TestUpsert_OverrideByKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
	orig := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/old",
		Scheme:        "header-template",
		Header:        "Authorization",
		Template:      "Bearer {token}",
	}
	if err := Upsert(path, orig); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	updated := Entry{
		Host:          "api.example.com",
		CredentialRef: "user/new",
		Scheme:        "header-template",
		Header:        "Authorization",
		Template:      "{token}",
	}
	if err := Upsert(path, updated); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	d := parseFile(t, path)
	if len(d.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1 (override, not duplicate)", len(d.Bindings))
	}
	if !reflect.DeepEqual(d.Bindings[0], updated) {
		t.Errorf("entry = %+v, want %+v", d.Bindings[0], updated)
	}
}

// Case-insensitive host override: a host differing only in case shares the
// dedup key, so upserting it replaces rather than duplicates (guards the
// strings.ToLower dedup contract, mirroring #1994).
func TestUpsert_CaseInsensitiveHostOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
	if err := Upsert(path, bearerEntry("api.linear.app", "user/lower")); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	upper := bearerEntry("API.Linear.App", "user/upper")
	if err := Upsert(path, upper); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	d := parseFile(t, path)
	if len(d.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1 (case-insensitive override)", len(d.Bindings))
	}
	if !reflect.DeepEqual(d.Bindings[0], upper) {
		t.Errorf("entry = %+v, want %+v", d.Bindings[0], upper)
	}
}

// Reject invalid document: an entry that fails Entry.Validate makes Upsert
// return an error and leaves the target file byte-identical (existing) or
// never creates it (absent).
func TestUpsert_RejectInvalidLeavesFileUnchanged(t *testing.T) {
	t.Run("existing file untouched", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
		if err := Upsert(path, bearerEntry("api.example.com", "user/example")); err != nil {
			t.Fatalf("seed Upsert: %v", err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}

		// Missing credential_ref fails Entry.Validate.
		invalid := Entry{Host: "bad.example.com", Scheme: "bearer"}
		if err := Upsert(path, invalid); err == nil {
			t.Fatal("Upsert accepted an invalid entry, want error")
		}

		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("file mutated on rejected Upsert:\nbefore=%q\nafter=%q", before, after)
		}
	})

	t.Run("absent file not created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
		// A sigv4-resign entry carrying a host is invalid per Entry.Validate.
		invalid := Entry{
			Host:          "s3.amazonaws.com",
			Kind:          "aws-sigv4",
			IdentityLabel: "default",
			CredentialRef: "aws-sigv4/s3/default",
			Scheme:        "sigv4-resign",
			AccessKeyID:   "AKIDEXAMPLE",
		}
		if err := Upsert(path, invalid); err == nil {
			t.Fatal("Upsert accepted an invalid entry, want error")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("target file exists after rejected Upsert; want absent (stat err = %v)", err)
		}
	})
}

// A malformed existing file (e.g. an operator hand-edited it into an invalid
// state) makes Upsert error rather than clobbering it: the write must never
// silently discard an unreadable existing document.
func TestUpsert_MalformedExistingFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
	// Wrong version fails Parse.
	garbage := []byte("version: v99\nbindings: []\n")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Upsert(path, bearerEntry("api.example.com", "user/example")); err == nil {
		t.Fatal("Upsert accepted a malformed existing file, want error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != string(garbage) {
		t.Errorf("file mutated on error:\nbefore=%q\nafter=%q", garbage, after)
	}
}

// File perms + dir creation: Upsert into a path whose parent dir is absent
// creates the dir and writes the file 0600.
func TestUpsert_CreatesDirAndPerms(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, ".aileron", "binding-descriptors.yaml")

	if err := Upsert(path, bearerEntry("api.example.com", "user/example")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600", perm)
	}
}

// New-file and empty-file creation both yield a valid v1 document.
func TestUpsert_NewAndEmptyFile(t *testing.T) {
	t.Run("new file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
		if err := Upsert(path, bearerEntry("api.example.com", "user/example")); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		d := parseFile(t, path)
		if d.Version != SchemaVersion {
			t.Errorf("version = %q, want %q", d.Version, SchemaVersion)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("seed empty file: %v", err)
		}
		if err := Upsert(path, bearerEntry("api.example.com", "user/example")); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		d := parseFile(t, path)
		if d.Version != SchemaVersion {
			t.Errorf("version = %q, want %q", d.Version, SchemaVersion)
		}
		if len(d.Bindings) != 1 {
			t.Fatalf("bindings = %d, want 1", len(d.Bindings))
		}
	})
}

// mergeEntries pure unit test: append, in-place override, self-dedup among
// incoming (last wins), and order preservation, with no disk I/O.
func TestMergeEntries(t *testing.T) {
	a := bearerEntry("a.example.com", "user/a")
	b := bearerEntry("b.example.com", "user/b")
	c := bearerEntry("c.example.com", "user/c")

	t.Run("append new key", func(t *testing.T) {
		got := mergeEntries([]Entry{a, b}, []Entry{c})
		want := []Entry{a, b, c}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("in-place override preserves order", func(t *testing.T) {
		bNew := bearerEntry("b.example.com", "user/b-new")
		got := mergeEntries([]Entry{a, b, c}, []Entry{bNew})
		want := []Entry{a, bNew, c}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("self-dedup among incoming last wins", func(t *testing.T) {
		aV1 := bearerEntry("dup.example.com", "user/v1")
		aV2 := bearerEntry("dup.example.com", "user/v2")
		got := mergeEntries(nil, []Entry{aV1, aV2})
		want := []Entry{aV2}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("does not mutate existing input", func(t *testing.T) {
		existing := []Entry{a, b}
		bNew := bearerEntry("b.example.com", "user/b-new")
		_ = mergeEntries(existing, []Entry{bNew})
		if existing[1].CredentialRef != "user/b" {
			t.Errorf("mergeEntries mutated its existing input: %+v", existing[1])
		}
	})
}
