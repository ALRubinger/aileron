package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/auth"
	connectorpkg "github.com/ALRubinger/aileron/internal/connector"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/store/mem"
	"github.com/ALRubinger/aileron/internal/vault"
)

// setTestUserClaims returns a context with fake auth claims for testing.
func setTestUserClaims(ctx context.Context, userID string) context.Context {
	claims := &auth.Claims{}
	claims.Subject = userID
	return auth.ContextWithClaims(ctx, claims)
}

func TestResolveConnector(t *testing.T) {
	tests := []struct {
		actionType   string
		wantType     string
		wantProvider string
	}{
		{"email.send", "email", "gmail"},
		{"email.draft", "email", "gmail"},
		{"git.pull_request.create", "git", "github"},
		{"git.issue.create", "git", "github"},
		{"git.issue.comment", "git", "github"},
		{"payment.charge", "payments", "stripe"},
		{"calendar.event.create", "calendar", "google_calendar"},
		{"calendar.event.update", "calendar", "google_calendar"},
		{"calendar.event.delete", "calendar", "google_calendar"},
		{"unknown.action", "", ""},
	}
	for _, tt := range tests {
		connType, provider := resolveConnector(tt.actionType)
		if connType != tt.wantType || provider != tt.wantProvider {
			t.Errorf("resolveConnector(%q) = (%q, %q), want (%q, %q)",
				tt.actionType, connType, provider, tt.wantType, tt.wantProvider)
		}
	}
}

func TestBuildConnectorParams_Email(t *testing.T) {
	subject := "Quarterly Report"
	bodyText := "See attached."
	bodyHTML := "<p>See attached.</p>"
	sendMode := api.SendNow
	threadRef := "thread123"
	to := []api.Recipient{{Email: "alice@example.com"}}
	cc := []api.Recipient{{Email: "bob@example.com"}}
	bcc := []api.Recipient{{Email: "audit@example.com"}}

	params := buildConnectorParams(api.ActionIntent{
		Type:    "email.send",
		Summary: "Send report",
		Domain: &api.DomainAction{
			Email: &api.EmailAction{
				Subject:   &subject,
				BodyText:  &bodyText,
				BodyHtml:  &bodyHTML,
				SendMode:  &sendMode,
				ThreadRef: &threadRef,
				To:        &to,
				Cc:        &cc,
				Bcc:       &bcc,
			},
		},
	})

	if params["subject"] != "Quarterly Report" {
		t.Errorf("subject = %v", params["subject"])
	}
	if params["body_text"] != "See attached." {
		t.Errorf("body_text = %v", params["body_text"])
	}
	if params["body_html"] != "<p>See attached.</p>" {
		t.Errorf("body_html = %v", params["body_html"])
	}
	if params["send_mode"] != "send_now" {
		t.Errorf("send_mode = %v", params["send_mode"])
	}
	if params["thread_ref"] != "thread123" {
		t.Errorf("thread_ref = %v", params["thread_ref"])
	}
	if params["to"] == nil {
		t.Error("to should not be nil")
	}
	if params["cc"] == nil {
		t.Error("cc should not be nil")
	}
	if params["bcc"] == nil {
		t.Error("bcc should not be nil")
	}
}

func TestBuildConnectorParams_Calendar(t *testing.T) {
	title := "Standup"
	desc := "Daily sync"
	startTime := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	endTime := time.Date(2026, 4, 25, 10, 30, 0, 0, time.UTC)
	tz := "America/New_York"
	loc := "Room B"
	calID := "team@group.calendar.google.com"
	confType := api.CalendarActionConferenceTypeGoogleMeet
	vis := api.Private
	attendees := []api.CalendarAttendee{{Email: "alice@example.com"}}

	params := buildConnectorParams(api.ActionIntent{
		Type:    "calendar.event.create",
		Summary: "Create standup",
		Domain: &api.DomainAction{
			Calendar: &api.CalendarAction{
				Title:          &title,
				Description:    &desc,
				StartTime:      &startTime,
				EndTime:        &endTime,
				Timezone:       &tz,
				Location:       &loc,
				CalendarId:     &calID,
				ConferenceType: &confType,
				Visibility:     &vis,
				Attendees:      &attendees,
			},
		},
	})

	if params["title"] != "Standup" {
		t.Errorf("title = %v", params["title"])
	}
	if params["description"] != "Daily sync" {
		t.Errorf("description = %v", params["description"])
	}
	if params["timezone"] != "America/New_York" {
		t.Errorf("timezone = %v", params["timezone"])
	}
	if params["location"] != "Room B" {
		t.Errorf("location = %v", params["location"])
	}
	if params["calendar_id"] != "team@group.calendar.google.com" {
		t.Errorf("calendar_id = %v", params["calendar_id"])
	}
	if params["conference_type"] != "google_meet" {
		t.Errorf("conference_type = %v", params["conference_type"])
	}
	if params["visibility"] != "private" {
		t.Errorf("visibility = %v", params["visibility"])
	}
	if params["start_time"] == nil {
		t.Error("start_time should not be nil")
	}
	if params["end_time"] == nil {
		t.Error("end_time should not be nil")
	}
	if params["attendees"] == nil {
		t.Error("attendees should not be nil")
	}
}

