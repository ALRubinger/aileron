package auth

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

func TestLogMailer_SendVerificationCode(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	m := NewLogMailer(log)

	err := m.SendVerificationCode(context.Background(), "alice@example.com", "123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if output == "" {
		t.Fatal("expected log output")
	}
	if !bytes.Contains(buf.Bytes(), []byte("alice@example.com")) {
		t.Error("log output should contain email address")
	}
	if !bytes.Contains(buf.Bytes(), []byte("123456")) {
		t.Error("log output should contain verification code")
	}
}

func TestLogMailer_ImplementsMailer(t *testing.T) {
	var _ Mailer = NewLogMailer(slog.Default())
}
