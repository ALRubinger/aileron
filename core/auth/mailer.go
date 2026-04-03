package auth

import (
	"context"
	"log/slog"
)

// Mailer sends transactional emails. Implementations may delegate to
// Mailgun, AWS SES, SendGrid, SMTP, etc. The built-in LogMailer prints
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