func TestBuildConnectorParams_GitIssue(t *testing.T) {
	repo := "acme/app"
	issueTitle := "Bug: login fails"
	issueBody := "Steps to reproduce..."
	issueLabels := []string{"bug", "priority-high"}
	issueAssignees := []string{"alice"}

	params := buildConnectorParams(api.ActionIntent{
		Type:    "git.issue.create",
		Summary: "Create issue",
		Domain: &api.DomainAction{
			Git: &api.GitAction{
				Repository:     &repo,
				IssueTitle:     &issueTitle,
				IssueBody:      &issueBody,
				IssueLabels:    &issueLabels,
				IssueAssignees: &issueAssignees,
			},
		},
	})

	if params["repository"] != "acme/app" {
		t.Errorf("repository = %v", params["repository"])
	}
	if params["issue_title"] != "Bug: login fails" {
		t.Errorf("issue_title = %v", params["issue_title"])
	}
	if params["issue_body"] != "Steps to reproduce..." {
		t.Errorf("issue_body = %v", params["issue_body"])
	}
	if params["issue_labels"] == nil {
		t.Error("issue_labels should not be nil")
	}
	if params["issue_assignees"] == nil {
		t.Error("issue_assignees should not be nil")
	}
}

func TestBuildConnectorParams_NilDomain(t *testing.T) {
	params := buildConnectorParams(api.ActionIntent{
		Type:    "unknown",
		Summary: "test",
	})
	if params["action_type"] != "unknown" {
		t.Errorf("action_type = %v", params["action_type"])
	}
	if params["summary"] != "test" {
		t.Errorf("summary = %v", params["summary"])
	}
}

func TestApiActionToModel_Email(t *testing.T) {
	subject := "Hello"
	bodyText := "Hi there"
	bodyHTML := "<p>Hi there</p>"
	sendMode := api.SendNow
	threadRef := "thread123"
	toName := "Alice"
	to := []api.Recipient{{Email: "alice@example.com", Name: &toName}}
	ccName := "Bob"
	cc := []api.Recipient{{Email: "bob@example.com", Name: &ccName}}
	bcc := []api.Recipient{{Email: "audit@example.com"}}

	m := apiActionToModel(api.ActionIntent{
		Type:    "email.send",
		Summary: "Send email",
		Domain: &api.DomainAction{
			Email: &api.EmailAction{
				Subject:   &subject,
				BodyText:  &bodyText,
				BodyHtml:  &bodyHTML,
				SendMode:  &sendMode,
				ThreadRef: &threadRef,
				To:        &to,
				Cc:        &cc,
				Bcc:       &bcc,
			},
		},
	})

	if m.Domain.Email == nil {
		t.Fatal("expected Email domain")
	}
	e := m.Domain.Email
	if e.Subject != "Hello" {
		t.Errorf("Subject = %q", e.Subject)
	}
	if e.BodyText != "Hi there" {
		t.Errorf("BodyText = %q", e.BodyText)
	}
	if e.BodyHTML != "<p>Hi there</p>" {
		t.Errorf("BodyHTML = %q", e.BodyHTML)
	}
	if e.SendMode != "send_now" {
		t.Errorf("SendMode = %q", e.SendMode)
	}
	if e.ThreadRef != "thread123" {
		t.Errorf("ThreadRef = %q", e.ThreadRef)
	}
	if len(e.To) != 1 || e.To[0].Email != "alice@example.com" || e.To[0].Name != "Alice" {
		t.Errorf("To = %+v", e.To)
	}
	if len(e.CC) != 1 || e.CC[0].Email != "bob@example.com" || e.CC[0].Name != "Bob" {
		t.Errorf("CC = %+v", e.CC)
	}
	if len(e.BCC) != 1 || e.BCC[0].Email != "audit@example.com" {
		t.Errorf("BCC = %+v", e.BCC)
	}
}

