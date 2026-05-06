package audit

import (
	"path/filepath"
	"time"
)

// dailyDir is the subdirectory under a state dir that holds the
// daily-rotated audit JSONL files.
const dailyDir = "audit"

// DailyPath returns today's audit JSONL file path inside stateDir,
// using the local clock. The file is `<stateDir>/audit/audit-YYYY-MM-DD.jsonl`.
//
// Production callers compute the path on each write so a session that
// crosses midnight rolls naturally to the next day's file.
func DailyPath(stateDir string) string {
	return DailyPathAt(stateDir, time.Now())
}

// DailyPathAt is the time-injectable variant of [DailyPath]. Tests use
// it to drive midnight-rollover scenarios without sleeping.
func DailyPathAt(stateDir string, t time.Time) string {
	return filepath.Join(stateDir, dailyDir, "audit-"+t.Format("2006-01-02")+".jsonl")
}

// DailyDir returns the subdirectory path the daily files live in:
// `<stateDir>/audit`. Useful for read-side callers that scan across
// every daily file (e.g., `aileron policy save`).
func DailyDir(stateDir string) string {
	return filepath.Join(stateDir, dailyDir)
}
