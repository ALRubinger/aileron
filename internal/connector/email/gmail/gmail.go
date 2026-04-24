// Package gmail implements the email connector for Gmail.
//
// This connector executes email.send and email.draft actions via the Gmail
// API. Credentials (OAuth refresh token) are injected at execution time
// from the vault.
package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"strings"

	"github.com/ALRubinger/aileron/internal/connector"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gmailapi "google.golang.org/api/gmail/v1"
	option "google.golang.org/api/option"
)

const (
	connectorType     = "email"
	connectorProvider = "gmail"
)

// Connector executes email actions via Gmail.
type Connector struct {
	clientID     string
	clientSecret string
	endpoint     string // override Gmail API base URL (for testing)
}

// New returns a new Gmail connector.
// clientID and clientSecret are required for OAuth token refresh.
func New(clientID, clientSecret string) *Connector {
	return &Connector{
		clientID:     clientID,
		clientSecret: clientSecret,
	}
}

// Type implements connector.Connector.
func (c *Connector) Type() string { return connectorType }

// Provider implements connector.Connector.
func (c *Connector) Provider() string { return connectorProvider }

// Execute implements connector.Connector.
func (c *Connector) Execute(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	if req.Credential == nil {
		return connector.ExecutionResult{}, errors.New("gmail: no credential injected")
	}

	switch req.ActionType {
	case "email.send":
		sendMode := paramStr(req.Parameters, "send_mode")
		if sendMode == "draft_only" {
			return c.createDraft(ctx, req)
		}
		return c.sendEmail(ctx, req)
	case "email.draft":
		return c.createDraft(ctx, req)
	default:
		return connector.ExecutionResult{
			Status: connector.ExecutionStatusFailed,
			Error:  "gmail: unsupported action type: " + req.ActionType,
		}, nil
	}
}

func (c *Connector) sendEmail(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	svc, err := c.newService(ctx, req.Credential.Value)
	if err != nil {
		return failResult("gmail: " + err.Error()), nil
	}

	mimeBytes, err := buildMIME(req.Parameters)
	if err != nil {
		return failResult("gmail: " + err.Error()), nil
	}

	msg := &gmailapi.Message{
		Raw: base64.URLEncoding.EncodeToString(mimeBytes),
	}
	if threadRef := paramStr(req.Parameters, "thread_ref"); threadRef != "" {
		msg.ThreadId = threadRef
	}

	sent, err := svc.Users.Messages.Send("me", msg).Context(ctx).Do()
	if err != nil {
		return failResult("gmail: messages.send: " + err.Error()), nil
	}

	return connector.ExecutionResult{
		Status:     connector.ExecutionStatusSucceeded,
		ReceiptRef: sent.Id,
		Output: map[string]any{
			"message_id": sent.Id,
			"thread_id":  sent.ThreadId,
		},
	}, nil
}

func (c *Connector) createDraft(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	svc, err := c.newService(ctx, req.Credential.Value)
	if err != nil {
		return failResult("gmail: " + err.Error()), nil
	}

	mimeBytes, err := buildMIME(req.Parameters)
	if err != nil {
		return failResult("gmail: " + err.Error()), nil
	}

	draft := &gmailapi.Draft{
		Message: &gmailapi.Message{
			Raw: base64.URLEncoding.EncodeToString(mimeBytes),
		},
	}
	if threadRef := paramStr(req.Parameters, "thread_ref"); threadRef != "" {
		draft.Message.ThreadId = threadRef
	}

	created, err := svc.Users.Drafts.Create("me", draft).Context(ctx).Do()
	if err != nil {
		return failResult("gmail: drafts.create: " + err.Error()), nil
	}

	return connector.ExecutionResult{
		Status:     connector.ExecutionStatusSucceeded,
		ReceiptRef: created.Id,
		Output: map[string]any{
			"draft_id": created.Id,
		},
	}, nil
}

func (c *Connector) newService(ctx context.Context, tokenJSON []byte) (*gmailapi.Service, error) {
	ts, err := c.tokenSource(ctx, tokenJSON)
	if err != nil {
		return nil, err
	}
	opts := []option.ClientOption{option.WithTokenSource(ts)}
	if c.endpoint != "" {
		opts = append(opts, option.WithEndpoint(c.endpoint))
	}
	return gmailapi.NewService(ctx, opts...)
}

