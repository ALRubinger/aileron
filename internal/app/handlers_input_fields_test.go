package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/approval"
)

// TestInputFieldsForAPI pins the adapter that maps the action
// package's [action.InputField] slice — what [action.BuildInputFields]
// returns — onto the approval package's wire-shape
// [approval.ActionApprovalPreviewField]. The two types are
// structurally identical; the conversion is the package boundary, not
// a transform. This test pins that boundary so a future drift between
// the structs (e.g. a renamed field) breaks here loudly rather than
// silently dropping data at the API surface.
func TestInputFieldsForAPI(t *testing.T) {
	in := []action.InputField{
		{Label: "To", Value: "alr@example.com"},
		{Label: "Subject", Missing: true},
		{Label: "Body", Value: "line one\nline two", Multiline: true},
	}
	got := inputFieldsForAPI(in)
	if len(got) != len(in) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(in))
	}
	for i, want := range in {
		if got[i].Label != want.Label {
			t.Errorf("[%d].Label = %q, want %q", i, got[i].Label, want.Label)
		}
		if got[i].Value != want.Value {
			t.Errorf("[%d].Value = %q, want %q", i, got[i].Value, want.Value)
		}
		if got[i].Missing != want.Missing {
			t.Errorf("[%d].Missing = %v, want %v", i, got[i].Missing, want.Missing)
		}
		if got[i].Multiline != want.Multiline {
			t.Errorf("[%d].Multiline = %v, want %v", i, got[i].Multiline, want.Multiline)
		}
	}
}

// TestInputFieldsForAPI_EmptyReturnsNil documents the adapter's
// nil-when-empty contract. The toPendingActionApproval path uses
// `len(a.InputFields) > 0` to decide whether to emit the wire field,
// so the adapter's contribution to the round-trip is to return nil
// for empty input — that keeps the API response omitting the field
// entirely and lets the webapp's fallback path render the historic
// raw-JSON accordion.
func TestInputFieldsForAPI_EmptyReturnsNil(t *testing.T) {
	if got := inputFieldsForAPI(nil); got != nil {
		t.Errorf("inputFieldsForAPI(nil) = %#v, want nil", got)
	}
	if got := inputFieldsForAPI([]action.InputField{}); got != nil {
		t.Errorf("inputFieldsForAPI([]) = %#v, want nil", got)
	}
}

// TestRunAction_ApprovalRequired_PopulatesInputFields is the end-to-
// end regression for the labeled-fields rendering: a manifest that
// declares `[[inputs]]` with display metadata causes RunAction to
// project the agent's call-time args through BuildInputFields and
// attach the result to the registered approval entry so the webapp's
// pending card renders labeled rows instead of a raw JSON dump.
//
// Without this assertion, the manifest amendment for `label` /
// `multiline` would compile and accept inputs without ever surfacing
// the projection at the approval surface — exactly the regression the
// patch is preventing.
func TestRunAction_ApprovalRequired_PopulatesInputFields(t *testing.T) {
	srv := newActionsTestServer(t, map[string]string{
		"send-email.md": actionsTestManifestApprovalWithInputs,
	})
	srv.actionApprovals = approval.NewActionApprovalQueue(nil, nil)
	srv.webappURL = "http://127.0.0.1:54321"

	body := bytes.NewReader([]byte(`{"args":{"to":"alr@example.com","subject":"Hi","body":"line one\nline two"}}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/send-email/run", body)
	rec := httptest.NewRecorder()
	srv.RunAction(rec, req, "send-email")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	pending := srv.actionApprovals.List()
	if len(pending) != 1 {
		t.Fatalf("pending entries = %d, want 1", len(pending))
	}
	entry := pending[0]
	if len(entry.InputFields) != 3 {
		t.Fatalf("InputFields len = %d, want 3 (To, Subject, Body); fields=%+v",
			len(entry.InputFields), entry.InputFields)
	}
	// Declaration order: To, Subject, Body.
	if entry.InputFields[0].Label != "To" || entry.InputFields[0].Value != "alr@example.com" {
		t.Errorf("[0] = %+v, want {To, alr@example.com}", entry.InputFields[0])
	}
	if entry.InputFields[1].Label != "Subject" || entry.InputFields[1].Value != "Hi" {
		t.Errorf("[1] = %+v, want {Subject, Hi}", entry.InputFields[1])
	}
	// Multiline body preserves embedded newlines verbatim — the
	// rendered surface (whitespace-pre-wrap) is what draws them as
	// real lines; the projection just hands the string through.
	if entry.InputFields[2].Label != "Body" {
		t.Errorf("[2].Label = %q, want Body", entry.InputFields[2].Label)
	}
	if entry.InputFields[2].Value != "line one\nline two" {
		t.Errorf("[2].Value = %q, want literal-newline string", entry.InputFields[2].Value)
	}
	if !entry.InputFields[2].Multiline {
		t.Errorf("[2].Multiline = false, want true (manifest declared multiline=true)")
	}
}

// actionsTestManifestApprovalWithInputs is a minimal approval-gated
// manifest declaring `[[inputs]]` with `label` and `multiline` so the
// RunAction approval-gate path exercises the input_fields projection
// end-to-end. Mirrors the shape an updated `send-email` connector
// manifest will land in.
const actionsTestManifestApprovalWithInputs = `+++
name = "send-email"
version = "1.0.0"
source = "hub://aileron/send-email@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc123"
capabilities = ["gmail:send"]

[match]
intent = "send an email"

[[inputs]]
name = "to"
type = "string"
description = "Recipient address."
label = "To"

[[inputs]]
name = "subject"
type = "string"
description = "Subject line."
label = "Subject"

[[inputs]]
name = "body"
type = "string"
description = "Plain-text body."
label = "Body"
multiline = true

[[execute]]
id = "send"
connector = "github://aileron/google"
op = "send_email"

[approval]
required = true
+++

# Send Email
`
