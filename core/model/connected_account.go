package model

import "time"

// ConnectedAccountProvider identifies the external service provider.
type ConnectedAccountProvider string

const (
	ConnectedAccountProviderGmail          ConnectedAccountProvider = "gmail"
	ConnectedAccountProviderGoogleCalendar ConnectedAccountProvider = "google_calendar"
	ConnectedAccountProviderOutlook        ConnectedAccountProvider = "outlook"
	ConnectedAccountProviderMicrosoftCalendar ConnectedAccountProvider = "microsoft_calendar"
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
	ID        string                   // conn_ + UUID
	UserID    string                   // owning user (usr_ + UUID)
	Provider  ConnectedAccountProvider // which external service
	Email     string                   // email associated with the external account
	Scopes    []string                 // OAuth scopes granted
	Status    ConnectedAccountStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// VaultPath returns the vault key where the OAuth refresh token is stored.
func (ca ConnectedAccount) VaultPath() string {
	return "connected-accounts/" + ca.UserID + "/" + string(ca.Provider)
}
