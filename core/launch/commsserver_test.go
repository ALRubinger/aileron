package launch_test

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/ALRubinger/aileron/core/launch"
)

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

	srv, err := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil)
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

	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil)
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

	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil)
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
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil)
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
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil)
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

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, copier, router)
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

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, copier, router)
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

func TestCommsServer_UnknownMethod(t *testing.T) {
	socketPath := os.TempDir() + "/aileron-test-comms-unknown.sock"
	t.Cleanup(func() { os.Remove(socketPath) })

	queue := launch.NewNotifyQueue(10, nil)
	srv, _ := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil)
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
	srv, err := launch.NewCommsServer(socketPath, queue, nil, nil, nil, nil)
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

	srv, _ := launch.NewCommsServer(socketPath, queue, []comms.Listener{sender}, nil, copier, router)
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
