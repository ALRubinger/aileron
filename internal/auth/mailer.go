package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// Mailer sends transactional emails. Implementations may delegate to
// Resend, AWS SES, SendGrid, SMTP, etc. The built-in LogMailer prints
// to the server log for development.
type Mailer interface {
	// SendVerificationCode sends an email with the verification code.
	SendVerificationCode(ctx context.Context, to string, code string) error
}

// LogMailer is a development Mailer that logs emails instead of sending them.
type LogMailer struct {
	log *slog.Logger
}

// NewLogMailer returns a mailer that writes to the log.
func NewLogMailer(log *slog.Logger) *LogMailer {
	return &LogMailer{log: log}
}

func (m *LogMailer) SendVerificationCode(_ context.Context, to string, code string) error {
	m.log.Info("verification code (dev mailer)",
		"to", to,
		"code", code,
	)
	return nil
}

// ResendMailer sends transactional emails via the Resend REST API.
type ResendMailer struct {
	apiKey     string
	from       string
	httpClient *http.Client
}

// ResendMailerConfig holds configuration for constructing a ResendMailer.
type ResendMailerConfig struct {
	// APIKey is the Resend API key. Required.
	// Env: RESEND_API_KEY
	APIKey string
	// From is the sender email address shown to recipients.
	// Env: MAIL_FROM (default: "noreply@withaileron.ai")
	From string
	// HTTPClient is an optional custom HTTP client. Defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// NewResendMailer returns a Mailer that sends emails via Resend.
func NewResendMailer(cfg ResendMailerConfig) *ResendMailer {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	from := cfg.From
	if from == "" {
		from = "noreply@withaileron.ai"
	}
	return &ResendMailer{
		apiKey:     cfg.APIKey,
		from:       from,
		httpClient: hc,
	}
}

// SendVerificationCode sends a verification code email via the Resend API.
func (m *ResendMailer) SendVerificationCode(ctx context.Context, to string, code string) error {
	body := map[string]any{
		"from":    m.from,
		"to":      []string{to},
		"subject": "Your Aileron verification code",
		"html": fmt.Sprintf(
			`<p>Your Aileron verification code is: <strong>%s</strong></p>`+
				`<p>This code expires in 15 minutes.</p>`,
			code,
		),
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("resend: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("resend: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("resend: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return fmt.Errorf("resend: unexpected status %d: %s", resp.StatusCode, errBody.Message)
	}

	return nil
}
