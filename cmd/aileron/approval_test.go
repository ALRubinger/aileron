package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeApprovalServer stands up an in-process httptest.NewServer that
// implements just enough of the `/v1/action-approvals` surface to
// drive the CLI tests. Each test scopes AILERON_API_URL to this
// server's URL so `approvalDoRequest` resolves to the fake.
func fakeApprovalServer(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/v1", fn))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")
	return srv
}

// TestRunApproval_DispatchUnknownSubcommand asserts the dispatch
// guard surfaces a clear error rather than running an unintended
// subcommand. Same shape as the keyring CLI for symmetry.
func TestRunApproval_DispatchUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"explode"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "unknown approval subcommand") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRunApproval_DispatchNoArgs covers the bare `aileron approval`
// invocation — should print usage rather than the unknown-command
// message.
func TestRunApproval_DispatchNoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runApproval(nil, &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRunApproval_ListEmpty exercises the happy path of `aileron
// approval list` against an empty queue. Confirms the human-friendly
// "No pending approvals." line that's the user's signal nothing's
// blocked.
func TestRunApproval_ListEmpty(t *testing.T) {
	fakeApprovalServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/action-approvals" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[]}`)
	})

	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"list"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No pending approvals.") {
		t.Errorf("output = %q", stdout.String())
	}
}

// TestRunApproval_ListShowsAllFields covers the load-bearing
// rendering path: every field a webapp / CLI user would want to see
// before deciding shows up in the printed output.
func TestRunApproval_ListShowsAllFields(t *testing.T) {
	fakeApprovalServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"items": [{
				"id": "act-test-1",
				"action_name": "send-email",
				"connector_fqn": "github://x/y",
				"args": {"to": "alice@example.com", "subject": "hi"},
				"session_id": "session-42",
				"requested_at": "2026-05-04T12:00:00Z"
			}]
		}`)
	})

	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"list"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"act-test-1",
		"send-email",
		"github://x/y",
		"alice@example.com",
		"session-42",
		"aileron approval approve <id>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestRunApproval_ListServerErrorReturnsNonZero asserts the CLI
// surfaces server-side failures instead of swallowing them. A 500
// from the daemon should produce a non-zero exit and an error line
// on stderr — the user needs to know their `list` query failed.
func TestRunApproval_ListServerErrorReturnsNonZero(t *testing.T) {
	fakeApprovalServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"boom"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"list"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "500") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRunApproval_ListServerUnreachable covers the daemon-not-running
// case. AILERON_API_URL points at a closed port; the CLI should
// return non-zero and surface the connect error.
func TestRunApproval_ListServerUnreachable(t *testing.T) {
	t.Setenv("AILERON_API_URL", "http://127.0.0.1:1/v1")
	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"list"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
}

// TestRunApproval_ApproveSendsApprovedTrue asserts the approve verb
// produces the right JSON body — `approved: true`. The server
// receives this and resolves the queue entry; the agent's blocked
// tool call returns success.
func TestRunApproval_ApproveSendsApprovedTrue(t *testing.T) {
	var receivedBody struct {
		Approved bool   `json:"approved"`
		Reason   string `json:"reason"`
	}
	var receivedPath string
	fakeApprovalServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
	})

	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"approve", "act-test-1"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr=%s", code, stderr.String())
	}
	if !receivedBody.Approved {
		t.Errorf("server received approved = false, want true")
	}
	if receivedPath != "/action-approvals/act-test-1/decide" {
		t.Errorf("path = %q", receivedPath)
	}
	if !strings.Contains(stdout.String(), "Approved act-test-1") {
		t.Errorf("output = %q", stdout.String())
	}
}

// TestRunApproval_DenyWithReasonFlag asserts the explicit `--reason`
// flag passes through. The reason gets to the agent in the failure
// envelope so it can recover gracefully.
func TestRunApproval_DenyWithReasonFlag(t *testing.T) {
	var receivedBody struct {
		Approved bool   `json:"approved"`
		Reason   string `json:"reason"`
	}
	fakeApprovalServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
	})

	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"deny", "act-test-1", "--reason", "wrong recipient"},
		&stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr=%s", code, stderr.String())
	}
	if receivedBody.Approved {
		t.Errorf("server received approved = true, want false")
	}
	if receivedBody.Reason != "wrong recipient" {
		t.Errorf("reason = %q", receivedBody.Reason)
	}
	if !strings.Contains(stdout.String(), "Denied act-test-1") {
		t.Errorf("output = %q", stdout.String())
	}
}

// TestRunApproval_DenyWithPositionalReason covers the ergonomic
// shorthand: `aileron approval deny <id> wrong-recipient` works
// without the explicit `--reason` flag. Documented at the dispatch
// site; tested here.
func TestRunApproval_DenyWithPositionalReason(t *testing.T) {
	var receivedBody struct {
		Approved bool   `json:"approved"`
		Reason   string `json:"reason"`
	}
	fakeApprovalServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
	})
	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"deny", "act-test-1", "wrong-recipient"},
		&stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr=%s", code, stderr.String())
	}
	if receivedBody.Reason != "wrong-recipient" {
		t.Errorf("reason = %q, want \"wrong-recipient\"", receivedBody.Reason)
	}
}

// TestRunApproval_ApproveMissingID asserts the dispatch guard fires
// when the user runs `aileron approval approve` without an id.
// Better than an HTTP call against `/decide` with empty id.
func TestRunApproval_ApproveMissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"approve"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRunApproval_ReasonFlagWithoutValue asserts `--reason` requires
// a value — common foot-gun if the user forgets to quote.
func TestRunApproval_ReasonFlagWithoutValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"deny", "act-test-1", "--reason"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "--reason requires a value") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRunApproval_ApproveAlreadyResolved covers the 404 path — the
// CLI surfaces it as a clear "unknown or already resolved" message
// rather than a generic HTTP error. This is the second-tab race or
// the post-timeout deny.
func TestRunApproval_ApproveAlreadyResolved(t *testing.T) {
	fakeApprovalServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"approve", "act-already-gone"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "unknown or already resolved") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRunApproval_ListExtraArgs asserts the sub-arg guard fires for
// `aileron approval list <unexpected>`. Mirrors the other CLI sub-
// commands' arg-count enforcement.
func TestRunApproval_ListExtraArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"list", "extra"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
}

// TestRunApproval_DenyWithUnrecognizedFlag asserts the parser rejects
// flags it doesn't know — better than silently treating them as
// reasons.
func TestRunApproval_DenyWithUnrecognizedFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runApproval([]string{"deny", "act-1", "--reason", "x", "--bogus"},
		&stdout, &stderr)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "unrecognized argument") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
