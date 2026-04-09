package comms_test

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/comms"
)

// mockListener implements Listener for testing.
type mockListener struct {
	service   string
	connected bool
	msgs      chan comms.IncomingMessage
	closed    bool
}

func newMockListener(service string) *mockListener {
	return &mockListener{
		service: service,
		msgs:    make(chan comms.IncomingMessage, 10),
	}
}

func (m *mockListener) Service() string { return m.service }

func (m *mockListener) Connect(ctx context.Context) error {
	m.connected = true
	return nil
}

func (m *mockListener) Listen(ctx context.Context) (<-chan comms.IncomingMessage, error) {
	return m.msgs, nil
}

func (m *mockListener) Send(ctx context.Context, msg comms.OutgoingMessage) error {
	return nil
}

func (m *mockListener) Close() error {
	m.closed = true
	close(m.msgs)
	return nil
}

func TestMockListener_ImplementsInterface(t *testing.T) {
	var _ comms.Listener = newMockListener("test")
}

func TestMockListener_DeliverMessages(t *testing.T) {
	ml := newMockListener("test")
	ml.Connect(context.Background())
	ch, _ := ml.Listen(context.Background())

	ml.msgs <- comms.IncomingMessage{
		ID:      "1",
		Service: "test",
		Channel: "#general",
		Author:  "Alice",
		Body:    "hello world",
	}

	select {
	case msg := <-ch:
		if msg.Author != "Alice" {
			t.Errorf("Author = %q, want Alice", msg.Author)
		}
		if msg.Body != "hello world" {
			t.Errorf("Body = %q, want 'hello world'", msg.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}
