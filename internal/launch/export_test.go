package launch

import "time"

// OpenSessionLogger exposes openSessionLogger for testing.
var OpenSessionLogger = openSessionLogger

// SessionLogPath exposes sessionLogPath for testing.
var SessionLogPath = sessionLogPath

// `wrapText` and `visibleWidth` lived in the pty rendering files
// (panel.go) deleted by #419; their tests die with them.

// IncomingForTest is the test-only shape for seeding a CommsServer's
// NotifyQueue with an "incoming" message. Used by the comms-server
// queue tests (#428) to set up a draft_reply scenario where the
// original message is in the queue and the dispatcher can route the
// edited reply back through the matching listener.
type IncomingForTest struct {
	ID      string
	Service string
	Channel string
	Author  string
	Body    string
}

// PushIncomingForTest pushes a synthetic incoming message onto the
// CommsServer's NotifyQueue, exposed for tests in the `launch_test`
// package. Production callers go through the actual comms.Listener
// dispatch path.
func PushIncomingForTest(cs *CommsServer, msg IncomingForTest) {
	cs.queue.Push(Message{
		ID:        msg.ID,
		Source:    msg.Service,
		Channel:   msg.Channel,
		Author:    msg.Author,
		Body:      msg.Body,
		Preview:   msg.Body,
		Timestamp: time.Now(),
	})
}

// SetApprovalTTLForTest overrides the CommsServer's approval-wait
// timeout so timeout-path tests can drive the deadline deterministically
// without sleeping for minutes.
func SetApprovalTTLForTest(cs *CommsServer, d time.Duration) {
	cs.approvalTTL = d
}

// MatchSecretForTest exposes matchSecret so tests can assert the
// per-name URL pattern matching without round-tripping through the
// httpRequest dispatch path.
func MatchSecretForTest(cs *CommsServer, url string) (string, bool) {
	return cs.matchSecret(url)
}
