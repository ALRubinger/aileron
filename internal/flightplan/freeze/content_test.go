package freeze

import (
	"regexp"
	"testing"
)

var sha256Pattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func TestContentHash_ShapeMatchesSchema(t *testing.T) {
	h := contentHash([]byte("manifest"), []byte("lock"))
	if !sha256Pattern.MatchString(h) {
		t.Errorf("contentHash = %q, want sha256:<64 hex>", h)
	}
}

func TestContentHash_StableAcrossRuns(t *testing.T) {
	m := []byte("frozen manifest bytes")
	l := []byte("lockfile bytes")
	a := contentHash(m, l)
	b := contentHash(m, l)
	if a != b {
		t.Errorf("contentHash not stable: %q != %q", a, b)
	}
}

func TestContentHash_ChangesWhenManifestChanges(t *testing.T) {
	l := []byte("lockfile bytes")
	a := contentHash([]byte("manifest A"), l)
	b := contentHash([]byte("manifest B"), l)
	if a == b {
		t.Error("contentHash must change when manifest content changes")
	}
}

func TestContentHash_ChangesWhenLockChanges(t *testing.T) {
	m := []byte("manifest")
	a := contentHash(m, []byte("lock A"))
	b := contentHash(m, []byte("lock B"))
	if a == b {
		t.Error("contentHash must change when lockfile content changes")
	}
}

// The 0x00 separator makes the two regions impossible to collide by
// shifting bytes across the boundary: (man, "x"+lock) must differ from
// (man+"x", lock).
func TestContentHash_SeparatorPreventsBoundaryCollision(t *testing.T) {
	a := contentHash([]byte("man"), []byte("xlock"))
	b := contentHash([]byte("manx"), []byte("lock"))
	if a == b {
		t.Error("separator must prevent a boundary-shift collision")
	}
}
