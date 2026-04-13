package launch_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/audit"
	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/launch"
	launchpolicy "github.com/ALRubinger/aileron/core/policy/launch"
	"github.com/ALRubinger/aileron/core/vault"
)

type mockHTTPServer struct {
	responseBody string
	statusCode   int
	handler      func(http.ResponseWriter, *http.Request)
}

func (m *mockHTTPServer) start() *httptest.Server {
	if m.handler != nil {
		return httptest.NewServer(http.HandlerFunc(m.handler))
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.statusCode != 0 {
			w.WriteHeader(m.statusCode)
		}
		w.Write([]byte(m.responseBody))
	}))
}

type mockSender struct {
	service string
	sent    bool
	lastMsg comms.OutgoingMessage
}

func (m *mockSender) Service() string                                              { return m.service }
func (m *mockSender) Connect(ctx context.Context) error                            { return nil }
func (m *mockSender) Listen(ctx context.Context) (<-chan comms.IncomingMessage, error) { return nil, nil }
func (m *mockSender) Send(ctx context.Context, msg comms.OutgoingMessage) error {
	m.sent = true
	m.lastMsg = msg
	return nil
}
func (m *mockSender) Close() error { return nil }

func TestCommsServer_ReadMessages(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-read.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)
	queue.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Alice", Body: "Is the deploy done?", Timestamp: time.Now()})
	queue.Push(launch.Message{ID: "2", Source: "discord", Channel: "dev-chat", Author: "Bob", Body: "PR looks good", Timestamp: time.Now()})

	srv, err := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	resp := launch.RequestComms(socketPath, launch.CommsRequest{Method: "read_messages"})
	if !resp.OK {
		t.Fatalf("expected OK, got error: %s", resp.Error)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(resp.Messages))
	}
	if resp.Messages[0].Author != "Alice" {
		t.Errorf("msg[0].Author = %q, want Alice", resp.Messages[0].Author)
	}
	if resp.Messages[1].Service != "discord" {
		t.Errorf("msg[1].Service = %q, want discord", resp.Messages[1].Service)
	}
}

func TestCommsServer_ReadMessages_FilterByService(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-filter.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)
	queue.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Body: "slack msg"})
	queue.Push(launch.Message{ID: "2", Source: "discord", Channel: "dev", Body: "discord msg"})

	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil, "", "")
	go srv.Serve()
	defer srv.Close()

	resp := launch.RequestComms(socketPath, launch.CommsRequest{Method: "read_messages", Service: "slack"})
	if len(resp.Messages) != 1 || resp.Messages[0].Service != "slack" {
		t.Errorf("expected 1 slack message, got %v", resp.Messages)
	}
}

func TestCommsServer_ReadMessages_MarksRead(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-markread.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)
	queue.Push(launch.Message{ID: "1", Source: "slack", Body: "hello"})

	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil, "", "")
	go srv.Serve()
	defer srv.Close()

	launch.RequestComms(socketPath, launch.CommsRequest{Method: "read_messages"})

	if queue.UnreadCount() != 0 {
		t.Errorf("expected 0 unread after read, got %d", queue.UnreadCount())
	}
}

func TestCommsServer_SendMessage_NoSender(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-nosender.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil, "", "")
	go srv.Serve()
	defer srv.Close()

	resp := launch.RequestComms(socketPath, launch.CommsRequest{
		Method:  "send_message",
		Service: "slack",
		Channel: "#test",
		Body:    "hello",
	})
	if resp.OK {
		t.Error("expected error when no sender configured")
	}
	if resp.Error == "" {
		t.Error("expected error message")
	}
}

func TestCommsServer_SendMessage_MissingFields(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-missing.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil, "", "")
	go srv.Serve()
	defer srv.Close()

	resp := launch.RequestComms(socketPath, launch.CommsRequest{Method: "send_message"})
	if resp.OK {
		t.Error("expected error for missing fields")
	}
}

