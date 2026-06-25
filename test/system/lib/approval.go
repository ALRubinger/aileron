// Pure parsing for the `aileron approval list` output the scenario driver uses
// to find and approve the agent's pending http_request (R10). Kept in the
// GO_TEST_PACKAGES lib so the parse is unit-tested in CI; the impure
// exec.Command plumbing stays in the by-hand driver.
package systestlib

import "strings"

// ParseApprovalIDForSession returns the approval id of the first pending
// approval in `aileron approval list` output whose Session matches sessionID, or
// "" if none. The list format prints each approval as a two-space-indented id
// line followed by four-space-indented "Field: value" lines:
//
//	Pending approvals (1):
//
//	  act-abc123
//	    Action:    http_request
//	    Session:   01KVYK...
//	    Requested: ...
//
// Matching by session is sufficient: a launch's session id is unique to the run,
// and the scenario only triggers one approval (the R10 http_request).
func ParseApprovalIDForSession(listOutput, sessionID string) string {
	curID := ""
	for _, line := range strings.Split(listOutput, "\n") {
		// An id line is indented exactly two spaces (not the four-space field
		// lines) and is a single whitespace-free token.
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			tok := strings.TrimSpace(line)
			if tok != "" && !strings.ContainsAny(tok, " \t") {
				curID = tok
				continue
			}
		}
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "Session:"); ok {
			if strings.TrimSpace(rest) == sessionID && curID != "" {
				return curID
			}
		}
	}
	return ""
}
