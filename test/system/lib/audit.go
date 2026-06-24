// Pure R10 audit-round-trip parsing for the shell-free scenario driver. The bash
// counts `jq -c 'select(.event != null)'` lines in today's audit JSONL file and
// asserts the count is greater than zero, proving the agent's http_request tool
// produced a daemon-side audit record for the run. This file re-expresses that
// count + assertion as pure Go over the file bytes.
//
// The event-line definition is reimplemented locally (not by importing
// internal/audit) so the nested test/system/lib module stays dependency-free and
// isolated, consistent with the existing probes.go isolation. The behavior
// matches internal/audit.readMessageEntriesFromFile: skip empty lines, skip lines
// that do not JSON-decode, and skip lines whose decoded "event" field is empty;
// every remaining line is one event record.
package systestlib

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// CountAuditEventRecords counts the audit "event records" in a JSONL byte stream,
// matching the bash `jq -c 'select(.event != null)' | wc -l`. A line counts when
// it JSON-decodes to an object with a non-empty "event" field. Empty lines and
// non-JSON lines are skipped (mirroring internal/audit). The returned error is
// non-nil only for an underlying read error; a well-formed stream with zero event
// records returns (0, nil) so the caller (AssertAuditHasEvents) can render the
// faithful R10 diagnostic.
func CountAuditEventRecords(jsonl []byte) (int, error) {
	return countAuditEventRecords(bytes.NewReader(jsonl))
}

func countAuditEventRecords(r io.Reader) (int, error) {
	count := 0
	scanner := bufio.NewScanner(r)
	// Audit lines can be long (message bodies); raise the token limit above the
	// 64KiB default so a large but valid record is not truncated into a parse miss.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Event == "" {
			continue
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

// AssertAuditHasEvents is the R10 decision wrapper: it returns nil when the audit
// JSONL contains at least one event record, and otherwise the faithful R10
// diagnostic ("R10 no event records in <path> for this run"), mirroring the bash
// `fail` wording. auditPath is used only for the diagnostic. A read error from
// CountAuditEventRecords is surfaced as a non-nil error too.
func AssertAuditHasEvents(jsonl []byte, auditPath string) error {
	count, err := CountAuditEventRecords(jsonl)
	if err != nil {
		return Fail(fmt.Sprintf("R10 could not parse audit file %s: %v", auditPath, err))
	}
	if count > 0 {
		return nil
	}
	return Fail(fmt.Sprintf("R10 no event records in %s for this run", auditPath))
}

// CountAuditEventsForSession counts audit records whose "event" equals wantEvent
// and whose "session_id" equals wantSession. This is the session-scoped R10
// check: it proves THIS launch's session produced the expected comms event
// (e.g. "http_request_sent"), rather than counting any unrelated event record in
// the shared daily audit file (which can hold thousands of lines from other
// daemon activity). Empty/non-JSON lines are skipped. A read error is returned;
// zero matches is (0, nil). When wantSession is empty, the session_id is not
// constrained (any record with the matching event counts).
func CountAuditEventsForSession(jsonl []byte, wantEvent, wantSession string) (int, error) {
	count := 0
	scanner := bufio.NewScanner(bytes.NewReader(jsonl))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec struct {
			Event     string `json:"event"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Event != wantEvent {
			continue
		}
		if wantSession != "" && rec.SessionID != wantSession {
			continue
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

// AssertAuditHasEventForSession is the session-scoped R10 decision wrapper: it
// returns nil when the audit JSONL contains at least one record with
// event==wantEvent and session_id==wantSession, and otherwise the R10 diagnostic
// naming the event, session, and file. A read error is surfaced as non-nil too.
func AssertAuditHasEventForSession(jsonl []byte, wantEvent, wantSession, auditPath string) error {
	count, err := CountAuditEventsForSession(jsonl, wantEvent, wantSession)
	if err != nil {
		return Fail(fmt.Sprintf("R10 could not parse audit file %s: %v", auditPath, err))
	}
	if count > 0 {
		return nil
	}
	return Fail(fmt.Sprintf("R10 no %q audit record for session %s in %s (the agent's http_request round-trip did not reach the daemon)", wantEvent, wantSession, auditPath))
}
