package action

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// nearLineErr emulates a TOML decoder that does not implement the Line()
// method but embeds a "Near line N" prefix in its message — tomlErrorLine's
// fallback parser must handle this shape.
type nearLineErr struct{ msg string }

func (e nearLineErr) Error() string { return e.msg }

func TestTomlErrorLine_FallbackParsesNearLineN(t *testing.T) {
	got := tomlErrorLine(nearLineErr{msg: "Near line 7 (last key parsed): bogus"})
	if got != 6 { // 1-based "Near line 7" → frontmatter-relative line 6 (0-based offset)
		t.Errorf("tomlErrorLine = %d, want 6", got)
	}
}

func TestTomlErrorLine_FallbackOnUnparseableErrorReturnsZero(t *testing.T) {
	got := tomlErrorLine(errors.New("no line here"))
	if got != 0 {
		t.Errorf("tomlErrorLine = %d, want 0", got)
	}
}

func TestValidateConnectorFQN_InvalidURI(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"://no-scheme", "scheme"},
		{"github://", "owner"},
		{"github://owner", "path"},
		{"\x7f://bad", "URI"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := validateConnectorFQN(tc.in)
			if err == nil {
				t.Fatalf("validateConnectorFQN(%q) succeeded; want error", tc.in)
			}
		})
	}
}

func TestValidateSourceFQN_RejectsNonSemverTail(t *testing.T) {
	if err := validateSourceFQN("hub://aileron/x@notsemver"); err == nil {
		t.Fatal("validateSourceFQN succeeded; want SemVer error")
	}
}

func TestDefaultDir_FallbackWhenHomeUnset(t *testing.T) {
	// Force os.UserHomeDir to fail by clearing the env vars it consults.
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	got := DefaultDir()
	if got == "" {
		t.Fatal("DefaultDir returned empty string")
	}
	want := filepath.Join(".aileron", "actions")
	if got != want {
		t.Errorf("DefaultDir() = %q, want %q (fallback)", got, want)
	}
}

func TestStore_Load_DirIsAFileReturnsError(t *testing.T) {
	// ReadDir on a regular file returns an error — Load surfaces it rather
	// than silently treating the path as empty.
	tmp := t.TempDir()
	notADir := filepath.Join(tmp, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := NewStore(notADir)
	if _, err := s.Load(); err == nil {
		t.Fatal("Load() succeeded on a regular file path; want error")
	}
}
