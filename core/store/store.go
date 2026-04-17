// Package store defines persistence interfaces for the control plane.
//
// These interfaces decouple the API handlers from the storage backend.
// The in-memory implementations (in the mem sub-package) are suitable for
// development and testing. Production deployments swap in Postgres-backed
// implementations without changing business logic.
package store

import (
	"context"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/model"
)

// IntentStore persists and retrieves action intents.
type IntentStore interface {
	Create(ctx context.Context, intent api.IntentEnvelope) error
	Get(ctx context.Context, intentID string) (api.IntentEnvelope, error)
	List(ctx context.Context, filter IntentFilter) ([]api.IntentEnvelope, error)
	Update(ctx context.Context, intent api.IntentEnvelope) error
}

// IntentFilter scopes an intent list query.
type IntentFilter struct {
	WorkspaceID string
	Status      *api.IntentStatus
	AgentID     string
	PageSize    int
	PageToken   string
}

// ApprovalStore persists and retrieves approval requests.
type ApprovalStore interface {
	Create(ctx context.Context, approval api.Approval) error
	Get(ctx context.Context, approvalID string) (api.Approval, error)
	List(ctx context.Context, filter ApprovalFilter) ([]api.Approval, error)
	Update(ctx context.Context, approval api.Approval) error
}

// ApprovalFilter scopes an approval list query.
type ApprovalFilter struct {
	WorkspaceID string
	Status      *api.ApprovalStatus
	IntentID    string
	PageSize    int
	PageToken   string
}

// PolicyStore persists and retrieves policy definitions.
type PolicyStore interface {
	Create(ctx context.Context, policy api.Policy) error
	Get(ctx context.Context, policyID string) (api.Policy, error)
	List(ctx context.Context, filter PolicyFilter) ([]api.Policy, error)
	Update(ctx context.Context, policy api.Policy) error
}

// PolicyFilter scopes a policy list query.
type PolicyFilter struct {
	WorkspaceID string
	Status      *api.PolicyStatus
	PageSize    int
	PageToken   string
}

// GrantStore persists and retrieves execution grants.
type GrantStore interface {
	Create(ctx context.Context, grant api.ExecutionGrant) error
	Get(ctx context.Context, grantID string) (api.ExecutionGrant, error)
	Update(ctx context.Context, grant api.ExecutionGrant) error
}

// ExecutionStore persists and retrieves execution records.
type ExecutionStore interface {
	Create(ctx context.Context, execution api.Execution) error
	Get(ctx context.Context, executionID string) (api.Execution, error)
	Update(ctx context.Context, execution api.Execution) error
}

// ConnectorStore persists and retrieves connector registrations.
type ConnectorStore interface {
	Create(ctx context.Context, connector api.Connector) error
	Get(ctx context.Context, connectorID string) (api.Connector, error)
	List(ctx context.Context, filter ConnectorFilter) ([]api.Connector, error)
	Update(ctx context.Context, connector api.Connector) error
}

// ConnectorFilter scopes a connector list query.
type ConnectorFilter struct {
	WorkspaceID string
	Type        *api.ConnectorType
	PageSize    int
	PageToken   string
}

// CredentialStore persists and retrieves credential references (not secrets).
type CredentialStore interface {
	Create(ctx context.Context, cred api.CredentialReference) error
	Get(ctx context.Context, credentialID string) (api.CredentialReference, error)
	List(ctx context.Context, filter CredentialFilter) ([]api.CredentialReference, error)
}

// CredentialFilter scopes a credential list query.
type CredentialFilter struct {
	WorkspaceID string
	PageSize    int
	PageToken   string
}

// FundingSourceStore persists and retrieves funding sources.
type FundingSourceStore interface {
	Create(ctx context.Context, fs api.FundingSource) error
	Get(ctx context.Context, fundingSourceID string) (api.FundingSource, error)
	List(ctx context.Context, filter FundingSourceFilter) ([]api.FundingSource, error)
}

// FundingSourceFilter scopes a funding source list query.
type FundingSourceFilter struct {
	WorkspaceID string
	PageSize    int
	PageToken   string
}

// EnterpriseStore persists and retrieves enterprises.
type EnterpriseStore interface {
	Create(ctx context.Context, enterprise model.Enterprise) error
	Get(ctx context.Context, enterpriseID string) (model.Enterprise, error)
	GetBySlug(ctx context.Context, slug string) (model.Enterprise, error)
	Update(ctx context.Context, enterprise model.Enterprise) error
}

// UserStore persists and retrieves users. The ID is a surrogate key (usr_ + UUID).
// Email is a unique column used for cross-provider deduplication.
type UserStore interface {
	Create(ctx context.Context, user model.User) error
	Get(ctx context.Context, userID string) (model.User, error)
	GetByEmail(ctx context.Context, email string) (model.User, error)
	List(ctx context.Context, filter UserFilter) ([]model.User, error)
	Update(ctx context.Context, user model.User) error
}

// UserAuthProviderStore persists and retrieves OAuth/SSO identity links for users.
// Each link maps a user to an external provider + subject pair.
type UserAuthProviderStore interface {
	Create(ctx context.Context, link model.UserAuthProvider) error
	GetByProviderSubject(ctx context.Context, provider, subjectID string) (model.UserAuthProvider, error)
	ListByUser(ctx context.Context, userID string) ([]model.UserAuthProvider, error)
	Delete(ctx context.Context, id string) error
	DeleteByUserAndProvider(ctx context.Context, userID, provider string) error
}

