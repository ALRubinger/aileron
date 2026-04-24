package gmail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/connector"
	"golang.org/x/oauth2"
)

func validToken() []byte {
	tok := oauth2.Token{AccessToken: "test-access-token"}
	b, _ := json.Marshal(tok)
	return b
}

func newTestConnector(serverURL string) *Connector {
	return &Connector{
		clientID:     "test-client-id",
		clientSecret: "test-client-secret",
		endpoint:     serverURL,
	}
}

func TestConnector_TypeAndProvider(t *testing.T) {
	c := New("", "")
	if c.Type() != "email" {
		t.Errorf("Type() = %q, want %q", c.Type(), "email")
	}
	if c.Provider() != "gmail" {
		t.Errorf("Provider() = %q, want %q", c.Provider(), "gmail")
	}
}

func TestConnector_NoCredential(t *testing.T) {
	c := New("", "")
	_, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.send",
	})
	if err == nil {
		t.Fatal("expected error for nil credential")
	}
}

func TestConnector_UnsupportedAction(t *testing.T) {
	c := New("", "")
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.unknown",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestSendEmail_Success(t *testing.T) {
	var receivedPath, receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":       "msg_123",
			"threadId": "thread_456",
		})
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.send",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"to":        []any{map[string]any{"email": "alice@example.com", "name": "Alice"}},
			"subject":   "Hello",
			"body_text": "Hi Alice!",
			"send_mode": "send_now",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q, want succeeded; Error: %s", result.Status, result.Error)
	}
	if result.ReceiptRef != "msg_123" {
		t.Errorf("ReceiptRef = %q, want msg_123", result.ReceiptRef)
	}
	if result.Output["message_id"] != "msg_123" {
		t.Errorf("message_id = %v", result.Output["message_id"])
	}
	if result.Output["thread_id"] != "thread_456" {
		t.Errorf("thread_id = %v", result.Output["thread_id"])
	}
	if receivedMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", receivedMethod)
	}
	if !strings.Contains(receivedPath, "/users/me/messages/send") {
		t.Errorf("path = %q, want /users/me/messages/send", receivedPath)
	}
}

func TestSendEmail_WithThreadRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":       "msg_reply",
			"threadId": "thread_original",
		})
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.send",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"to":         []any{map[string]any{"email": "bob@example.com"}},
			"subject":    "Re: Meeting",
			"body_text":  "See you there",
			"thread_ref": "thread_original",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q; Error: %s", result.Status, result.Error)
	}
}

func TestSendEmail_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"Insufficient permissions"}}`))
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.send",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"to":        []any{map[string]any{"email": "alice@example.com"}},
			"subject":   "Test",
			"body_text": "Hello",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Error, "messages.send") {
		t.Errorf("Error = %q, want mention of messages.send", result.Error)
	}
}

func TestSendEmail_NoRecipients(t *testing.T) {
	c := newTestConnector("http://unused")
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.send",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"subject":   "Test",
			"body_text": "Hello",
			// no "to"
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestSendEmail_InvalidToken(t *testing.T) {
	c := newTestConnector("http://unused")
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.send",
		Credential: &connector.InjectedCredential{Value: []byte("not-json")},
		Parameters: map[string]any{
			"to":        []any{map[string]any{"email": "alice@example.com"}},
			"subject":   "Test",
			"body_text": "Hello",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestCreateDraft_Success(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "draft_789",
			"message": map[string]any{
				"id": "msg_draft",
			},
		})
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.draft",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"to":        []any{map[string]any{"email": "alice@example.com"}},
			"subject":   "Draft Report",
			"body_text": "WIP...",
			"body_html": "<p>WIP...</p>",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q; Error: %s", result.Status, result.Error)
	}
	if result.ReceiptRef != "draft_789" {
		t.Errorf("ReceiptRef = %q, want draft_789", result.ReceiptRef)
	}
	if result.Output["draft_id"] != "draft_789" {
		t.Errorf("draft_id = %v", result.Output["draft_id"])
	}
	if !strings.Contains(receivedPath, "/users/me/drafts") {
		t.Errorf("path = %q, want /users/me/drafts", receivedPath)
	}
}

func TestCreateDraft_WithThreadRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "draft_reply"})
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.draft",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"to":         []any{map[string]any{"email": "alice@example.com"}},
			"subject":    "Re: Thread",
			"body_text":  "Reply text",
			"thread_ref": "thread_xyz",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q; Error: %s", result.Status, result.Error)
	}
}

func TestCreateDraft_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"Server error"}}`))
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.draft",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"to":        []any{map[string]any{"email": "alice@example.com"}},
			"subject":   "Test",
			"body_text": "Hello",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Error, "drafts.create") {
		t.Errorf("Error = %q, want mention of drafts.create", result.Error)
	}
}

func TestSendEmail_DraftOnlyRouting(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "draft_from_send"})
	}))
	defer server.Close()

	c := newTestConnector(server.URL)
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "email.send",
		Credential: &connector.InjectedCredential{Value: validToken()},
		Parameters: map[string]any{
			"to":        []any{map[string]any{"email": "test@example.com"}},
			"subject":   "Test",
			"body_text": "Hello",
			"send_mode": "draft_only",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q; Error: %s", result.Status, result.Error)
	}
	// Should have hit the drafts endpoint, not messages/send.
	if !strings.Contains(receivedPath, "/drafts") {
		t.Errorf("path = %q, expected drafts endpoint for draft_only", receivedPath)
	}
}