func (c *Connector) tokenSource(ctx context.Context, tokenJSON []byte) (oauth2.TokenSource, error) {
	var tok oauth2.Token
	if err := json.Unmarshal(tokenJSON, &tok); err != nil {
		return nil, fmt.Errorf("invalid token JSON: %w", err)
	}
	if tok.RefreshToken == "" && tok.AccessToken == "" {
		return nil, fmt.Errorf("token has neither access_token nor refresh_token")
	}
	oauthConfig := &oauth2.Config{
		ClientID:     c.clientID,
		ClientSecret: c.clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{gmailapi.GmailSendScope, gmailapi.GmailComposeScope},
	}
	return oauthConfig.TokenSource(ctx, &tok), nil
}

// buildMIME constructs an RFC 2822 MIME message from the parameters.
func buildMIME(params map[string]any) ([]byte, error) {
	subject := paramStr(params, "subject")
	bodyText := paramStr(params, "body_text")
	bodyHTML := paramStr(params, "body_html")

	to := formatRecipients(params, "to")
	cc := formatRecipients(params, "cc")
	bcc := formatRecipients(params, "bcc")

	if len(to) == 0 {
		return nil, fmt.Errorf("at least one 'to' recipient is required")
	}

	var buf bytes.Buffer

	// If we have both text and HTML, use multipart/alternative.
	if bodyText != "" && bodyHTML != "" {
		writer := multipart.NewWriter(&buf)
		boundary := writer.Boundary()

		// Write headers.
		fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(to, ", "))
		if len(cc) > 0 {
			fmt.Fprintf(&buf, "Cc: %s\r\n", strings.Join(cc, ", "))
		}
		if len(bcc) > 0 {
			fmt.Fprintf(&buf, "Bcc: %s\r\n", strings.Join(bcc, ", "))
		}
		fmt.Fprintf(&buf, "Subject: %s\r\n", subject)
		fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
		fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%s\r\n\r\n", boundary)

		// Text part.
		textHeader := textproto.MIMEHeader{}
		textHeader.Set("Content-Type", "text/plain; charset=UTF-8")
		pw, _ := writer.CreatePart(textHeader)
		pw.Write([]byte(bodyText))

		// HTML part.
		htmlHeader := textproto.MIMEHeader{}
		htmlHeader.Set("Content-Type", "text/html; charset=UTF-8")
		pw, _ = writer.CreatePart(htmlHeader)
		pw.Write([]byte(bodyHTML))

		writer.Close()
	} else {
		// Simple message with either text or HTML.
		fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(to, ", "))
		if len(cc) > 0 {
			fmt.Fprintf(&buf, "Cc: %s\r\n", strings.Join(cc, ", "))
		}
		if len(bcc) > 0 {
			fmt.Fprintf(&buf, "Bcc: %s\r\n", strings.Join(bcc, ", "))
		}
		fmt.Fprintf(&buf, "Subject: %s\r\n", subject)
		fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")

		if bodyHTML != "" {
			fmt.Fprintf(&buf, "Content-Type: text/html; charset=UTF-8\r\n\r\n")
			buf.WriteString(bodyHTML)
		} else {
			fmt.Fprintf(&buf, "Content-Type: text/plain; charset=UTF-8\r\n\r\n")
			buf.WriteString(bodyText)
		}
	}

	return buf.Bytes(), nil
}

// formatRecipients extracts email addresses from the params map.
// Recipients can be stored as []any (from JSON) containing maps with "email" and "name" keys.
func formatRecipients(params map[string]any, key string) []string {
	v, ok := params[key]
	if !ok {
		return nil
	}

	list, ok := v.([]any)
	if !ok {
		return nil
	}

	var addrs []string
	for _, item := range list {
		switch r := item.(type) {
		case map[string]any:
			email, _ := r["email"].(string)
			name, _ := r["name"].(string)
			if email != "" {
				if name != "" {
					addrs = append(addrs, fmt.Sprintf("%s <%s>", name, email))
				} else {
					addrs = append(addrs, email)
				}
			}
		case string:
			addrs = append(addrs, r)
		}
	}
	return addrs
}

func paramStr(params map[string]any, key string) string {
	v, ok := params[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

func failResult(msg string) connector.ExecutionResult {
	return connector.ExecutionResult{
		Status: connector.ExecutionStatusFailed,
		Error:  msg,
	}
}
