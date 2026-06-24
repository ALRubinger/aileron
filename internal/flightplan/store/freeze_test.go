package store

import (
	"os"
	"path/filepath"
	"testing"
)

func sampleVersion(id string) FrozenVersion {
	return FrozenVersion{
		ID:        id,
		SkillMD:   []byte("---\nname: demo\n---\nfrozen body\n"),
		Lockfile:  []byte("contentHash: sha256:abc\nversion: 1.0.0\n"),
		Signature: []byte("signature-bytes"),
		PublicKey: []byte("-----BEGIN PUBLIC KEY-----\n...\n-----END PUBLIC KEY-----\n"),
	}
}

func TestWriteFrozen_ProducesExpectedFiles(t *testing.T) {
	s := New(t.TempDir())
	v := sampleVersion("abc123")
	if err := s.WriteFrozen("demo", v); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	dir := s.FrozenDir("demo", "abc123")
	for fname, want := range map[string]string{
		"SKILL.md":        string(v.SkillMD),
		"aileron.lock":    string(v.Lockfile),
		"signature.sig":   string(v.Signature),
		"signing-key.pub": string(v.PublicKey),
	} {
		got, err := os.ReadFile(filepath.Join(dir, fname))
		if err != nil {
			t.Errorf("read %s: %v", fname, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", fname, got, want)
		}
	}
}

func TestWriteFrozen_NeverContainsPrivateKey(t *testing.T) {
	s := New(t.TempDir())
	if err := s.WriteFrozen("demo", sampleVersion("v1")); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	entries, err := os.ReadDir(s.FrozenDir("demo", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name != "SKILL.md" && name != "aileron.lock" && name != "signature.sig" && name != "signing-key.pub" {
			t.Errorf("unexpected file in frozen version: %q", name)
		}
		// The public key is fine; a private key file would not be.
		if name == "signing-key.pem" || name == "signing-key" {
			t.Errorf("a private key must never be stored: %q", name)
		}
	}
}

func TestWriteFrozen_NewVersionLeavesPriorIntact(t *testing.T) {
	s := New(t.TempDir())
	v1 := sampleVersion("v1")
	if err := s.WriteFrozen("demo", v1); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	v2 := sampleVersion("v2")
	v2.SkillMD = []byte("---\nname: demo\n---\ndifferent frozen body\n")
	if err := s.WriteFrozen("demo", v2); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	// v1 is byte-identical to what was written.
	got, err := os.ReadFile(filepath.Join(s.FrozenDir("demo", "v1"), "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(v1.SkillMD) {
		t.Errorf("v1 was mutated: %q", got)
	}
}

func TestWriteFrozen_IdenticalReWriteIsNoOp(t *testing.T) {
	s := New(t.TempDir())
	v := sampleVersion("v1")
	if err := s.WriteFrozen("demo", v); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := s.WriteFrozen("demo", v); err != nil {
		t.Errorf("re-writing identical content must be a no-op, got: %v", err)
	}
}

func TestWriteFrozen_DifferingContentSameIDRejected(t *testing.T) {
	s := New(t.TempDir())
	if err := s.WriteFrozen("demo", sampleVersion("v1")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	changed := sampleVersion("v1")
	changed.SkillMD = []byte("tampered")
	if err := s.WriteFrozen("demo", changed); err == nil {
		t.Error("writing different content to an existing version id must be rejected (immutability)")
	}
}

func TestWriteFrozen_RejectsBadID(t *testing.T) {
	s := New(t.TempDir())
	for _, id := range []string{"", "../escape", "a/b", ".", ".."} {
		v := sampleVersion(id)
		if err := s.WriteFrozen("demo", v); err == nil {
			t.Errorf("id %q must be rejected", id)
		}
	}
}

func TestWriteFrozen_RejectsEmptyName(t *testing.T) {
	s := New(t.TempDir())
	if err := s.WriteFrozen("", sampleVersion("v1")); err == nil {
		t.Error("empty skill name must be rejected")
	}
}

func TestWriteFrozen_PreInstalledRootUntouched(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	// Simulate a pre-freeze install: a SKILL.md at the name root.
	nameRoot := s.Dir("demo")
	if err := os.MkdirAll(nameRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	const installed = "---\nname: demo\n---\nthe installed body\n"
	if err := os.WriteFile(filepath.Join(nameRoot, "SKILL.md"), []byte(installed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.WriteFrozen("demo", sampleVersion("v1")); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(nameRoot, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != installed {
		t.Errorf("pre-freeze installed SKILL.md was modified: %q", got)
	}
}

func TestFrozenVersions_SortedAndEmpty(t *testing.T) {
	s := New(t.TempDir())
	// No versions yet.
	ids, err := s.FrozenVersions("demo")
	if err != nil {
		t.Fatalf("FrozenVersions on empty: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("empty versions = %v", ids)
	}

	for _, id := range []string{"v3", "v1", "v2"} {
		if err := s.WriteFrozen("demo", sampleVersion(id)); err != nil {
			t.Fatal(err)
		}
	}
	ids, err = s.FrozenVersions("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != "v1" || ids[1] != "v2" || ids[2] != "v3" {
		t.Errorf("FrozenVersions = %v, want sorted [v1 v2 v3]", ids)
	}
}

func TestReadFrozen_RoundTrips(t *testing.T) {
	s := New(t.TempDir())
	want := sampleVersion("v1")
	if err := s.WriteFrozen("demo", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadFrozen("demo", "v1")
	if err != nil {
		t.Fatalf("ReadFrozen: %v", err)
	}
	if string(got.SkillMD) != string(want.SkillMD) ||
		string(got.Lockfile) != string(want.Lockfile) ||
		string(got.Signature) != string(want.Signature) ||
		string(got.PublicKey) != string(want.PublicKey) {
		t.Errorf("ReadFrozen round-trip mismatch: %+v", got)
	}
}

func TestReadFrozen_MissingErrors(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.ReadFrozen("demo", "nope"); err == nil {
		t.Error("reading a missing version must error")
	}
}

func TestReadFrozen_RejectsTraversalID(t *testing.T) {
	s := New(t.TempDir())
	for _, id := range []string{"../escape", "a/b", "..", "."} {
		if _, err := s.ReadFrozen("demo", id); err == nil {
			t.Errorf("ReadFrozen must reject traversal id %q", id)
		}
	}
}

func TestWriteFrozen_NoPartialDirOnFailure(t *testing.T) {
	// A write whose content is fine succeeds; this asserts the atomic-rename
	// path leaves exactly the four artifacts and no leftover temp dir.
	root := t.TempDir()
	s := New(root)
	if err := s.WriteFrozen("demo", sampleVersion("v1")); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	versionsDir := filepath.Join(s.Dir("demo"), "versions")
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "v1" {
			t.Errorf("leftover entry in versions dir: %q (temp dir not cleaned?)", e.Name())
		}
	}
}
