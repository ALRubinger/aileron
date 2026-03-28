// Package aileron provides a Go client for the Aileron control plane API.
//
// Usage:
//
//	client := aileron.NewClient("https://api.example.com", aileron.WithAPIKey("..."))
//	intent, err := client.Intents.Create(ctx, aileron.CreateIntentRequest{ ... })
package aileron

import (
	"net/http"
	"time"
)

// Client is the Aileron API client.
type Client struct {
	baseURL    string
	httpClient *http.Client
	apiKey     string

	Intents  *IntentsService
	Approvals *ApprovalsService
	Policies *PoliciesService
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

	return c
}

// IntentsService provides access to intent endpoints.
// TODO: implement Create, Get, List, AppendEvidence.
type IntentsService struct{ client *Client }

// ApprovalsService provides access to approval endpoints.
// TODO: implement List, Get, Approve, Deny, Modify.
type ApprovalsService struct{ client *Client }

// PoliciesService provides access to policy endpoints.
// TODO: implement Create, Get, List, Update, Simulate.
type PoliciesService struct{ client *Client }