func TestApiActionToModel_Calendar(t *testing.T) {
	title := "Standup"
	desc := "Daily sync"
	provider := api.CalendarActionProviderGoogleCalendar
	tz := "America/New_York"
	loc := "Room B"
	confType := api.CalendarActionConferenceTypeGoogleMeet
	calID := "primary"
	vis := api.Private
	startTime := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	endTime := time.Date(2026, 4, 25, 10, 30, 0, 0, time.UTC)
	aName := "Alice"
	optional := true
	attendees := []api.CalendarAttendee{
		{Email: "alice@example.com", Name: &aName, Optional: &optional},
	}

	m := apiActionToModel(api.ActionIntent{
		Type:    "calendar.event.create",
		Summary: "Create standup",
		Domain: &api.DomainAction{
			Calendar: &api.CalendarAction{
				Title:          &title,
				Description:    &desc,
				Provider:       &provider,
				Timezone:       &tz,
				Location:       &loc,
				ConferenceType: &confType,
				CalendarId:     &calID,
				Visibility:     &vis,
				StartTime:      &startTime,
				EndTime:        &endTime,
				Attendees:      &attendees,
			},
		},
	})

	if m.Domain.Calendar == nil {
		t.Fatal("expected Calendar domain")
	}
	c := m.Domain.Calendar
	if c.Title != "Standup" {
		t.Errorf("Title = %q", c.Title)
	}
	if c.Description != "Daily sync" {
		t.Errorf("Description = %q", c.Description)
	}
	if c.Provider != "google_calendar" {
		t.Errorf("Provider = %q", c.Provider)
	}
	if c.Timezone != "America/New_York" {
		t.Errorf("Timezone = %q", c.Timezone)
	}
	if c.Location != "Room B" {
		t.Errorf("Location = %q", c.Location)
	}
	if c.ConferenceType != "google_meet" {
		t.Errorf("ConferenceType = %q", c.ConferenceType)
	}
	if c.CalendarID != "primary" {
		t.Errorf("CalendarID = %q", c.CalendarID)
	}
	if c.Visibility != "private" {
		t.Errorf("Visibility = %q", c.Visibility)
	}
	if c.StartTime == nil || !c.StartTime.Equal(startTime) {
		t.Errorf("StartTime = %v", c.StartTime)
	}
	if c.EndTime == nil || !c.EndTime.Equal(endTime) {
		t.Errorf("EndTime = %v", c.EndTime)
	}
	if len(c.Attendees) != 1 {
		t.Fatalf("Attendees count = %d", len(c.Attendees))
	}
	if c.Attendees[0].Email != "alice@example.com" {
		t.Errorf("Attendee email = %q", c.Attendees[0].Email)
	}
	if c.Attendees[0].Name != "Alice" {
		t.Errorf("Attendee name = %q", c.Attendees[0].Name)
	}
	if !c.Attendees[0].Optional {
		t.Error("Attendee should be optional")
	}
}

func TestApiActionToModel_GitIssue(t *testing.T) {
	issueTitle := "Bug report"
	issueBody := "Details..."
	issueLabels := []string{"bug"}
	issueAssignees := []string{"alice"}

	m := apiActionToModel(api.ActionIntent{
		Type:    "git.issue.create",
		Summary: "Create issue",
		Domain: &api.DomainAction{
			Git: &api.GitAction{
				IssueTitle:     &issueTitle,
				IssueBody:      &issueBody,
				IssueLabels:    &issueLabels,
				IssueAssignees: &issueAssignees,
			},
		},
	})

	if m.Domain.Git == nil {
		t.Fatal("expected Git domain")
	}
	g := m.Domain.Git
	if g.IssueTitle != "Bug report" {
		t.Errorf("IssueTitle = %q", g.IssueTitle)
	}
	if g.IssueBody != "Details..." {
		t.Errorf("IssueBody = %q", g.IssueBody)
	}
	if len(g.IssueLabels) != 1 || g.IssueLabels[0] != "bug" {
		t.Errorf("IssueLabels = %v", g.IssueLabels)
	}
	if len(g.IssueAssignees) != 1 || g.IssueAssignees[0] != "alice" {
		t.Errorf("IssueAssignees = %v", g.IssueAssignees)
	}
}

