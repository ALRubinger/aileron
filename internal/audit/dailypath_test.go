package audit_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
)

func TestDailyPathAt_FormatsYYYYMMDD(t *testing.T) {
	stateDir := "/home/user/.aileron"
	got := audit.DailyPathAt(stateDir, time.Date(2026, 5, 5, 14, 22, 3, 0, time.UTC))
	want := filepath.Join(stateDir, "audit", "audit-2026-05-05.jsonl")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDailyPathAt_RollsOverAtMidnight(t *testing.T) {
	stateDir := "/x"
	beforeMidnight := time.Date(2026, 5, 5, 23, 59, 59, 999_000_000, time.UTC)
	afterMidnight := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)

	if a, b := audit.DailyPathAt(stateDir, beforeMidnight), audit.DailyPathAt(stateDir, afterMidnight); a == b {
		t.Fatalf("daily path should differ across midnight; both = %q", a)
	}
}

func TestDailyPath_UsesNowAndStateDir(t *testing.T) {
	got := audit.DailyPath("/home/user/.aileron")
	if !strings.HasPrefix(got, "/home/user/.aileron/audit/audit-") {
		t.Fatalf("unexpected prefix: %q", got)
	}
	if !strings.HasSuffix(got, ".jsonl") {
		t.Fatalf("missing .jsonl suffix: %q", got)
	}
}

func TestDailyDir_IsAuditSubdir(t *testing.T) {
	if got, want := audit.DailyDir("/x"), filepath.Join("/x", "audit"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
