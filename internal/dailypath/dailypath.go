// Package dailypath gives Aileron's on-disk JSONL surfaces a shared
// path-naming convention: `<stateDir>/<subdir>/<prefix>YYYY-MM-DD.jsonl`.
//
// Two surfaces use it today: the audit log
// (`~/.aileron/audit/audit-YYYY-MM-DD.jsonl`, ADR-0012) and the OTel
// trace exporter (`~/.aileron/traces/spans-YYYY-MM-DD.jsonl`, issue
// #390). Both rotate by local-clock day so a session that crosses
// midnight rolls naturally to the next day's file. Filenames embed
// the date so directory listings sort chronologically by name —
// scanning for replay (audit) or for a date range (future trace
// reader) is just `os.ReadDir` plus a sort by name.
//
// The package is deliberately tiny: just path arithmetic, no I/O.
// File creation, locking, and replay live with each consumer because
// the lifecycle needs differ — audit replays its files at startup to
// rebuild the in-memory index; the OTel exporter doesn't, since
// spans aren't queried by ID.
package dailypath

import (
	"path/filepath"
	"time"
)

// Path returns today's JSONL file path inside the supplied state
// directory, using the local clock. The file is
// `<stateDir>/<subdir>/<prefix>YYYY-MM-DD.jsonl`.
//
// Production callers compute the path on each write so a session
// that crosses midnight rolls naturally to the next day's file.
//
// `subdir` and `prefix` are caller-owned: the audit store passes
// `"audit"` / `"audit-"`; the trace exporter passes `"traces"` /
// `"spans-"`. The function does not validate them — non-conforming
// values produce non-conforming paths, which is the caller's
// problem to surface.
func Path(stateDir, subdir, prefix string) string {
	return PathAt(stateDir, subdir, prefix, time.Now())
}

// PathAt is the time-injectable variant of [Path]. Tests use it to
// drive midnight-rollover scenarios without sleeping.
func PathAt(stateDir, subdir, prefix string, t time.Time) string {
	return filepath.Join(stateDir, subdir, prefix+t.Format("2006-01-02")+".jsonl")
}

// Dir returns the subdirectory the daily files live in:
// `<stateDir>/<subdir>`. Useful for read-side callers that scan
// across every daily file (audit replay, future trace reader).
func Dir(stateDir, subdir string) string {
	return filepath.Join(stateDir, subdir)
}
