package audit

import (
	"time"

	"github.com/ALRubinger/aileron/internal/dailypath"
)

// auditSubdir / auditPrefix bind the audit log to a stable on-disk
// shape: `<stateDir>/audit/audit-YYYY-MM-DD.jsonl`. The path
// arithmetic itself lives in [dailypath]; this package owns the
// binding so callers asking for "today's audit path" don't have to
// know either name.
const (
	auditSubdir = "audit"
	auditPrefix = "audit-"
)

// DailyPath returns today's audit JSONL file path inside stateDir,
// using the local clock. Production callers compute the path on
// each write so a session that crosses midnight rolls naturally to
// the next day's file.
func DailyPath(stateDir string) string {
	return dailypath.Path(stateDir, auditSubdir, auditPrefix)
}

// DailyPathAt is the time-injectable variant of [DailyPath]. Tests
// use it to drive midnight-rollover scenarios without sleeping.
func DailyPathAt(stateDir string, t time.Time) string {
	return dailypath.PathAt(stateDir, auditSubdir, auditPrefix, t)
}

// DailyDir returns the subdirectory path the daily files live in:
// `<stateDir>/audit`. Useful for read-side callers that scan across
// every daily file (e.g., `aileron policy save`).
func DailyDir(stateDir string) string {
	return dailypath.Dir(stateDir, auditSubdir)
}
