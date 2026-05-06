package dailypath_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/dailypath"
)

// PathAt contract:
//   - Renders <stateDir>/<subdir>/<prefix>YYYY-MM-DD.jsonl exactly.
//   - Date formatting is deterministic across timezones (the package
//     uses the local clock; tests inject UTC for stability).
//   - Different (subdir, prefix) combinations don't collide.
//   - Path rolls over at midnight: the same stateDir+subdir+prefix
//     yields different paths on consecutive days.

func TestPathAt_RendersStateDirSubdirPrefixDate(t *testing.T) {
	stateDir := "/home/user/.aileron"
	got := dailypath.PathAt(stateDir, "audit", "audit-",
		time.Date(2026, 5, 5, 14, 22, 3, 0, time.UTC))
	want := filepath.Join(stateDir, "audit", "audit-2026-05-05.jsonl")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPathAt_TraceExporterShape(t *testing.T) {
	// The OTel trace exporter consumes this with subdir="traces"
	// and prefix="spans-"; the resulting filename mirrors the
	// audit shape but lands in its own directory.
	stateDir := "/home/user/.aileron"
	got := dailypath.PathAt(stateDir, "traces", "spans-",
		time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC))
	want := filepath.Join(stateDir, "traces", "spans-2026-05-05.jsonl")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPathAt_DifferentConsumersDontCollide(t *testing.T) {
	t0 := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	auditPath := dailypath.PathAt("/x", "audit", "audit-", t0)
	tracePath := dailypath.PathAt("/x", "traces", "spans-", t0)
	if auditPath == tracePath {
		t.Fatalf("audit and traces paths must differ; both = %q", auditPath)
	}
	if filepath.Dir(auditPath) == filepath.Dir(tracePath) {
		t.Errorf("audit and traces should live in different subdirs; both in %q", filepath.Dir(auditPath))
	}
}

func TestPathAt_RollsOverAtMidnight(t *testing.T) {
	stateDir := "/x"
	beforeMidnight := time.Date(2026, 5, 5, 23, 59, 59, 999_000_000, time.UTC)
	afterMidnight := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	if a, b := dailypath.PathAt(stateDir, "audit", "audit-", beforeMidnight),
		dailypath.PathAt(stateDir, "audit", "audit-", afterMidnight); a == b {
		t.Fatalf("daily path should differ across midnight; both = %q", a)
	}
}

func TestPath_UsesNow(t *testing.T) {
	// Path is the live-clock convenience wrapper; assert the shape
	// without pinning the date so the test stays deterministic.
	got := dailypath.Path("/home/user/.aileron", "audit", "audit-")
	if !strings.HasPrefix(got, "/home/user/.aileron/audit/audit-") {
		t.Fatalf("unexpected prefix: %q", got)
	}
	if !strings.HasSuffix(got, ".jsonl") {
		t.Fatalf("missing .jsonl suffix: %q", got)
	}
}

func TestDir_StateDirSubdirJoin(t *testing.T) {
	if got, want := dailypath.Dir("/x", "audit"), filepath.Join("/x", "audit"); got != want {
		t.Fatalf("audit Dir = %q, want %q", got, want)
	}
	if got, want := dailypath.Dir("/x", "traces"), filepath.Join("/x", "traces"); got != want {
		t.Fatalf("traces Dir = %q, want %q", got, want)
	}
}