func TestCommsServer_SendMessage_WithApproval(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-send.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	// Set up copier and router for the approval prompt.
	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	// Use a mock sender.
	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, copier, router, "", "")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "send_message",
			Service: "slack",
			Channel: "#backend",
			Body:    "The deploy is complete.",
		})
	}()

	// Wait for prompt, then approve.
	time.Sleep(300 * time.Millisecond)
	stdinW.Write([]byte("y"))

	select {
	case resp := <-done:
		if !resp.OK {
			t.Errorf("expected OK after approval, got error: %s", resp.Error)
		}
		if !sender.sent {
			t.Error("expected message to be sent")
		}
		if sender.lastMsg.Body != "The deploy is complete." {
			t.Errorf("sent body = %q", sender.lastMsg.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestCommsServer_SendMessage_Denied(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-deny.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, copier, router, "", "")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "send_message",
			Service: "slack",
			Channel: "#backend",
			Body:    "Draft reply",
		})
	}()

	time.Sleep(300 * time.Millisecond)
	stdinW.Write([]byte("n"))

	select {
	case resp := <-done:
		if resp.OK {
			t.Error("expected denial")
		}
		if sender.sent {
			t.Error("message should not have been sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestCommsServer_SendMessage_AuditApproved(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-audit-approve.sock"
	t.Cleanup(func() { os.Remove(socketPath) })
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, copier, router, auditPath, "s1")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method: "send_message", Service: "slack", Channel: "#backend", Body: "approved msg",
		})
	}()

	time.Sleep(300 * time.Millisecond)
	stdinW.Write([]byte("y"))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	entries, _ := audit.ReadMessageEntries(auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	if entries[0].Event != "message_sent" {
		t.Errorf("expected 'message_sent', got %q", entries[0].Event)
	}
	if entries[0].SessionID != "s1" {
		t.Errorf("expected session 's1', got %q", entries[0].SessionID)
	}
}

func TestCommsServer_SendMessage_AuditDenied(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-audit-deny.sock"
	t.Cleanup(func() { os.Remove(socketPath) })
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, copier, router, auditPath, "s1")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method: "send_message", Service: "slack", Channel: "#backend", Body: "denied msg",
		})
	}()

	time.Sleep(300 * time.Millisecond)
	stdinW.Write([]byte("n"))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	entries, _ := audit.ReadMessageEntries(auditPath)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	if entries[0].Event != "message_denied" {
		t.Errorf("expected 'message_denied', got %q", entries[0].Event)
	}
}

func TestCommsServer_UnknownMethod(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-unknown.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil, "", "")
	go srv.Serve()
	defer srv.Close()

	resp := launch.RequestComms(socketPath, launch.CommsRequest{Method: "bogus"})
	if resp.OK {
		t.Error("expected error for unknown method")
	}
}

func TestCommsServer_SocketPath(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-path.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)
	srv, err := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	if srv.SocketPath() != socketPath {
		t.Errorf("SocketPath = %q, want %q", srv.SocketPath(), socketPath)
	}
}

func TestRequestComms_NoSocket(t *testing.T) {
	resp := launch.RequestComms("/nonexistent/comms.sock", launch.CommsRequest{Method: "read_messages"})
	if resp.OK {
		t.Error("expected error when socket unavailable")
	}
}

func TestCommsServer_WrapText(t *testing.T) {
	// Verify long messages don't break the send approval prompt.
	socketPath := os.TempDir() + "/aileron-test-comms-wrap.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	dst := &safeBuf{}
	copier := launch.NewOutputCopier(srcR, dst, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, copier, router, "", "")
	go srv.Serve()
	defer srv.Close()

	longBody := "This is a very long message that should be wrapped across multiple lines in the approval prompt to ensure it fits within the box boundaries."

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "send_message",
			Service: "slack",
			Channel: "#backend",
			Body:    longBody,
		})
	}()

	time.Sleep(300 * time.Millisecond)
	stdinW.Write([]byte("y"))
	<-done

	if sender.lastMsg.Body != longBody {
		t.Error("full body should be sent regardless of wrapping")
	}
}

func TestCommsServer_DirectSend_Success(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-direct.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	srv, err := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	err = srv.DirectSend("slack", "#backend", "my reply")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !sender.sent {
		t.Error("expected sender.Send to be called")
	}
	if sender.lastMsg.Channel != "#backend" {
		t.Errorf("expected channel '#backend', got %q", sender.lastMsg.Channel)
	}
	if sender.lastMsg.Body != "my reply" {
		t.Errorf("expected body 'my reply', got %q", sender.lastMsg.Body)
	}
}

