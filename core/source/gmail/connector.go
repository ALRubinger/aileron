// Package gmail implements a SourceConnector for Gmail, providing read-only
// tools for searching emails and reading threads.
package gmail

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ALRubinger/aileron/core/source"
	"golang.org/x/oauth2"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// Connector implements source.SourceConnector for Gmail.
type Connector struct {
	// clientOption allows injecting a custom HTTP client for testing.
	clientOption option.ClientOption
}

// New creates a new Gmail source connector.
func New() *Connector {
	return &Connector{}
}

// WithClientOption returns a copy with a custom client option (for testing).
func (c *Connector) WithClientOption(opt option.ClientOption) *Connector {
	cp := *c
	cp.clientOption = opt
	return &cp
}

func (c *Connector) Provider() string { return "gmail" }

func (c *Connector) Tools() []source.ToolDefinition {
	return []source.ToolDefinition{
		{
			Name:        "gmail_search",
			Description: "Search emails using Gmail search syntax. Returns matching messages with subject, sender, snippet, and date.",
			Parameters: []source.ToolParam{
				{Name: "query", Type: "string", Description: "Gmail search query (e.g. 'from:sarah subject:migration')", Required: true},
				{Name: "max_results", Type: "integer", Description: "Maximum results to return (default 10, max 25)", Required: false},
			},
		},
		{
			Name:        "gmail_get_thread",
			Description: "Get a full email thread with all messages. Returns each message's sender, subject, date, and body snippet.",
			Parameters: []source.ToolParam{
				{Name: "thread_id", Type: "string", Description: "The Gmail thread ID", Required: true},
			},
		},
	}
}

func (c *Connector) Execute(ctx context.Context, tool string, params map[string]any, token []byte) (map[string]any, error) {
	svc, err := c.newService(ctx, token)
	if err != nil {
		return nil, err
	}

	switch tool {
	case "gmail_search":
		return c.search(ctx, svc, params)
	case "gmail_get_thread":
		return c.getThread(ctx, svc, params)
	default:
		return nil, fmt.Errorf("unknown tool: %s", tool)
	}
}

func (c *Connector) newService(ctx context.Context, token []byte) (*gmail.Service, error) {
	accessToken, err := extractAccessToken(token)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: accessToken})
	opts := []option.ClientOption{option.WithTokenSource(ts)}
	if c.clientOption != nil {
		opts = append(opts, c.clientOption)
	}
	svc, err := gmail.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating gmail service: %w", err)
	}
	return svc, nil
}

func (c *Connector) search(ctx context.Context, svc *gmail.Service, params map[string]any) (map[string]any, error) {
	query, ok := params["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("query parameter is required")
	}

	maxResults := int64(10)
	if mr, ok := params["max_results"]; ok {
		maxResults = toInt64(mr, 10)
	}
	if maxResults > 25 {
		maxResults = 25
	}

	resp, err := svc.Users.Messages.List("me").Q(query).MaxResults(maxResults).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail API error: %w", err)
	}

	messages := make([]map[string]any, 0, len(resp.Messages))
	for _, msg := range resp.Messages {
		detail, err := svc.Users.Messages.Get("me", msg.Id).Format("metadata").
			MetadataHeaders("Subject", "From", "Date").Context(ctx).Do()
		if err != nil {
			continue
		}

		item := map[string]any{
			"id":        msg.Id,
			"thread_id": msg.ThreadId,
			"snippet":   detail.Snippet,
		}
		for _, h := range detail.Payload.Headers {
			switch h.Name {
			case "Subject":
				item["subject"] = h.Value
			case "From":
				item["from"] = h.Value
			case "Date":
				item["date"] = h.Value
			}
		}
		messages = append(messages, item)
	}

	return map[string]any{"messages": messages, "total": resp.ResultSizeEstimate}, nil
}

func (c *Connector) getThread(ctx context.Context, svc *gmail.Service, params map[string]any) (map[string]any, error) {
	threadID, ok := params["thread_id"].(string)
	if !ok || threadID == "" {
		return nil, fmt.Errorf("thread_id parameter is required")
	}

	thread, err := svc.Users.Threads.Get("me", threadID).Format("metadata").
		MetadataHeaders("Subject", "From", "Date").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail API error: %w", err)
	}

	messages := make([]map[string]any, 0, len(thread.Messages))
	for _, msg := range thread.Messages {
		item := map[string]any{
			"id":      msg.Id,
			"snippet": msg.Snippet,
		}
		for _, h := range msg.Payload.Headers {
			switch h.Name {
			case "Subject":
				item["subject"] = h.Value
			case "From":
				item["from"] = h.Value
			case "Date":
				item["date"] = h.Value
			}
		}
		messages = append(messages, item)
	}

	return map[string]any{"messages": messages}, nil
}

// extractAccessToken parses stored token JSON and returns the access token.
func extractAccessToken(token []byte) (string, error) {
	// Try OAuth2 token format.
	var oauthToken oauth2.Token
	if err := json.Unmarshal(token, &oauthToken); err == nil && oauthToken.AccessToken != "" {
		return oauthToken.AccessToken, nil
	}
	// Try simple map.
	var data map[string]string
	if err := json.Unmarshal(token, &data); err == nil {
		if at := data["access_token"]; at != "" {
			return at, nil
		}
	}
	return "", fmt.Errorf("no access_token in token data")
}

func toInt64(v any, def int64) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return def
		}
		return i
	default:
		return def
	}
}

