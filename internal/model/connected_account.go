package model

import "time"

// ConnectedAccountProvider identifies the external service provider.
type ConnectedAccountProvider string

const (
	ConnectedAccountProviderGmail             ConnectedAccountProvider = "gmail"
	ConnectedAccountProviderGoogleCalendar    ConnectedAccountProvider = "google_calendar"
	ConnectedAccountProviderOutlook           ConnectedAccountProvider = "outlook"
	ConnectedAccountProviderMicrosoftCalendar ConnectedAccountProvider = "microsoft_calendar"
	ConnectedAccountProviderSlack             ConnectedAccountProvider = "slack"
	ConnectedAccountProviderGitHub            ConnectedAccountProvider = "github_repos"
)

// ConnectedAccountStatus tracks the lifecycle state of a connected account.
type ConnectedAccountStatus string

const (
	ConnectedAccountStatusActive  ConnectedAccountStatus = "active"
	ConnectedAccountStatusExpired ConnectedAccountStatus = "expired"
	ConnectedAccountStatusRevoked ConnectedAccountStatus = "revoked"
)

// ConnectedAccount represents a user's linked external service account.
// Aileron uses this to execute irreversible actions (send email, create
// calendar events, issue payments) on behalf of the user. The OAuth
// refresh token is stored in the vault at a well-known path; this record
// holds metadata only.
type ConnectedAccount struct {
	ID             string                   `json:"id"`               // conn_ + UUID
	UserID         string                   `json:"user_id"`          // owning user (usr_ + UUID)
	Provider       ConnectedAccountProvider `json:"provider"`         // which external service
	Scopes         []string                 `json:"scopes"`           // OAuth scopes granted
	Status         ConnectedAccountStatus   `json:"status"`
	ExternalUserID string                   `json:"external_user_id"` // provider-specific user ID (e.g. Slack U...)
	ExternalTeamID string                   `json:"external_team_id"` // provider-specific workspace/team ID (e.g. Slack T...)
	CreatedAt      time.Time                `json:"created_at"`
	UpdatedAt      time.Time                `json:"updated_at"`
}

// VaultPath returns the vault key where the OAuth refresh token is stored.
func (ca ConnectedAccount) VaultPath() string {
	return "connected-accounts/" + ca.UserID + "/" + string(ca.Provider)
}
