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
	ID                   string
	Name                 string
	Slug                 string // URL-friendly, unique
	Plan                 EnterprisePlan
	Personal             bool   // true for single-user personal accounts
	BillingEmail         string
	SSORequired          bool
	AllowedAuthProviders []string // e.g. ["google", "okta"]
	AllowedEmailDomains  []string // e.g. ["acme.com"]
	CreatedAt            time.Time
	UpdatedAt            time.Time
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
	ID                    string // usr_ + UUID — immutable surrogate key
	EnterpriseID          string
	Email                 string // unique, used for cross-provider deduplication
	DisplayName           string
	AvatarURL             string
	Role                  UserRole
	Status                UserStatus
	AuthProvider          string // "email", "google", "okta", "saml", etc.
	AuthProviderSubjectID string // external IdP subject identifier; empty for email auth
	PasswordHash          string // bcrypt hash; empty for OAuth-only users
	LastLoginAt           *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// VerificationCode is a single-use code sent to a user's email to verify
// their account. Codes are hashed (SHA-256) before storage.
type VerificationCode struct {
	ID        string
	UserID    string
	CodeHash  string // SHA-256 of the 6-digit code
	ExpiresAt time.Time
	Used      bool
	CreatedAt time.Time
}

// Session is a login session backed by a refresh token.
type Session struct {
	ID               string
	UserID           string
	TokenHash        string // SHA-256 of access token
	RefreshTokenHash string // SHA-256 of refresh token
	ExpiresAt        time.Time
	CreatedAt        time.Time
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
	ID           string
	EnterpriseID string
	Provider     SSOProvider
	ClientID     string // stored encrypted
	IssuerURL    string
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
