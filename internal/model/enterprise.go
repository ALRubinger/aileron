// Package model — Enterprise, User, Session, and SSO types for the hosted
// control plane. These complement the agent-facing types in model.go.
package model

import "time"

// EnterprisePlan classifies the billing tier of an enterprise.
type EnterprisePlan string

const (
	EnterprisePlanFree       EnterprisePlan = "free"
	EnterprisePlanPro        EnterprisePlan = "pro"
	EnterprisePlanEnterprise EnterprisePlan = "enterprise"
)

// Enterprise is a top-level account for billing, user management, and settings.
// A personal enterprise (Personal == true) is auto-created for users who sign
// in with a personal email (e.g. Gmail) that is not tied to an organization.
type Enterprise struct {
	ID                   string         `json:"id"`
	Name                 string         `json:"name"`
	Slug                 string         `json:"slug"`                    // URL-friendly, unique
	Plan                 EnterprisePlan `json:"plan"`
	Personal             bool           `json:"personal"`                // true for single-user personal accounts
	BillingEmail         string         `json:"billing_email"`
	SSORequired          bool           `json:"sso_required"`
	AllowedAuthProviders []string       `json:"allowed_auth_providers"`  // e.g. ["google", "okta"]
	AllowedEmailDomains  []string       `json:"allowed_email_domains"`   // e.g. ["acme.com"]
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
}

// UserRole classifies a user's permissions within an enterprise.
type UserRole string

const (
	UserRoleOwner  UserRole = "owner"
	UserRoleAdmin  UserRole = "admin"
	UserRoleMember UserRole = "member"
)

// UserStatus tracks the lifecycle state of a user.
type UserStatus string

const (
	UserStatusActive              UserStatus = "active"
	UserStatusPendingVerification UserStatus = "pending_verification"
	UserStatusInvited             UserStatus = "invited"
	UserStatusSuspended           UserStatus = "suspended"
)

// User is an authenticated person associated with an enterprise.
// The ID is an opaque surrogate key (usr_ + UUID). Email is the stable
// logical identity used to deduplicate across OAuth providers — when any
// provider returns an email, we look up or create the user by email.
type User struct {
	ID            string             `json:"id"`              // usr_ + UUID — immutable surrogate key
	EnterpriseID  string             `json:"enterprise_id"`
	Email         string             `json:"email"`           // unique, used for cross-provider deduplication
	DisplayName   string             `json:"display_name"`
	AvatarURL     string             `json:"avatar_url"`
	Role          UserRole           `json:"role"`
	Status        UserStatus         `json:"status"`
	PasswordHash  string             `json:"-"`               // bcrypt hash; never exposed via API
	LastLoginAt   *time.Time         `json:"last_login_at"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
	AuthProviders []UserAuthProvider `json:"auth_providers"`  // populated by handler; not stored on users table
}

// UserAuthProvider links a user to an external OAuth/SSO identity provider.
// Each row represents one connected provider (e.g. Google, GitHub).
type UserAuthProvider struct {
	ID        string    `json:"id"`         // uap_ + UUID
	UserID    string    `json:"user_id"`
	Provider  string    `json:"provider"`   // "google", "github", "okta", "saml", etc.
	SubjectID string    `json:"subject_id"` // external IdP subject identifier
	CreatedAt time.Time `json:"created_at"`
}

// VerificationCode is a single-use code sent to a user's email to verify
// their account. Codes are hashed (SHA-256) before storage.
type VerificationCode struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	CodeHash  string    `json:"-"`          // SHA-256 of the 6-digit code; never exposed via API
	ExpiresAt time.Time `json:"expires_at"`
	Used      bool      `json:"used"`
	CreatedAt time.Time `json:"created_at"`
}

// Session is a login session backed by a refresh token.
type Session struct {
	ID               string    `json:"id"`
	UserID           string    `json:"user_id"`
	TokenHash        string    `json:"-"`          // SHA-256 of access token; never exposed via API
	RefreshTokenHash string    `json:"-"`          // SHA-256 of refresh token; never exposed via API
	ExpiresAt        time.Time `json:"expires_at"`
	CreatedAt        time.Time `json:"created_at"`
}

// SSOProvider identifies the type of SSO integration.
type SSOProvider string

const (
	SSOProviderGoogle      SSOProvider = "google"
	SSOProviderOkta        SSOProvider = "okta"
	SSOProviderSAMLGeneric SSOProvider = "saml_generic"
)

// SSOConfig stores per-enterprise SSO provider configuration.
type SSOConfig struct {
	ID           string      `json:"id"`
	EnterpriseID string      `json:"enterprise_id"`
	Provider     SSOProvider `json:"provider"`
	ClientID     string      `json:"-"`              // stored encrypted; never exposed via API
	IssuerURL    string      `json:"issuer_url"`
	Enabled      bool        `json:"enabled"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}
