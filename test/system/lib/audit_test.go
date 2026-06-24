// Contract tests for the pure R10 audit parser. They assert the happy path (a
// file with N event lines counts N), the documented-failure path (no event lines
// yields the faithful "no event records" diagnostic via the assert wrapper), and
// the skip rules that match internal/audit: malformed JSON lines and empty lines
// are skipped, and a line whose "event" field is empty/absent is not an event
// record.
package systestlib_test

import (
	"strings"
	"testing"

	systestlib "github.com/ALRubinger/aileron/test/system/lib"
)

func TestCountAuditEventRecordsHappyPath(t *testing.T) {
	jsonl := []byte(strings.Join([]string{
		`{"event":"message_received","session_id":"s1"}`,
		`{"event":"message_sent","session_id":"s1"}`,
		`{"event":"reply_sent","session_id":"s2"}`,
	}, "\n") + "\n")
	got, err := systestlib.CountAuditEventRecords(jsonl)
	if err != nil {
		t.Fatalf("CountAuditEventRecords returned error %v; want nil", err)
	}
	if got != 3 {
		t.Errorf("count = %d; want 3", got)
	}
}

func TestCountAuditEventRecordsSkipRules(t *testing.T) {
	jsonl := []byte(strings.Join([]string{
		`{"event":"message_received"}`, // counts
		``,                             // empty line: skipped
		`   `,                          // whitespace-only line: skipped
		`not json at all`,              // malformed: skipped
		`{"event":""}`,                 // empty event field: skipped
		`{"session_id":"s1"}`,          // no event field: skipped
		`{"event":"reply_sent"}`,       // counts
	}, "\n") + "\n")
	got, err := systestlib.CountAuditEventRecords(jsonl)
	if err != nil {
		t.Fatalf("CountAuditEventRecords returned error %v; want nil", err)
	}
	if got != 2 {
		t.Errorf("count = %d; want 2 (only the two non-empty event lines)", got)
	}
}

func TestCountAuditEventRecordsEmptyStream(t *testing.T) {
	got, err := systestlib.CountAuditEventRecords([]byte(""))
	if err != nil {
		t.Fatalf("CountAuditEventRecords returned error %v; want nil", err)
	}
	if got != 0 {
		t.Errorf("count = %d; want 0 for an empty stream", got)
	}
}

func TestAssertAuditHasEventsHappyPath(t *testing.T) {
	jsonl := []byte(`{"event":"message_sent","session_id":"s1"}` + "\n")
	if err := systestlib.AssertAuditHasEvents(jsonl, "/state/audit/audit-2026-06-24.jsonl"); err != nil {
		t.Errorf("AssertAuditHasEvents returned error %v; want nil for a file with an event record", err)
	}
}

func TestAssertAuditHasEventsNoRecords(t *testing.T) {
	// A file with only non-event lines must fail with the faithful R10 diagnostic.
	jsonl := []byte(`{"session_id":"s1"}` + "\n" + `not json` + "\n")
	const path = "/state/audit/audit-2026-06-24.jsonl"
	err := systestlib.AssertAuditHasEvents(jsonl, path)
	if err == nil {
		t.Fatal("AssertAuditHasEvents returned nil; want the R10 no-event-records error")
	}
	for _, sub := range []string{"R10", "no event records", path} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not contain %q", err.Error(), sub)
		}
	}
}