func TestResolveCredentialVaultPath_ConnectedAccount(t *testing.T) {
	accounts := mem.NewConnectedAccountStore()
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_1",
		UserID:   "usr_test",
		Provider: model.ConnectedAccountProviderGmail,
		Status:   model.ConnectedAccountStatusActive,
	})

	s := &apiServer{
		log:               slog.Default(),
		connectedAccounts: accounts,
	}

	// Simulate authenticated context with user claims.
	ctx = setTestUserClaims(ctx, "usr_test")
	path := s.resolveCredentialVaultPath(ctx, "gmail")
	if path != "connected-accounts/usr_test/gmail" {
		t.Errorf("vaultPath = %q, want connected-accounts/usr_test/gmail", path)
	}
}

func TestResolveCredentialVaultPath_FallbackToInfra(t *testing.T) {
	accounts := mem.NewConnectedAccountStore()

	s := &apiServer{
		log:               slog.Default(),
		connectedAccounts: accounts,
	}

	// No connected account — should fall back to infra path.
	ctx := setTestUserClaims(context.Background(), "usr_test")
	path := s.resolveCredentialVaultPath(ctx, "gmail")
	if path != "connectors/gmail/default" {
		t.Errorf("vaultPath = %q, want connectors/gmail/default", path)
	}
}

func TestResolveCredentialVaultPath_NoAuthContext(t *testing.T) {
	s := &apiServer{
		log:               slog.Default(),
		connectedAccounts: mem.NewConnectedAccountStore(),
	}

	// No auth claims — should fall back to infra path.
	path := s.resolveCredentialVaultPath(context.Background(), "gmail")
	if path != "connectors/gmail/default" {
		t.Errorf("vaultPath = %q, want connectors/gmail/default", path)
	}
}

func TestResolveCredentialVaultPath_InactiveAccount(t *testing.T) {
	accounts := mem.NewConnectedAccountStore()
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID:       "conn_1",
		UserID:   "usr_test",
		Provider: model.ConnectedAccountProviderGmail,
		Status:   model.ConnectedAccountStatusExpired,
	})

	s := &apiServer{
		log:               slog.Default(),
		connectedAccounts: accounts,
	}

	ctx = setTestUserClaims(ctx, "usr_test")
	path := s.resolveCredentialVaultPath(ctx, "gmail")
	// Expired account should not be used.
	if path != "connectors/gmail/default" {
		t.Errorf("vaultPath = %q, want connectors/gmail/default (expired account)", path)
	}
}

func TestResolveCredentialVaultPath_UnknownProvider(t *testing.T) {
	s := &apiServer{
		log:               slog.Default(),
		connectedAccounts: mem.NewConnectedAccountStore(),
	}

	ctx := setTestUserClaims(context.Background(), "usr_test")
	path := s.resolveCredentialVaultPath(ctx, "stripe")
	// Stripe has no connected account provider mapping.
	if path != "connectors/stripe/default" {
		t.Errorf("vaultPath = %q, want connectors/stripe/default", path)
	}
}

// --- Health tests ---