// UserFilter scopes a user list query.
type UserFilter struct {
	EnterpriseID string
	Status       *model.UserStatus
	PageSize     int
	PageToken    string
}

// SessionStore persists and retrieves login sessions.
type SessionStore interface {
	Create(ctx context.Context, session model.Session) error
	GetByTokenHash(ctx context.Context, tokenHash string) (model.Session, error)
	GetByRefreshTokenHash(ctx context.Context, refreshHash string) (model.Session, error)
	Delete(ctx context.Context, sessionID string) error
	DeleteAllForUser(ctx context.Context, userID string) error
}

// VerificationCodeStore persists and retrieves email verification codes.
type VerificationCodeStore interface {
	Create(ctx context.Context, code model.VerificationCode) error
	// GetActiveByUserID returns the most recent unused, unexpired code for the user.
	GetActiveByUserID(ctx context.Context, userID string) (model.VerificationCode, error)
	MarkUsed(ctx context.Context, codeID string) error
	DeleteExpiredForUser(ctx context.Context, userID string) error
}

// SSOConfigStore persists and retrieves per-enterprise SSO configurations.
type SSOConfigStore interface {
	Create(ctx context.Context, cfg model.SSOConfig) error
	Get(ctx context.Context, configID string) (model.SSOConfig, error)
	GetByEnterprise(ctx context.Context, enterpriseID string) ([]model.SSOConfig, error)
	Update(ctx context.Context, cfg model.SSOConfig) error
	Delete(ctx context.Context, configID string) error
}

// ConnectedAccountStore persists and retrieves connected external accounts.
// Connected accounts link a user's external service (Gmail, Google Calendar,
// etc.) to Aileron so it can execute irreversible actions on the user's behalf.
type ConnectedAccountStore interface {
	Create(ctx context.Context, account model.ConnectedAccount) error
	// Upsert creates or updates a connected account by user+provider.
	// If an account already exists for this user+provider, it updates the
	// metadata (scopes, external IDs, timestamps) and returns the existing ID.
	// Used by OAuth callbacks when a user reconnects.
	Upsert(ctx context.Context, account model.ConnectedAccount) (model.ConnectedAccount, error)
	Get(ctx context.Context, accountID string) (model.ConnectedAccount, error)
	List(ctx context.Context, filter ConnectedAccountFilter) ([]model.ConnectedAccount, error)
	Delete(ctx context.Context, accountID string) error
}

// ConnectedAccountFilter scopes a connected account list query.
type ConnectedAccountFilter struct {
	UserID         string
	Provider       *model.ConnectedAccountProvider
	Status         *model.ConnectedAccountStatus
	ExternalUserID string // filter by provider-specific user ID (e.g. Slack user)
	ExternalTeamID string // filter by provider-specific team/workspace ID (e.g. Slack team)
}

// DraftStore persists and retrieves AI-generated draft replies.
type DraftStore interface {
	Create(ctx context.Context, draft model.Draft) error
	Get(ctx context.Context, draftID string) (model.Draft, error)
	List(ctx context.Context, filter DraftFilter) ([]model.Draft, error)
	Update(ctx context.Context, draft model.Draft) error
}

// DraftFilter scopes a draft list query.
type DraftFilter struct {
	UserID string
	Status *model.DraftStatus
}

// DraftFeedbackStore persists and retrieves feedback signals from draft
// approve/edit/discard actions. These signals feed the behavioral model.
type DraftFeedbackStore interface {
	Create(ctx context.Context, feedback model.DraftFeedback) error
	List(ctx context.Context, filter DraftFeedbackFilter) ([]model.DraftFeedback, error)
}

// DraftFeedbackFilter scopes a feedback list query.
type DraftFeedbackFilter struct {
	UserID string
	Signal *model.FeedbackSignal
}

// UserInstructionStore persists and retrieves user instructions for
// the context store envelope.
type UserInstructionStore interface {
	Create(ctx context.Context, instruction model.UserInstruction) error
	Get(ctx context.Context, instructionID string) (model.UserInstruction, error)
	List(ctx context.Context, filter UserInstructionFilter) ([]model.UserInstruction, error)
	Update(ctx context.Context, instruction model.UserInstruction) error
	Delete(ctx context.Context, instructionID string) error
}

// UserInstructionFilter scopes an instruction list query.
type UserInstructionFilter struct {
	UserID string
	Active *bool
}

// UserKeyMaterialStore persists and retrieves user key material for the
// zero-knowledge vault. The KEK itself is never stored — only the Argon2id
// salt and an encrypted verification blob.
type UserKeyMaterialStore interface {
	Create(ctx context.Context, material model.UserKeyMaterial) error
	Get(ctx context.Context, userID string) (model.UserKeyMaterial, error)
	Update(ctx context.Context, material model.UserKeyMaterial) error
}

// ErrNotFound is returned when a requested entity does not exist.
type ErrNotFound struct {
	Entity string
	ID     string
}

func (e *ErrNotFound) Error() string {
	return e.Entity + " not found: " + e.ID
}
