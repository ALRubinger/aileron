package systestlib_test

import (
	"strings"
	"testing"

	systestlib "github.com/ALRubinger/aileron/test/system/lib"
)

const sampleApprovalList = `Pending approvals (2):

  act-aaa111
    Action:    send_message
    Session:   other-session
    Requested: 2026-06-24T10:00:00Z

  act-bbb222
    Action:    http_request
    Session:   01KVYK-target
    Requested: 2026-06-24T10:00:01Z
    Args:
      method: GET
      url: http://127.0.0.1:60036/healthz

Approve with: aileron approval approve <id>
Deny with:    aileron approval deny <id> [--reason "..."]`

func TestParseApprovalIDForSession(t *testing.T) {
	if got := systestlib.ParseApprovalIDForSession(sampleApprovalList, "01KVYK-target"); got != "act-bbb222" {
		t.Errorf("ParseApprovalIDForSession = %q; want act-bbb222", got)
	}
	// The other session's approval resolves to its own id.
	if got := systestlib.ParseApprovalIDForSession(sampleApprovalList, "other-session"); got != "act-aaa111" {
		t.Errorf("ParseApprovalIDForSession(other) = %q; want act-aaa111", got)
	}
}

func TestParseApprovalIDForSessionNoMatch(t *testing.T) {
	if got := systestlib.ParseApprovalIDForSession(sampleApprovalList, "absent-session"); got != "" {
		t.Errorf("ParseApprovalIDForSession(absent) = %q; want empty", got)
	}
	if got := systestlib.ParseApprovalIDForSession("No pending approvals.\n", "any"); got != "" {
		t.Errorf("ParseApprovalIDForSession(empty queue) = %q; want empty", got)
	}
}

func TestParseApprovalIDForSessionDoesNotMisreadFieldLines(t *testing.T) {
	// A four-space-indented "Session:" line must not be mistaken for an id, and
	// the id must come from the two-space-indented line above it.
	out := strings.Join([]string{
		"Pending approvals (1):",
		"",
		"  act-xyz",
		"    Action:    http_request",
		"    Session:   sess-1",
	}, "\n")
	if got := systestlib.ParseApprovalIDForSession(out, "sess-1"); got != "act-xyz" {
		t.Errorf("got %q; want act-xyz", got)
	}
}
