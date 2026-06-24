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

func TestCountAuditEventsForSessionMatchesEventAndSession(t *testing.T) {
	jsonl := []byte(strings.Join([]string{
		`{"event":"http_request_sent","session_id":"sess-A"}`, // counts
		`{"event":"http_request_sent","session_id":"sess-B"}`, // wrong session: skipped
		`{"event":"message_sent","session_id":"sess-A"}`,      // wrong event: skipped
		`{"event":"http_request_sent"}`,                       // no session_id: skipped (session constrained)
		`{"event":"http_request_sent","session_id":"sess-A"}`, // counts
		``,         // empty: skipped
		`not json`, // malformed: skipped
	}, "\n") + "\n")
	got, err := systestlib.CountAuditEventsForSession(jsonl, "http_request_sent", "sess-A")
	if err != nil {
		t.Fatalf("CountAuditEventsForSession returned error %v; want nil", err)
	}
	if got != 2 {
		t.Errorf("count = %d; want 2 (only http_request_sent records for sess-A)", got)
	}
}

func TestCountAuditEventsForSessionEmptySessionIgnoresSession(t *testing.T) {
	// An empty wantSession constrains only the event, not the session_id.
	jsonl := []byte(strings.Join([]string{
		`{"event":"http_request_sent","session_id":"sess-A"}`,
		`{"event":"http_request_sent","session_id":"sess-B"}`,
	}, "\n") + "\n")
	got, err := systestlib.CountAuditEventsForSession(jsonl, "http_request_sent", "")
	if err != nil {
		t.Fatalf("CountAuditEventsForSession returned error %v; want nil", err)
	}
	if got != 2 {
		t.Errorf("count = %d; want 2 (both events, session unconstrained)", got)
	}
}

func TestAssertAuditHasEventForSessionHappyPath(t *testing.T) {
	jsonl := []byte(`{"event":"http_request_sent","session_id":"sess-A"}` + "\n")
	if err := systestlib.AssertAuditHasEventForSession(jsonl, "http_request_sent", "sess-A", "/state/audit/x.jsonl"); err != nil {
		t.Errorf("returned error %v; want nil for a matching record", err)
	}
}

func TestAssertAuditHasEventForSessionMissing(t *testing.T) {
	// The session's event is absent (only another session's record present).
	jsonl := []byte(`{"event":"http_request_sent","session_id":"other"}` + "\n")
	const path = "/state/audit/audit-2026-06-24.jsonl"
	err := systestlib.AssertAuditHasEventForSession(jsonl, "http_request_sent", "sess-A", path)
	if err == nil {
		t.Fatal("returned nil; want the R10 missing-record diagnostic")
	}
	for _, sub := range []string{"R10", "http_request_sent", "sess-A", path} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not contain %q", err.Error(), sub)
		}
	}
}
