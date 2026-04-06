package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestResendMailer_ImplementsMailer(t *testing.T) {
	var _ Mailer = NewResendMailer(ResendMailerConfig{APIKey: "re_test"})
}

func TestNewResendMailer_Defaults(t *testing.T) {
	m := NewResendMailer(ResendMailerConfig{APIKey: "re_test"})
	if m.from != "noreply@withaileron.ai" {
		t.Errorf("from = %q, want noreply@withaileron.ai", m.from)
	}
	if m.httpClient != http.DefaultClient {
		t.Error("expected default http client when none provided")
	}
	if m.apiKey != "re_test" {
		t.Errorf("apiKey = %q, want re_test", m.apiKey)
	}
}

func TestResendMailer_SendVerificationCode(t *testing.T) {
	var gotReq struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		HTML    string   `json:"html"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer re_testkey" {
			t.Errorf("unexpected Authorization header: %s", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("unexpected Content-Type: %s", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"abc123"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	// Point the mailer at the fake server by using a custom HTTP client
	// with a transport that rewrites the host.
	transport := &hostOverrideTransport{
		base:    http.DefaultTransport,
		target:  srv.URL,
		replace: "https://api.resend.com",
	}
	mailer := NewResendMailer(ResendMailerConfig{
		APIKey:     "re_testkey",
		From:       "test@example.com",
		HTTPClient: &http.Client{Transport: transport},
	})

	if err := mailer.SendVerificationCode(context.Background(), "bob@example.com", "654321"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotReq.From != "test@example.com" {
		t.Errorf("from: got %q, want %q", gotReq.From, "test@example.com")
	}
	if len(gotReq.To) != 1 || gotReq.To[0] != "bob@example.com" {
		t.Errorf("to: got %v, want [bob@example.com]", gotReq.To)
	}
	if gotReq.Subject == "" {
		t.Error("subject should not be empty")
	}
	if gotReq.HTML == "" {
		t.Error("html body should not be empty")
	}
}

func TestResendMailer_SendVerificationCode_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Invalid API key"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	transport := &hostOverrideTransport{
		base:    http.DefaultTransport,
		target:  srv.URL,
		replace: "https://api.resend.com",
	}
	mailer := NewResendMailer(ResendMailerConfig{
		APIKey:     "bad_key",
		HTTPClient: &http.Client{Transport: transport},
	})

	err := mailer.SendVerificationCode(context.Background(), "bob@example.com", "000000")
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}

func TestResendMailer_SendVerificationCode_NetworkError(t *testing.T) {
	mailer := NewResendMailer(ResendMailerConfig{
		APIKey: "re_test",
		HTTPClient: &http.Client{Transport: &failingTransport{}},
	})

	err := mailer.SendVerificationCode(context.Background(), "bob@example.com", "123456")
	if err == nil {
		t.Fatal("expected error for network failure")
	}
}

// failingTransport always returns a connection error.
type failingTransport struct{}

func (t *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("connection refused")
}

// hostOverrideTransport rewrites requests targeting `replace` to `target`.
type hostOverrideTransport struct {
	base    http.RoundTripper
	target  string // e.g. "http://127.0.0.1:PORT"
	replace string // e.g. "https://api.resend.com"
}

func (t *hostOverrideTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	url := cloned.URL.String()
	if len(url) >= len(t.replace) && url[:len(t.replace)] == t.replace {
		cloned.URL.Scheme = "http"
		cloned.URL.Host = req.URL.Host
		// parse target host
		parsed, err := http.NewRequest(http.MethodGet, t.target, nil)
		if err == nil {
			cloned.URL.Host = parsed.URL.Host
			cloned.URL.Scheme = parsed.URL.Scheme
		}
	}
	return t.base.RoundTrip(cloned)
}
