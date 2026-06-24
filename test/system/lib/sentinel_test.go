// Contract tests for the per-run sentinel (R9): the format
// AILERON_SYSTEST_OK_<runid>, freshness across calls (a stale workspace file can
// never pass), and the filename-safe charset.
package systestlib_test

import (
	"strings"
	"testing"

	systestlib "github.com/ALRubinger/aileron/test/system/lib"
)

func TestSentinelFormat(t *testing.T) {
	got := systestlib.Sentinel("17-42-abc")
	want := "AILERON_SYSTEST_OK_17-42-abc"
	if got != want {
		t.Errorf("Sentinel = %q; want %q", got, want)
	}
	if !strings.HasPrefix(got, "AILERON_SYSTEST_OK_") {
		t.Errorf("Sentinel %q missing the AILERON_SYSTEST_OK_ prefix", got)
	}
}

// TestNewRunIDFreshness proves two calls produce distinct run ids so a stale
// sentinel file from a prior run cannot satisfy the byte-exact R9 check.
func TestNewRunIDFreshness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := systestlib.NewRunID()
		if id == "" {
			t.Fatal("NewRunID returned empty")
		}
		if seen[id] {
			t.Fatalf("NewRunID produced a duplicate id %q within %d calls; freshness violated", id, i)
		}
		seen[id] = true
	}
}

// TestNewRunIDFilenameSafe proves the run id (and thus the sentinel that embeds
// it) uses only filename-safe characters [0-9a-z-], so writing it as a file is
// safe and the bash prompt comparison stays byte-exact.
func TestNewRunIDFilenameSafe(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := systestlib.NewRunID()
		for _, r := range id {
			ok := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || r == '-'
			if !ok {
				t.Fatalf("NewRunID %q contains a non-filename-safe rune %q", id, r)
			}
		}
		// The sentinel that embeds it must also be safe.
		s := systestlib.Sentinel(id)
		for _, r := range s {
			ok := (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || r == '-'
			if !ok {
				t.Fatalf("Sentinel %q contains an unexpected rune %q", s, r)
			}
		}
	}
}
