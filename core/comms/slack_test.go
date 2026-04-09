package comms_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/slack-go/slack/slackevents"
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

func TestSlackListener_ProcessMessageEvent(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test",
		[]string{"C123"}, []string{"C999"})

	msg, ok := sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User:      "U123",
		Channel:   "C123",
		Text:      "Is the deploy blocked?",
		TimeStamp: "1234567890.123456",
	})
	if !ok {
		t.Fatal("expected message to be delivered")
	}
	if msg.Service != "slack" {
		t.Errorf("Service = %q, want 'slack'", msg.Service)
	}
	if msg.Body != "Is the deploy blocked?" {
		t.Errorf("Body = %q", msg.Body)
	}
	if msg.Author != "U123" {
		t.Errorf("Author = %q, want 'U123' (no API to resolve)", msg.Author)
	}
}

func TestSlackListener_ProcessMessageEvent_BotSkipped(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", nil, nil)

	_, ok := sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User:    "U123",
		Channel: "C123",
		BotID:   "B456",
		Text:    "bot message",
	})
	if ok {
		t.Error("bot messages should be skipped")
	}
}

func TestSlackListener_ProcessMessageEvent_IgnoredChannel(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", nil, []string{"C999"})

	_, ok := sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User:    "U123",
		Channel: "C999",
		Text:    "ignored",
	})
	if ok {
		t.Error("messages from ignored channels should be skipped")
	}
}

func TestSlackListener_ProcessMessageEvent_UnlistedChannel(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test",
		[]string{"C123"}, nil) // only listen on C123

	_, ok := sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User:    "U123",
		Channel: "C456", // not in the listen list
		Text:    "wrong channel",
	})
	if ok {
		t.Error("messages from unlisted channels should be skipped")
	}
}

func TestSlackListener_ProcessMessageEvent_LongPreview(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", nil, nil)

	longText := strings.Repeat("x", 100)
	msg, ok := sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User:    "U123",
		Channel: "C123",
		Text:    longText,
	})
	if !ok {
		t.Fatal("expected delivery")
	}
	if msg.Body != longText {
		t.Error("full body should be preserved")
	}
}

func TestSlackListener_ProcessMessageEvent_NoChannelFilter(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", nil, nil) // no channel filter

	_, ok := sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User:    "U123",
		Channel: "C_ANY",
		Text:    "any channel",
	})
	if !ok {
		t.Error("with no channel filter, all channels should be accepted")
	}
}

func TestBuildIncomingMessage(t *testing.T) {
	msg := comms.BuildIncomingMessage("12345.67", "#backend", "Alice", "Hello world")
	if msg.ID != "12345.67" {
		t.Errorf("ID = %q", msg.ID)
	}
	if msg.Channel != "#backend" {
		t.Errorf("Channel = %q", msg.Channel)
	}
	if msg.Author != "Alice" {
		t.Errorf("Author = %q", msg.Author)
	}
	if msg.Body != "Hello world" {
		t.Errorf("Body = %q", msg.Body)
	}
	if msg.Service != "slack" {
		t.Errorf("Service = %q", msg.Service)
	}
}

func TestBuildIncomingMessage_LongTextTruncated(t *testing.T) {
	longText := strings.Repeat("x", 100)
	msg := comms.BuildIncomingMessage("1", "#ch", "u", longText)
	if msg.Body != longText {
		t.Error("body should be full text")
	}
	// Preview is internal to the IncomingMessage — Body is preserved.
}

// Verify SlackListener implements the Listener interface.
var _ comms.Listener = (*comms.SlackListener)(nil)
