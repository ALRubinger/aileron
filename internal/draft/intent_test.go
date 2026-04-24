package draft

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/model"
)

func TestParseIntentResponse_Informational(t *testing.T) {
	intent, err := parseIntentResponse(`{"type": "informational"}`)
	if err != nil {
		t.Fatalf("parseIntentResponse: %v", err)
	}
	if intent.Type != IntentTypeInformational {
		t.Errorf("Type = %q, want informational", intent.Type)
	}
	if intent.IsWriteAction() {
		t.Error("informational should not be a write action")
	}
}

func TestParseIntentResponse_EmailSend(t *testing.T) {
	raw := `{
		"type": "email.send",
		"to": [{"name": "Alice", "email": "alice@example.com"}],
		"cc": [{"email": "bob@example.com"}],
		"subject": "Q4 Report",
		"body_text": "Please find the report attached.",
		"send_mode": "send_now"
	}`
	intent, err := parseIntentResponse(raw)
	if err != nil {
		t.Fatalf("parseIntentResponse: %v", err)
	}
	if intent.Type != IntentTypeEmailSend {
		t.Errorf("Type = %q, want email.send", intent.Type)
	}
	if !intent.IsWriteAction() {
		t.Error("email.send should be a write action")
	}
	if intent.Action == nil {
		t.Fatal("Action should not be nil")
	}
	if intent.Action.Domain.Email == nil {
		t.Fatal("Email domain should not be nil")
	}
	e := intent.Action.Domain.Email
	if len(e.To) != 1 || e.To[0].Email != "alice@example.com" {
		t.Errorf("To = %+v", e.To)
	}
	if e.Subject != "Q4 Report" {
		t.Errorf("Subject = %q", e.Subject)
	}
	if e.BodyText != "Please find the report attached." {
		t.Errorf("BodyText = %q", e.BodyText)
	}
	if e.SendMode != "send_now" {
		t.Errorf("SendMode = %q", e.SendMode)
	}
}

func TestParseIntentResponse_EmailDraft(t *testing.T) {
	raw := `{"type": "email.draft", "to": [{"email": "test@example.com"}], "subject": "Draft", "body_text": "WIP"}`
	intent, err := parseIntentResponse(raw)
	if err != nil {
		t.Fatalf("parseIntentResponse: %v", err)
	}
	if intent.Type != IntentTypeEmailDraft {
		t.Errorf("Type = %q", intent.Type)
	}
	if intent.Action.Domain.Email.SendMode != "draft_only" {
		t.Errorf("SendMode = %q, want draft_only", intent.Action.Domain.Email.SendMode)
	}
}

func TestParseIntentResponse_CalendarEvent(t *testing.T) {
	raw := `{
		"type": "calendar.event.create",
		"title": "Team Standup",
		"description": "Daily sync",
		"start_time": "2026-04-25T10:00:00",
		"end_time": "2026-04-25T10:30:00",
		"timezone": "America/New_York",
		"location": "Room B",
		"attendees": [{"name": "Alice", "email": "alice@example.com"}],
		"conference_type": "google_meet"
	}`
	intent, err := parseIntentResponse(raw)
	if err != nil {
		t.Fatalf("parseIntentResponse: %v", err)
	}
	if intent.Type != IntentTypeCalendarEventCreate {
		t.Errorf("Type = %q", intent.Type)
	}
	c := intent.Action.Domain.Calendar
	if c == nil {
		t.Fatal("Calendar domain should not be nil")
	}
	if c.Title != "Team Standup" {
		t.Errorf("Title = %q", c.Title)
	}
	if c.StartTime == nil {
		t.Error("StartTime should not be nil")
	}
	if c.EndTime == nil {
		t.Error("EndTime should not be nil")
	}
	if len(c.Attendees) != 1 {
		t.Errorf("Attendees count = %d", len(c.Attendees))
	}
	if c.ConferenceType != "google_meet" {
		t.Errorf("ConferenceType = %q", c.ConferenceType)
	}
}