func TestTokenSource_InvalidJSON(t *testing.T) {
	c := New("id", "secret")
	_, err := c.tokenSource(context.Background(), []byte("not-json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestTokenSource_EmptyToken(t *testing.T) {
	c := New("id", "secret")
	emptyTok, _ := json.Marshal(oauth2.Token{})
	_, err := c.tokenSource(context.Background(), emptyTok)
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestTokenSource_ValidToken(t *testing.T) {
	c := New("id", "secret")
	ts, err := c.tokenSource(context.Background(), validToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ts == nil {
		t.Fatal("expected non-nil token source")
	}
}

func TestBuildMIME_SimpleText(t *testing.T) {
	params := map[string]any{
		"to":        []any{map[string]any{"email": "alice@example.com", "name": "Alice"}},
		"subject":   "Hello",
		"body_text": "Hi Alice, how are you?",
	}

	mime, err := buildMIME(params)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}

	s := string(mime)
	if !strings.Contains(s, "To: Alice <alice@example.com>") {
		t.Errorf("missing To header in MIME:\n%s", s)
	}
	if !strings.Contains(s, "Subject: Hello") {
		t.Errorf("missing Subject header in MIME:\n%s", s)
	}
	if !strings.Contains(s, "Content-Type: text/plain; charset=UTF-8") {
		t.Errorf("missing Content-Type in MIME:\n%s", s)
	}
	if !strings.Contains(s, "Hi Alice, how are you?") {
		t.Errorf("missing body in MIME:\n%s", s)
	}
}

func TestBuildMIME_HTMLOnly(t *testing.T) {
	params := map[string]any{
		"to":        []any{map[string]any{"email": "alice@example.com"}},
		"subject":   "News",
		"body_html": "<h1>Breaking</h1>",
	}

	mime, err := buildMIME(params)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}

	s := string(mime)
	if !strings.Contains(s, "Content-Type: text/html; charset=UTF-8") {
		t.Errorf("missing text/html Content-Type in MIME:\n%s", s)
	}
	if !strings.Contains(s, "<h1>Breaking</h1>") {
		t.Errorf("missing HTML body in MIME:\n%s", s)
	}
}

func TestBuildMIME_MultipartAlternative(t *testing.T) {
	params := map[string]any{
		"to":        []any{map[string]any{"email": "bob@example.com"}},
		"subject":   "Report",
		"body_text": "Plain text body",
		"body_html": "<h1>HTML body</h1>",
	}

	mime, err := buildMIME(params)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}

	s := string(mime)
	if !strings.Contains(s, "multipart/alternative") {
		t.Errorf("expected multipart/alternative in MIME:\n%s", s)
	}
	if !strings.Contains(s, "Plain text body") {
		t.Errorf("missing text body in MIME:\n%s", s)
	}
	if !strings.Contains(s, "<h1>HTML body</h1>") {
		t.Errorf("missing HTML body in MIME:\n%s", s)
	}
}

func TestBuildMIME_WithCCAndBCC(t *testing.T) {
	params := map[string]any{
		"to":        []any{map[string]any{"email": "alice@example.com"}},
		"cc":        []any{map[string]any{"email": "bob@example.com", "name": "Bob"}},
		"bcc":       []any{map[string]any{"email": "audit@example.com"}},
		"subject":   "Test",
		"body_text": "Hello",
	}

	mime, err := buildMIME(params)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}

	s := string(mime)
	if !strings.Contains(s, "Cc: Bob <bob@example.com>") {
		t.Errorf("missing CC header in MIME:\n%s", s)
	}
	if !strings.Contains(s, "Bcc: audit@example.com") {
		t.Errorf("missing BCC header in MIME:\n%s", s)
	}
}

func TestBuildMIME_NoRecipient(t *testing.T) {
	params := map[string]any{
		"subject":   "Test",
		"body_text": "Hello",
	}

	_, err := buildMIME(params)
	if err == nil {
		t.Fatal("expected error for no recipients")
	}
}

func TestFormatRecipients(t *testing.T) {
	params := map[string]any{
		"to": []any{
			map[string]any{"email": "alice@example.com", "name": "Alice"},
			map[string]any{"email": "bob@example.com"},
		},
	}

	addrs := formatRecipients(params, "to")
	if len(addrs) != 2 {
		t.Fatalf("expected 2 recipients, got %d", len(addrs))
	}
	if addrs[0] != "Alice <alice@example.com>" {
		t.Errorf("addrs[0] = %q", addrs[0])
	}
	if addrs[1] != "bob@example.com" {
		t.Errorf("addrs[1] = %q", addrs[1])
	}
}

func TestFormatRecipients_StringItems(t *testing.T) {
	params := map[string]any{
		"to": []any{"plain@example.com"},
	}
	addrs := formatRecipients(params, "to")
	if len(addrs) != 1 || addrs[0] != "plain@example.com" {
		t.Errorf("addrs = %v", addrs)
	}
}

func TestFormatRecipients_MissingKey(t *testing.T) {
	addrs := formatRecipients(map[string]any{}, "to")
	if addrs != nil {
		t.Errorf("expected nil for missing key, got %v", addrs)
	}
}

func TestFormatRecipients_WrongType(t *testing.T) {
	addrs := formatRecipients(map[string]any{"to": "not-a-list"}, "to")
	if addrs != nil {
		t.Errorf("expected nil for wrong type, got %v", addrs)
	}
}