func TestCommsServer_HttpRequest_WithApproval(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-http.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	// Set up a mock HTTP server.
	mockAPI := &mockHTTPServer{responseBody: `{"ok": true}`, statusCode: 200}
	ts := mockAPI.start()
	defer ts.Close()

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	queue := launch.NewNotifyQueue(10, nil)
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, copier, router, "", "")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "http_request",
			Service: "GET",
			Channel: ts.URL + "/test",
		})
	}()

	time.Sleep(300 * time.Millisecond)
	stdinW.Write([]byte("y"))

	select {
	case resp := <-done:
		if !resp.OK {
			t.Errorf("expected OK, got error: %s", resp.Error)
		}
		if len(resp.Messages) == 0 {
			t.Fatal("expected response body in messages")
		}
		if !strings.Contains(resp.Messages[0].Body, "ok") {
			t.Errorf("expected response body containing 'ok', got %q", resp.Messages[0].Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestCommsServer_HttpRequest_WithHeadersAndBody(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-http-headers.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	mockAPI := &mockHTTPServer{
		handler: func(w http.ResponseWriter, r *http.Request) {
			ct := r.Header.Get("Content-Type")
			w.Write([]byte("ct:" + ct))
		},
	}
	ts := mockAPI.start()
	defer ts.Close()

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	queue := launch.NewNotifyQueue(10, nil)
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, copier, router, "", "")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "http_request",
			Service: "POST",
			Channel: ts.URL + "/api",
			Body:    `{"key":"value"}`,
			ReplyTo: `{"Content-Type":"application/json"}`,
		})
	}()

	time.Sleep(300 * time.Millisecond)
	stdinW.Write([]byte("y"))

	select {
	case resp := <-done:
		if !resp.OK {
			t.Fatalf("expected OK, got error: %s", resp.Error)
		}
		if !strings.Contains(resp.Messages[0].Body, "application/json") {
			t.Errorf("expected Content-Type header passed through, got %q", resp.Messages[0].Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestCommsServer_HttpRequest_Denied(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-http-deny.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	queue := launch.NewNotifyQueue(10, nil)
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, copier, router, "", "")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "http_request",
			Service: "GET",
			Channel: "https://example.com/api",
		})
	}()

	time.Sleep(300 * time.Millisecond)
	stdinW.Write([]byte("n"))

	select {
	case resp := <-done:
		if resp.OK {
			t.Error("expected denial")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestCommsServer_HttpRequest_MissingFields(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-http-missing.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil, "", "")
	go srv.Serve()
	defer srv.Close()

	resp := launch.RequestComms(socketPath, launch.CommsRequest{
		Method: "http_request",
		// Missing Service (method) and Channel (url)
	})
	if resp.OK {
		t.Error("expected error for missing fields")
	}
}

func TestCommsServer_HttpRequest_WithSecretInjection(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-http-secret.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	// Mock HTTP server that echoes the Authorization header.
	mockAPI := &mockHTTPServer{
		handler: func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("auth:" + r.Header.Get("Authorization")))
		},
	}
	ts := mockAPI.start()
	defer ts.Close()

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	queue := launch.NewNotifyQueue(10, nil)
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, copier, router, "", "")

	// Set up secrets with a vault.
	v := vault.NewMemVault()
	v.Put(nil, "SLACK_TOKEN", []byte("xoxb-secret-123"), vault.Metadata{})
	secrets := map[string]launchpolicy.SecretDef{
		"SLACK_TOKEN": {Targets: []string{"slack.com/api/*"}},
	}
	// The mock server URL won't match slack.com, so use a pattern that matches.
	secrets["SLACK_TOKEN"] = launchpolicy.SecretDef{Targets: []string{ts.URL[7:]}} // strip "http://"
	srv.SetSecrets(secrets, v)

	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "http_request",
			Service: "GET",
			Channel: ts.URL + "/api/test",
		})
	}()

	time.Sleep(300 * time.Millisecond)
	stdinW.Write([]byte("y"))

	select {
	case resp := <-done:
		if !resp.OK {
			t.Fatalf("expected OK, got error: %s", resp.Error)
		}
		if len(resp.Messages) == 0 {
			t.Fatal("expected response")
		}
		if !strings.Contains(resp.Messages[0].Body, "Bearer xoxb-secret-123") {
			t.Errorf("expected injected Bearer token, got %q", resp.Messages[0].Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestURLMatchesPattern(t *testing.T) {
	tests := []struct {
		url, pattern string
		want         bool
	}{
		{"https://slack.com/api/chat.postMessage", "slack.com/api/*", true},
		{"https://discord.com/api/channels", "discord.com/api/*", true},
		{"https://example.com/other", "slack.com/api/*", false},
		{"https://slack.com/api/users", "slack.com/api/users", true},
		{"https://example.com", "example.com", true},
	}
	for _, tt := range tests {
		got := launch.URLMatchesPattern(tt.url, tt.pattern)
		if got != tt.want {
			t.Errorf("URLMatchesPattern(%q, %q) = %v, want %v", tt.url, tt.pattern, got, tt.want)
		}
	}
}

func TestCommsServer_DirectSend_AuditLog(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-direct-audit.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	srv, err := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, nil, nil, auditPath, "test-session")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	srv.DirectSend("slack", "#backend", "my reply")

	entries, err := audit.ReadMessageEntries(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	if entries[0].Event != "reply_sent" {
		t.Errorf("expected event 'reply_sent', got %q", entries[0].Event)
	}
	if entries[0].SessionID != "test-session" {
		t.Errorf("expected session 'test-session', got %q", entries[0].SessionID)
	}
	if entries[0].Channel != "#backend" {
		t.Errorf("expected channel '#backend', got %q", entries[0].Channel)
	}
}

func TestCommsServer_SendMessage_DraftDetection(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-draft-detect.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	// Push an auto-draft message to the queue.
	queue.Push(launch.Message{
		ID: "orig-1", Source: "slack", Channel: "#backend",
		Author: "Alice", Body: "Is the deploy done?",
		AutoDraft: true, Timestamp: time.Now(),
	})

	srv, err := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	// Send a message to the same channel — should trigger draft detection.
	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "send_message",
			Service: "slack",
			Channel: "#backend",
			Body:    "Yes, the deploy is complete.",
		})
	}()

	// Give time for the draft to be queued.
	time.Sleep(300 * time.Millisecond)

	// Verify the draft was attached to the original message.
	msg, found := queue.FindByID("orig-1")
	if !found {
		t.Fatal("expected to find original message")
	}
	if msg.Draft != "Yes, the deploy is complete." {
		t.Errorf("Draft = %q, want 'Yes, the deploy is complete.'", msg.Draft)
	}

	// Approve the draft by setting the sentinel.
	queue.ApproveDraft("orig-1")

	select {
	case resp := <-done:
		if !resp.OK {
			t.Errorf("expected OK after approval, got error: %s", resp.Error)
		}
		if !sender.sent {
			t.Error("expected message to be sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestCommsServer_SendMessage_DraftDiscard(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-draft-discard.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	queue.Push(launch.Message{
		ID: "orig-1", Source: "slack", Channel: "#backend",
		Author: "Alice", Body: "Is the deploy done?",
		AutoDraft: true, Timestamp: time.Now(),
	})

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, nil, nil, "", "")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "send_message",
			Service: "slack",
			Channel: "#backend",
			Body:    "draft reply",
		})
	}()

	time.Sleep(300 * time.Millisecond)

	// Discard the draft by clearing it.
	queue.ClearDraft("orig-1")

	select {
	case resp := <-done:
		if resp.OK {
			t.Error("expected error after discard")
		}
		if sender.sent {
			t.Error("message should not have been sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestCommsServer_SendMessage_NoDraftForNonAutoDraft(t *testing.T) {
	// When no auto-draft message exists for the channel, the normal
	// approval prompt path should be used (which requires a router).
	socketPath := os.TempDir() + "/aileron-test-comms-nodraft.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	router := launch.NewKeyRouter(stdinR, &safeBuf{}, &simpleOverlay{})
	go router.Run()

	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	// Push a NON-auto-draft message — should not trigger draft detection.
	queue.Push(launch.Message{
		ID: "1", Source: "slack", Channel: "#backend",
		AutoDraft: false, Timestamp: time.Now(),
	})

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, copier, router, "", "")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "send_message",
			Service: "slack",
			Channel: "#backend",
			Body:    "reply",
		})
	}()

	// Should go through normal approval prompt.
	time.Sleep(300 * time.Millisecond)
	stdinW.Write([]byte("y"))

	select {
	case resp := <-done:
		if !resp.OK {
			t.Errorf("expected OK after normal approval, got error: %s", resp.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestCommsServer_DirectSend_UnknownService(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-direct-unknown.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)

	srv, err := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	err = srv.DirectSend("nonexistent", "#test", "hello")
	if err == nil {
		t.Fatal("expected error for unknown service")
	}
}

func TestResolveDraft_NotFound(t *testing.T) {
	decision := launch.ResolveDraft(launch.Message{}, false, "original")
	if decision != launch.DraftDiscard {
		t.Errorf("expected DraftDiscard, got %q", decision)
	}
}

func TestResolveDraft_DraftCleared(t *testing.T) {
	msg := launch.Message{ID: "1", Draft: "", DraftFor: ""}
	decision := launch.ResolveDraft(msg, true, "original")
	if decision != launch.DraftDiscard {
		t.Errorf("expected DraftDiscard, got %q", decision)
	}
}

func TestResolveDraft_Approved(t *testing.T) {
	msg := launch.Message{ID: "1", Draft: "\x00approved", DraftFor: "1"}
	decision := launch.ResolveDraft(msg, true, "original")
	if decision != launch.DraftApprove {
		t.Errorf("expected DraftApprove, got %q", decision)
	}
}

func TestResolveDraft_Edited(t *testing.T) {
	msg := launch.Message{ID: "1", Draft: "edited text", DraftFor: "1"}
	decision := launch.ResolveDraft(msg, true, "original")
	if decision != launch.DraftEdited {
		t.Errorf("expected DraftEdited, got %q", decision)
	}
}

func TestResolveDraft_Pending(t *testing.T) {
	msg := launch.Message{ID: "1", Draft: "original", DraftFor: "1"}
	decision := launch.ResolveDraft(msg, true, "original")
	if decision != launch.DraftPending {
		t.Errorf("expected DraftPending, got %q", decision)
	}
}

type failSender struct {
	service string
	err     error
}

func (f *failSender) Service() string                                              { return f.service }
func (f *failSender) Connect(ctx context.Context) error                            { return nil }
func (f *failSender) Listen(ctx context.Context) (<-chan comms.IncomingMessage, error) { return nil, nil }
func (f *failSender) Send(ctx context.Context, msg comms.OutgoingMessage) error    { return f.err }
func (f *failSender) Close() error                                                 { return nil }

func TestCommsServer_SendMessage_DraftApprove_SendFailure(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-draft-sendfail.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	sender := &failSender{service: "slack", err: fmt.Errorf("network error")}
	queue := launch.NewNotifyQueue(10, nil)

	queue.Push(launch.Message{
		ID: "orig-1", Source: "slack", Channel: "#backend",
		Author: "Alice", Body: "question?",
		AutoDraft: true, Timestamp: time.Now(),
	})

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, nil, nil, "", "")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "send_message",
			Service: "slack",
			Channel: "#backend",
			Body:    "draft reply",
		})
	}()

	time.Sleep(300 * time.Millisecond)

	// Approve the draft.
	queue.ApproveDraft("orig-1")

	select {
	case resp := <-done:
		if resp.OK {
			t.Error("expected error when send fails")
		}
		if !strings.Contains(resp.Error, "send failed") {
			t.Errorf("expected 'send failed' error, got %q", resp.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestCommsServer_SendMessage_DraftEdited(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-draft-edited.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	sender := &mockSender{service: "slack"}
	queue := launch.NewNotifyQueue(10, nil)

	queue.Push(launch.Message{
		ID: "orig-1", Source: "slack", Channel: "#backend",
		Author: "Alice", Body: "question?",
		AutoDraft: true, Timestamp: time.Now(),
	})

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, nil, nil, auditPath, "s1")
	go srv.Serve()
	defer srv.Close()

	done := make(chan launch.CommsResponse, 1)
	go func() {
		done <- launch.RequestComms(socketPath, launch.CommsRequest{
			Method:  "send_message",
			Service: "slack",
			Channel: "#backend",
			Body:    "original draft",
		})
	}()

	time.Sleep(300 * time.Millisecond)

	// Simulate the user editing the draft to different text.
	queue.SetDraft("orig-1", "edited draft text")

	select {
	case resp := <-done:
		if !resp.OK {
			t.Errorf("expected OK after edit, got error: %s", resp.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	// Verify audit log.
	entries, _ := audit.ReadMessageEntries(auditPath)
	foundEdited := false
	for _, e := range entries {
		if e.Event == "draft_edited" {
			foundEdited = true
		}
	}
	if !foundEdited {
		t.Error("expected 'draft_edited' audit entry")
	}
}

func TestCommsServer_ReadMessages_FilterByChannel(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-filter-channel.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)
	queue.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Body: "msg1"})
	queue.Push(launch.Message{ID: "2", Source: "slack", Channel: "#frontend", Body: "msg2"})

	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil, "", "")
	go srv.Serve()
	defer srv.Close()

	resp := launch.RequestComms(socketPath, launch.CommsRequest{
		Method:  "read_messages",
		Channel: "#frontend",
	})
	if len(resp.Messages) != 1 || resp.Messages[0].Channel != "#frontend" {
		t.Errorf("expected 1 #frontend message, got %v", resp.Messages)
	}
}

func TestCommsServer_DraftReply(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-draftreply.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	queue := launch.NewNotifyQueue(10, nil)
	queue.Push(launch.Message{ID: "msg-1", Source: "slack", Channel: "#backend", Author: "Alice", Body: "question?"})

	srv, err := launch.NewCommsServer(socketPath, queue, nil, nil, copier, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Serve()
	time.Sleep(100 * time.Millisecond)

	resp := launch.RequestComms(socketPath, launch.CommsRequest{
		Method:  "draft_reply",
		ReplyTo: "msg-1",
		Body:    "Here is my draft.",
	})
	if !resp.OK {
		t.Fatalf("expected OK, got error: %s", resp.Error)
	}

	// Verify draft was set on the message.
	msg, ok := queue.FindByID("msg-1")
	if !ok {
		t.Fatal("message not found")
	}
	if msg.Draft != "Here is my draft." {
		t.Errorf("expected draft text, got %q", msg.Draft)
	}
}

func TestCommsServer_DraftReply_MissingFields(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-draftreply-missing.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	srv, err := launch.NewCommsServer(socketPath, launch.NewNotifyQueue(10, nil), nil, nil, copier, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Serve()
	time.Sleep(100 * time.Millisecond)

	resp := launch.RequestComms(socketPath, launch.CommsRequest{
		Method: "draft_reply",
	})
	if resp.OK {
		t.Error("expected error for missing fields")
	}
}

func TestCommsServer_DraftReply_NotFound(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-draftreply-notfound.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	srv, err := launch.NewCommsServer(socketPath, launch.NewNotifyQueue(10, nil), nil, nil, copier, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Serve()
	time.Sleep(100 * time.Millisecond)

	resp := launch.RequestComms(socketPath, launch.CommsRequest{
		Method:  "draft_reply",
		ReplyTo: "nonexistent",
		Body:    "draft text",
	})
	if resp.OK {
		t.Error("expected error for nonexistent message")
	}
}

func TestCommsServer_ReadMessages_DraftRequestFlag(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-draftflag.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	srcR, srcW := io.Pipe()
	defer srcW.Close()
	copier := launch.NewOutputCopier(srcR, &safeBuf{}, nil)
	go copier.Run()

	queue := launch.NewNotifyQueue(10, nil)
	queue.Push(launch.Message{ID: "1", Source: "slack", Channel: "#ch", AutoDraft: true})
	queue.Push(launch.Message{ID: "2", Source: "slack", Channel: "#ch", AutoDraft: false})

	srv, err := launch.NewCommsServer(socketPath, queue, nil, nil, copier, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Serve()
	time.Sleep(100 * time.Millisecond)

	resp := launch.RequestComms(socketPath, launch.CommsRequest{Method: "read_messages"})
	if len(resp.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(resp.Messages))
	}
	if !resp.Messages[0].DraftRequest {
		t.Error("expected DraftRequest=true on auto-draft message")
	}
	if resp.Messages[1].DraftRequest {
		t.Error("expected DraftRequest=false on non-auto-draft message")
	}
}
