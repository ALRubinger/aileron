// Package aileron provides a Go client for the Aileron control plane API.
//
// Usage:
//
//	client := aileron.NewClient("https://api.example.com", aileron.WithAPIKey("..."))
//	intent, err := client.Intents.Create(ctx, aileron.CreateIntentRequest{ ... })
package aileron

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is the Aileron API client.
type Client struct {
	baseURL    string
	httpClient *http.Client
	apiKey     string

	Intents    *IntentsService
	Approvals  *ApprovalsService
	Policies   *PoliciesService
	Executions *ExecutionsService
}

// Option configures the client.
type Option func(*Client)

// WithAPIKey sets the API key for authentication.
func WithAPIKey(key string) Option {
	return func(c *Client) {
		c.apiKey = key
	}
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// NewClient returns a new Aileron API client.
func NewClient(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(c)
	}

	c.Intents = &IntentsService{client: c}
	c.Approvals = &ApprovalsService{client: c}
	c.Policies = &PoliciesService{client: c}
	c.Executions = &ExecutionsService{client: c}

	return c
}

func (c *Client) do(ctx context.Context, method, path string, body, result any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}

// --- Intents ---

type IntentsService struct{ client *Client }

type CreateIntentRequest struct {
	WorkspaceID    string          `json:"workspace_id"`
	AgentID        string          `json:"agent_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Action         ActionIntent    `json:"action"`
	Context        *IntentContext  `json:"context,omitempty"`
	CallbackURL    *string         `json:"callback_url,omitempty"`
}

type ActionIntent struct {
	Type          string       `json:"type"`
	Summary       string       `json:"summary"`
	Justification *string      `json:"justification,omitempty"`
	Domain        *DomainAction `json:"domain,omitempty"`
}

type DomainAction struct {
	Git     *GitAction     `json:"git,omitempty"`
	Payment *PaymentAction `json:"payment,omitempty"`
}

type GitAction struct {
	Provider   *string  `json:"provider,omitempty"`
	Repository *string  `json:"repository,omitempty"`
	Branch     *string  `json:"branch,omitempty"`
	BaseBranch *string  `json:"base_branch,omitempty"`
	PRTitle    *string  `json:"pr_title,omitempty"`
	PRBody     *string  `json:"pr_body,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Reviewers  []string `json:"reviewers,omitempty"`
}

type PaymentAction struct {
	VendorName *string `json:"vendor_name,omitempty"`
	Amount     *Money  `json:"amount,omitempty"`
}

type Money struct {
	Amount   int    `json:"amount"`
	Currency string `json:"currency"`
}

type IntentContext struct {
	Environment    *string `json:"environment,omitempty"`
	SourcePlatform *string `json:"source_platform,omitempty"`
	UserPresent    *bool   `json:"user_present,omitempty"`
}

type IntentEnvelope struct {
	IntentID    string      `json:"intent_id"`
	WorkspaceID string      `json:"workspace_id"`
	Status      string      `json:"status"`
	Action      ActionIntent `json:"action"`
	Decision    Decision    `json:"decision"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

type Decision struct {
	Disposition      string         `json:"disposition"`
	RiskLevel        string         `json:"risk_level"`
	ApprovalID       *string        `json:"approval_id,omitempty"`
	ExecutionGrantID *string        `json:"execution_grant_id,omitempty"`
	DenialReason     *string        `json:"denial_reason,omitempty"`
	RequiresApproval *bool          `json:"requires_approval,omitempty"`
	MatchedPolicies  []PolicyMatch  `json:"matched_policies,omitempty"`
}

type PolicyMatch struct {
	PolicyID      *string `json:"policy_id,omitempty"`
	RuleID        *string `json:"rule_id,omitempty"`
	Explanation   *string `json:"explanation,omitempty"`
}

func (s *IntentsService) Create(ctx context.Context, req CreateIntentRequest) (*IntentEnvelope, error) {
	var result IntentEnvelope
	if err := s.client.do(ctx, http.MethodPost, "/v1/intents", req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *IntentsService) Get(ctx context.Context, intentID string) (*IntentEnvelope, error) {
	var result IntentEnvelope
	if err := s.client.do(ctx, http.MethodGet, "/v1/intents/"+intentID, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Approvals ---

type ApprovalsService struct{ client *Client }

type Approval struct {
	ApprovalID  string     `json:"approval_id"`
	IntentID    string     `json:"intent_id"`
	Status      string     `json:"status"`
	Rationale   *string    `json:"rationale,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type ApprovalListResponse struct {
	Items []Approval `json:"items"`
}

func (s *ApprovalsService) Get(ctx context.Context, approvalID string) (*Approval, error) {
	var result Approval
	if err := s.client.do(ctx, http.MethodGet, "/v1/approvals/"+approvalID, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *ApprovalsService) List(ctx context.Context, workspaceID string) (*ApprovalListResponse, error) {
	var result ApprovalListResponse
	if err := s.client.do(ctx, http.MethodGet, "/v1/approvals?workspace_id="+workspaceID, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Executions ---

type ExecutionsService struct{ client *Client }

type ExecutionRunRequest struct {
	GrantID string `json:"grant_id"`
}

type ExecutionRunResponse struct {
	ExecutionID string `json:"execution_id"`
	Status      string `json:"status"`
}

func (s *ExecutionsService) Run(ctx context.Context, req ExecutionRunRequest) (*ExecutionRunResponse, error) {
	var result ExecutionRunResponse
	if err := s.client.do(ctx, http.MethodPost, "/v1/executions/run", req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

type Execution struct {
	ExecutionID string                  `json:"execution_id"`
	IntentID    string                  `json:"intent_id"`
	Status      string                  `json:"status"`
	Output      *map[string]interface{} `json:"output,omitempty"`
	ReceiptRef  *string                 `json:"receipt_ref,omitempty"`
}

func (s *ExecutionsService) Get(ctx context.Context, executionID string) (*Execution, error) {
	var result Execution
	if err := s.client.do(ctx, http.MethodGet, "/v1/executions/"+executionID, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Policies ---

type PoliciesService struct{ client *Client }
