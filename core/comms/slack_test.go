package comms_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func debugLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

func TestNewSlackListener(t *testing.T) {
	sl := comms.NewSlackListener(
		"xapp-test",
		"xoxb-test",
		"",
		[]string{"#backend", "#incidents"},
		[]string{"#random"},
		nopLogger(),
	)
	if sl.Service() != "slack" {
		t.Errorf("Service() = %q, want 'slack'", sl.Service())
	}
}

func TestSlackListener_ConnectMissingTokens(t *testing.T) {
	sl := comms.NewSlackListener("", "", "", nil, nil, nopLogger())
	err := sl.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error for missing tokens")
	}
}

func TestSlackListener_ConnectValidTokens(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger())
	err := sl.Connect(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestSlackListener_ConnectWithUserToken(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "xoxp-user", nil, nil, nopLogger())
	err := sl.Connect(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	// Send should use the user token client (we can't verify the token
	// directly, but Connect should not error with a user token set).
}

func TestSlackListener_SendWithoutConnect_UserToken(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "xoxp-user", nil, nil, nopLogger())
	// Don't call Connect — sendAPI is nil.
	err := sl.Send(context.Background(), comms.OutgoingMessage{Channel: "#test", Body: "hi"})
	if err == nil {
		t.Fatal("expected error when sending without connect")
	}
}

func TestSlackListener_ListenWithoutConnect(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger())
	// Don't call Connect.
	_, err := sl.Listen(context.Background())
	if err == nil {
		t.Fatal("expected error when listening without connect")
	}
}

func TestSlackListener_SendWithoutConnect(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger())
	err := sl.Send(context.Background(), comms.OutgoingMessage{
		Channel: "#test",
		Body:    "hello",
	})
	if err == nil {
		t.Fatal("expected error when sending without connect")
	}
}

func TestSlackListener_Close(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger())
	// Close without connect should not panic.
	if err := sl.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestSlackListener_ProcessMessageEvent(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "",
		[]string{"C123"}, []string{"C999"}, nopLogger())

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
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger())

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
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, []string{"C999"}, nopLogger())

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
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "",
		[]string{"C123"}, nil, nopLogger()) // only listen on C123

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
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger())

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
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger()) // no channel filter

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

func TestSlackListener_DebugLogging(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "",
		[]string{"C123"}, []string{"C999"}, debugLogger())

	// Connect — exercises the "connected" log line.
	if err := sl.Connect(context.Background()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	// Bot message — exercises "skipping bot message" log.
	sl.ProcessMessageEvent(&slackevents.MessageEvent{
		BotID: "B1", Channel: "C123", Text: "bot",
	})
	// Ignored channel — exercises "skipping ignored channel" log.
	sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User: "U1", Channel: "C999", Text: "ignored",
	})
	// Unconfigured channel — exercises "skipping unconfigured channel" log.
	sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User: "U1", Channel: "C_OTHER", Text: "other",
	})
	// Delivered message — exercises "delivering message" log.
	msg, ok := sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User: "U1", Channel: "C123", Text: "hello", TimeStamp: "123.456",
	})
	if !ok {
		t.Fatal("expected message to be delivered")
	}
	if msg.Body != "hello" {
		t.Errorf("Body = %q", msg.Body)
	}
}

func TestSlackListener_HandleEvent_NonMessageTypes(t *testing.T) {
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, debugLogger())
	if err := sl.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	msgs := make(chan comms.IncomingMessage, 10)

	// Exercise all non-message event types via handleEvent.
	eventTypes := []socketmode.EventType{
		socketmode.EventTypeConnectionError,
		socketmode.EventTypeConnecting,
		socketmode.EventTypeConnected,
		socketmode.EventTypeDisconnect,
		socketmode.EventTypeHello,
		"unknown_event_type", // default case
	}
	for _, et := range eventTypes {
		sl.HandleEvent(socketmode.Event{Type: et}, msgs)
	}

	// Also test EventsAPI with wrong data type.
	sl.HandleEvent(socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: "not-an-events-api-event",
	}, msgs)

	// No messages should have been delivered.
	if len(msgs) != 0 {
		t.Errorf("expected no messages, got %d", len(msgs))
	}
}

func TestSlackListener_ChannelNameMatching(t *testing.T) {
	// Config uses channel name "#backend" but Slack sends channel ID "C123".
	// Without API connection, resolveChannelName returns raw ID, so the
	// channel filter uses both ID and name for matching.
	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "",
		[]string{"#backend"}, nil, nopLogger())

	// Channel ID "C123" doesn't match "#backend" by ID, and without
	// an API client to resolve the name, it falls through.
	_, ok := sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User: "U1", Channel: "C123", Text: "hello",
	})
	if ok {
		t.Error("without API resolution, channel ID should not match channel name config")
	}

	// Channel name "#backend" should match directly.
	_, ok = sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User: "U1", Channel: "#backend", Text: "hello",
	})
	if !ok {
		t.Error("channel name should match directly")
	}
}

// Verify SlackListener implements the Listener interface.
var _ comms.Listener = (*comms.SlackListener)(nil)