func TestGetHealth_OK(t *testing.T) {
	s := &apiServer{log: slog.Default()}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	s.GetHealth(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// --- executeGrant tests ---

func TestExecuteGrant_GrantNotFound(t *testing.T) {
	s := newExecutionServer()
	_, err := s.executeGrant(context.Background(), "nonexistent", "usr_test")
	if err == nil {
		t.Fatal("expected error for missing grant")
	}
}

func TestExecuteGrant_ExpiredGrant(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()

	s.intents.Create(ctx, api.IntentEnvelope{
		IntentId: "int_exp", WorkspaceId: "ws_1", Status: api.IntentStatusApproved,
		Action: api.ActionIntent{Type: "git.pull_request.create", Summary: "test"},
	})
	s.grants.Create(ctx, api.ExecutionGrant{
		GrantId:   "grt_exp",
		IntentId:  "int_exp",
		Status:    api.ExecutionGrantStatusActive,
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	})

	_, err := s.executeGrant(ctx, "grt_exp", "usr_test")
	if err == nil {
		t.Fatal("expected error for expired grant")
	}
}

func TestExecuteGrant_NoConnector(t *testing.T) {
	s := newExecutionServer()
	ctx := context.Background()
	seedGrantAndIntent(ctx, s, "grt_nc", "int_nc")

	result, err := s.executeGrant(ctx, "grt_nc", "usr_test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != api.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestExecuteGrant_SuccessWithConnector(t *testing.T) {
	s := newExecutionServer()
	enableVaultEncryption(s, "usr_exec")
	ctx := context.Background()

	conn := &stubConnector{
		result: connectorpkg.ExecutionResult{
			Status:     connectorpkg.ExecutionStatusSucceeded,
			ReceiptRef: "https://github.com/acme/app/pull/42",
			Output:     map[string]any{"pr_url": "https://github.com/acme/app/pull/42"},
		},
	}
	s.registry.Register(ctx, conn)
	storeEncryptedToken(s, "connectors/github/default", []byte("token"))
	seedGrantAndIntent(ctx, s, "grt_ok", "int_ok")

	result, err := s.executeGrant(ctx, "grt_ok", "usr_exec")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != api.ExecutionStatusSucceeded {
		t.Errorf("Status = %q, want succeeded; Error: %s", result.Status, result.Error)
	}
	if result.ReceiptRef != "https://github.com/acme/app/pull/42" {
		t.Errorf("ReceiptRef = %q", result.ReceiptRef)
	}
}

func TestExecuteGrant_ConnectorFails(t *testing.T) {
	s := newExecutionServer()
	enableVaultEncryption(s, "usr_exec")
	ctx := context.Background()

	conn := &stubConnector{
		result: connectorpkg.ExecutionResult{
			Status: connectorpkg.ExecutionStatusFailed,
			Error:  "API error 422",
		},
	}
	s.registry.Register(ctx, conn)
	storeEncryptedToken(s, "connectors/github/default", []byte("token"))
	seedGrantAndIntent(ctx, s, "grt_fail", "int_fail")

	result, err := s.executeGrant(ctx, "grt_fail", "usr_exec")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != api.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestExecuteGrant_CredentialNotFound(t *testing.T) {
	s := newExecutionServer()
	enableVaultEncryption(s, "usr_nocred")
	ctx := context.Background()

	conn := &stubConnector{}
	s.registry.Register(ctx, conn)

	// Seed grant and intent WITHOUT a vault credential.
	s.intents.Create(ctx, api.IntentEnvelope{
		IntentId: "int_nocred", WorkspaceId: "ws_1", Status: api.IntentStatusApproved,
		Action:    api.ActionIntent{Type: "git.push", Summary: "push"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	s.grants.Create(ctx, api.ExecutionGrant{
		GrantId: "grt_nocred", IntentId: "int_nocred",
		Status: api.ExecutionGrantStatusActive, ExpiresAt: time.Now().Add(time.Hour),
	})

	result, err := s.executeGrant(ctx, "grt_nocred", "usr_nocred")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != api.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed (no credential in vault)", result.Status)
	}
}

func TestExecuteGrant_VaultLocked(t *testing.T) {
	s := newExecutionServer()
	// kekSessionCache is nil → vault locked
	ctx := context.Background()

	conn := &stubConnector{}
	s.registry.Register(ctx, conn)
	s.vault.Put(ctx, "connectors/github/default", []byte("token"), vault.Metadata{})
	seedGrantAndIntent(ctx, s, "grt_locked", "int_locked")

	result, err := s.executeGrant(ctx, "grt_locked", "usr_test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != api.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
	if result.Error != "vault locked" {
		t.Errorf("Error = %q, want 'vault locked'", result.Error)
	}
}

func TestBuildModelDomainParams_Email(t *testing.T) {
	domain := model.DomainAction{
		Email: &model.EmailAction{
			To:       []model.Recipient{{Name: "Alice", Email: "alice@example.com"}},
			CC:       []model.Recipient{{Email: "bob@example.com"}},
			Subject:  "Report",
			BodyText: "Here it is.",
			SendMode: "send_now",
		},
	}
	params := buildModelDomainParams("email.send", domain)
	if params["subject"] != "Report" {
		t.Errorf("subject = %v", params["subject"])
	}
	if params["body_text"] != "Here it is." {
		t.Errorf("body_text = %v", params["body_text"])
	}
	if params["send_mode"] != "send_now" {
		t.Errorf("send_mode = %v", params["send_mode"])
	}
	if params["to"] == nil {
		t.Error("to should not be nil")
	}
}

func TestBuildModelDomainParams_Calendar(t *testing.T) {
	start := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 25, 10, 30, 0, 0, time.UTC)
	domain := model.DomainAction{
		Calendar: &model.CalendarAction{
			Title:     "Standup",
			StartTime: &start,
			EndTime:   &end,
			Timezone:  "America/New_York",
			Location:  "Room B",
			Attendees: []model.CalendarAttendee{{Email: "alice@example.com", Name: "Alice"}},
		},
	}
	params := buildModelDomainParams("calendar.event.create", domain)
	if params["title"] != "Standup" {
		t.Errorf("title = %v", params["title"])
	}
	if params["timezone"] != "America/New_York" {
		t.Errorf("timezone = %v", params["timezone"])
	}
	if params["location"] != "Room B" {
		t.Errorf("location = %v", params["location"])
	}
	if params["start_time"] == nil {
		t.Error("start_time should not be nil")
	}
	if params["attendees"] == nil {
		t.Error("attendees should not be nil")
	}
}

func TestBuildModelDomainParams_GitIssue(t *testing.T) {
	domain := model.DomainAction{
		Git: &model.GitAction{
			Repository:     "acme/app",
			IssueTitle:     "Bug report",
			IssueBody:      "Details...",
			IssueLabels:    []string{"bug"},
			IssueAssignees: []string{"alice"},
		},
	}
	params := buildModelDomainParams("git.issue.create", domain)
	if params["repository"] != "acme/app" {
		t.Errorf("repository = %v", params["repository"])
	}
	if params["issue_title"] != "Bug report" {
		t.Errorf("issue_title = %v", params["issue_title"])
	}
	if params["issue_labels"] == nil {
		t.Error("issue_labels should not be nil")
	}
}

func TestBuildModelDomainParams_EmptyDomain(t *testing.T) {
	params := buildModelDomainParams("unknown.action", model.DomainAction{})
	if params["action_type"] != "unknown.action" {
		t.Errorf("action_type = %v", params["action_type"])
	}
}

func TestExecuteGrant_ConnectorReturnsError(t *testing.T) {
	s := newExecutionServer()
	enableVaultEncryption(s, "usr_exec")
	ctx := context.Background()

	conn := &stubConnector{err: fmt.Errorf("connection refused")}
	s.registry.Register(ctx, conn)
	storeEncryptedToken(s, "connectors/github/default", []byte("token"))
	seedGrantAndIntent(ctx, s, "grt_cerr", "int_cerr")

	result, err := s.executeGrant(ctx, "grt_cerr", "usr_exec")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != api.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Error, "connection refused") {
		t.Errorf("Error = %q, want connection refused", result.Error)
	}
}

func TestExecuteGrant_DecryptionFailure(t *testing.T) {
	s := newExecutionServer()
	enableVaultEncryption(s, "usr_dec")
	ctx := context.Background()

	conn := &stubConnector{result: connectorpkg.ExecutionResult{Status: connectorpkg.ExecutionStatusSucceeded}}
	s.registry.Register(ctx, conn)
	seedGrantAndIntent(ctx, s, "grt_dec", "int_dec")
	// Overwrite the valid credential with garbage → decryption fails.
	s.vault.Put(ctx, "connectors/github/default", []byte("not-encrypted-garbage"), vault.Metadata{
		Type:   "api_key",
		Labels: map[string]string{vault.EncryptedLabel: "true"},
	})

	result, err := s.executeGrant(ctx, "grt_dec", "usr_dec")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != api.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Error, "credential decryption failed") {
		t.Errorf("Error = %q, want credential decryption failed", result.Error)
	}
}

func TestResolveCredentialVaultPath_GoogleCalendar(t *testing.T) {
	accounts := mem.NewConnectedAccountStore()
	ctx := context.Background()

	accounts.Create(ctx, model.ConnectedAccount{
		ID: "conn_cal", UserID: "usr_cal",
		Provider: model.ConnectedAccountProviderGoogleCalendar,
		Status:   model.ConnectedAccountStatusActive,
	})

	s := &apiServer{log: slog.Default(), connectedAccounts: accounts}
	ctx = setTestUserClaims(ctx, "usr_cal")
	path := s.resolveCredentialVaultPath(ctx, "google_calendar")
	if path != "connected-accounts/usr_cal/google_calendar" {
		t.Errorf("path = %q, want connected-accounts/usr_cal/google_calendar", path)
	}
}
