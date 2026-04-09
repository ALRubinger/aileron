package comms_test

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
)

func TestNewSlackListener(t *testing.T) {
	sl := comms.NewSlackListener(
		"xapp-test",
		"xoxb-test",
		[]string{"#backend", "#incidents"},
		[]string{"#random"},
	)
	if sl.Service() != "slack" {
		t.Errorf("Service() = %q, want 'slack'", sl.Service())
	}
}

func TestSlackListener_ConnectMissingTokens(t *testing.T) {
	sl := comms.NewSlackListener("", "", nil, nil)
	err := sl.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error for missing tokens")
	}
}

func TestSlackListener_ConnectValidTokens(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", nil, nil)
	err := sl.Connect(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestSlackListener_ListenWithoutConnect(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", nil, nil)
	// Don't call Connect.
	_, err := sl.Listen(context.Background())
	if err == nil {
		t.Fatal("expected error when listening without connect")
	}
}

func TestSlackListener_SendWithoutConnect(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", nil, nil)
	err := sl.Send(context.Background(), comms.OutgoingMessage{
		Channel: "#test",
		Body:    "hello",
	})
	if err == nil {
		t.Fatal("expected error when sending without connect")
	}
}

func TestSlackListener_Close(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", nil, nil)
	// Close without connect should not panic.
	if err := sl.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

// Verify SlackListener implements the Listener interface.
var _ comms.Listener = (*comms.SlackListener)(nil)