func TestParseIntentResponse_GitIssue(t *testing.T) {
	raw := `{
		"type": "git.issue.create",
		"repository": "acme/app",
		"issue_title": "Bug: login fails",
		"issue_body": "Steps to reproduce...",
		"issue_labels": ["bug", "priority-high"],
		"issue_assignees": ["alice"]
	}`
	intent, err := parseIntentResponse(raw)
	if err != nil {
		t.Fatalf("parseIntentResponse: %v", err)
	}
	if intent.Type != IntentTypeGitIssueCreate {
		t.Errorf("Type = %q", intent.Type)
	}
	g := intent.Action.Domain.Git
	if g == nil {
		t.Fatal("Git domain should not be nil")
	}
	if g.Repository != "acme/app" {
		t.Errorf("Repository = %q", g.Repository)
	}
	if g.IssueTitle != "Bug: login fails" {
		t.Errorf("IssueTitle = %q", g.IssueTitle)
	}
	if len(g.IssueLabels) != 2 {
		t.Errorf("IssueLabels = %v", g.IssueLabels)
	}
}

func TestParseIntentResponse_GitIssueComment(t *testing.T) {
	raw := `{"type": "git.issue.comment", "repository": "acme/app", "issue_number": "42", "body": "Fixed in PR #43"}`
	intent, err := parseIntentResponse(raw)
	if err != nil {
		t.Fatalf("parseIntentResponse: %v", err)
	}
	if intent.Type != IntentTypeGitIssueComment {
		t.Errorf("Type = %q", intent.Type)
	}
	if intent.Action.Metadata["issue_number"] != "42" {
		t.Errorf("issue_number = %v", intent.Action.Metadata["issue_number"])
	}
}

func TestParseIntentResponse_SlackSend(t *testing.T) {
	raw := `{"type": "comms.slack.send", "channel": "C123", "body": "Hello team!"}`
	intent, err := parseIntentResponse(raw)
	if err != nil {
		t.Fatalf("parseIntentResponse: %v", err)
	}
	if intent.Type != IntentTypeSlackSend {
		t.Errorf("Type = %q", intent.Type)
	}
	if intent.Action.Metadata["body"] != "Hello team!" {
		t.Errorf("body = %v", intent.Action.Metadata["body"])
	}
}

func TestParseIntentResponse_InvalidJSON(t *testing.T) {
	_, err := parseIntentResponse("not json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseIntentResponse_UnknownType(t *testing.T) {
	_, err := parseIntentResponse(`{"type": "unknown.action"}`)
	if err == nil {
		t.Error("expected error for unknown type")
	}
}

func TestParseIntentResponse_MarkdownFences(t *testing.T) {
	// LLMs sometimes wrap JSON in markdown code fences.
	raw := "```json\n{\"type\": \"informational\"}\n```"
	intent, err := parseIntentResponse(raw)
	if err != nil {
		t.Fatalf("parseIntentResponse: %v", err)
	}
	if intent.Type != IntentTypeInformational {
		t.Errorf("Type = %q, want informational", intent.Type)
	}
}

func TestIsWriteAction(t *testing.T) {
	tests := []struct {
		intentType IntentType
		want       bool
	}{
		{IntentTypeInformational, false},
		{IntentTypeEmailSend, true},
		{IntentTypeEmailDraft, true},
		{IntentTypeCalendarEventCreate, true},
		{IntentTypeGitIssueCreate, true},
		{IntentTypeGitIssueComment, true},
		{IntentTypeSlackSend, true},
	}
	for _, tt := range tests {
		ri := ResolvedIntent{Type: tt.intentType}
		if ri.IsWriteAction() != tt.want {
			t.Errorf("IsWriteAction(%q) = %v, want %v", tt.intentType, ri.IsWriteAction(), tt.want)
		}
	}
}

func TestFormatRecipientNames(t *testing.T) {
	tests := []struct {
		name       string
		recipients []model.Recipient
		want       string
	}{
		{"empty", nil, "(no recipients)"},
		{"name only", []model.Recipient{{Name: "Alice", Email: "a@b.com"}}, "Alice"},
		{"email only", []model.Recipient{{Email: "a@b.com"}}, "a@b.com"},
		{"multiple", []model.Recipient{{Name: "Alice"}, {Email: "bob@b.com"}}, "Alice, bob@b.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatRecipientNames(tt.recipients)
			if got != tt.want {
				t.Errorf("formatRecipientNames = %q, want %q", got, tt.want)
			}
		})
	}
}
