package comms_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/bwmarrin/discordgo"
)

func TestNewDiscordListener(t *testing.T) {
	dl := comms.NewDiscordListener(
		"bot-token-test",
		[]string{"123456", "789012"},
		[]string{"999999"},
		nopLogger(),
	)
	if dl.Service() != "discord" {
		t.Errorf("Service() = %q, want 'discord'", dl.Service())
	}
}

func TestDiscordListener_ConnectMissingToken(t *testing.T) {
	dl := comms.NewDiscordListener("", nil, nil, nopLogger())
	err := dl.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestDiscordListener_ConnectValidToken(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())
	err := dl.Connect(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestDiscordListener_ListenWithoutConnect(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())
	_, err := dl.Listen(context.Background())
	if err == nil {
		t.Fatal("expected error when listening without connect")
	}
}

func TestDiscordListener_SendWithoutConnect(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())
	err := dl.Send(context.Background(), comms.OutgoingMessage{
		Channel: "123456",
		Body:    "hello",
	})
	if err == nil {
		t.Fatal("expected error when sending without connect")
	}
}

func TestDiscordListener_Close(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())
	// Close without connect should not panic.
	if err := dl.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestDiscordListener_ProcessMessageCreate(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test",
		[]string{"C123"}, []string{"C999"}, nopLogger())

	msg, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-1",
			ChannelID: "C123",
			Content:   "Is the deploy blocked?",
			Author: &discordgo.User{
				ID:         "U123",
				Username:   "alice",
				GlobalName: "Alice Smith",
				Bot:        false,
			},
		},
	})
	if !ok {
		t.Fatal("expected message to be delivered")
	}
	if msg.Service != "discord" {
		t.Errorf("Service = %q, want 'discord'", msg.Service)
	}
	if msg.Body != "Is the deploy blocked?" {
		t.Errorf("Body = %q", msg.Body)
	}
	if msg.Author != "Alice Smith" {
		t.Errorf("Author = %q, want 'Alice Smith'", msg.Author)
	}
	if msg.ID != "msg-1" {
		t.Errorf("ID = %q, want 'msg-1'", msg.ID)
	}
}

func TestDiscordListener_ProcessMessageCreate_BotSkipped(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())

	_, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C123",
			Content:   "bot message",
			Author: &discordgo.User{
				ID:  "U123",
				Bot: true,
			},
		},
	})
	if ok {
		t.Error("bot messages should be skipped")
	}
}

func TestDiscordListener_ProcessMessageCreate_NilAuthor(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())

	_, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C123",
			Content:   "no author",
			Author:    nil,
		},
	})
	if ok {
		t.Error("messages with nil author should be skipped")
	}
}

func TestDiscordListener_ProcessMessageCreate_IgnoredChannel(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, []string{"C999"}, nopLogger())

	_, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C999",
			Content:   "ignored",
			Author:    &discordgo.User{ID: "U1", Username: "u"},
		},
	})
	if ok {
		t.Error("messages from ignored channels should be skipped")
	}
}

func TestDiscordListener_ProcessMessageCreate_UnlistedChannel(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test",
		[]string{"C123"}, nil, nopLogger()) // only listen on C123

	_, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C456",
			Content:   "wrong channel",
			Author:    &discordgo.User{ID: "U1", Username: "u"},
		},
	})
	if ok {
		t.Error("messages from unlisted channels should be skipped")
	}
}

func TestDiscordListener_ProcessMessageCreate_NoChannelFilter(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())

	_, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C_ANY",
			Content:   "any channel",
			Author:    &discordgo.User{ID: "U1", Username: "u"},
		},
	})
	if !ok {
		t.Error("with no channel filter, all channels should be accepted")
	}
}

func TestDiscordListener_ProcessMessageCreate_NicknamePriority(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())

	msg, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C123",
			Content:   "test",
			Author: &discordgo.User{
				ID:         "U1",
				Username:   "alice",
				GlobalName: "Alice Smith",
			},
			Member: &discordgo.Member{
				Nick: "Ally",
			},
		},
	})
	if !ok {
		t.Fatal("expected delivery")
	}
	if msg.Author != "Ally" {
		t.Errorf("Author = %q, want 'Ally' (guild nickname)", msg.Author)
	}
}

func TestDiscordListener_ProcessMessageCreate_FallbackToUsername(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())

	msg, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C123",
			Content:   "test",
			Author: &discordgo.User{
				ID:       "U1",
				Username: "bob_handle",
			},
		},
	})
	if !ok {
		t.Fatal("expected delivery")
	}
	if msg.Author != "bob_handle" {
		t.Errorf("Author = %q, want 'bob_handle' (fallback from empty GlobalName)", msg.Author)
	}
}

func TestDiscordListener_ListenSuccess(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())
	if err := dl.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Skip real gateway — inject a no-op opener.
	dl.SetOpenGateway(func() error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msgs, err := dl.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	if msgs == nil {
		t.Fatal("expected non-nil message channel")
	}

	// Cancel context to trigger cleanup goroutine.
	cancel()
	// Drain the channel to confirm it closes.
	for range msgs {
	}
}

func TestDiscordListener_ListenOpenFails(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())
	if err := dl.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Listen will try to open the gateway with a fake token, which should fail.
	_, err := dl.Listen(context.Background())
	if err == nil {
		t.Fatal("expected error when gateway open fails with invalid token")
	}
}

func TestDiscordListener_ServiceName(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())
	if got := dl.Service(); got != "discord" {
		t.Errorf("Service() = %q, want 'discord'", got)
	}
}

func TestDiscordListener_SetEndpointURL_NilSession(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())
	// Should not panic when session is nil.
	dl.SetEndpointURL("http://localhost:1234/")
}

func TestDiscordListener_CloseAfterConnect(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())
	if err := dl.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Close with an active session (not opened to gateway, but non-nil).
	if err := dl.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestDiscordListener_ProcessMessageCreate_NilAuthorEmptyResult(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil, nopLogger())

	// Message with a non-bot author but nil Member — exercises the
	// resolveAuthor path where Author is non-nil but Member is nil
	// and GlobalName is empty, falling through to return "".
	msg, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C123",
			Content:   "test",
			Author: &discordgo.User{
				ID: "U1",
				// Username, GlobalName both empty.
			},
		},
	})
	if !ok {
		t.Fatal("expected delivery")
	}
	// With empty Username and GlobalName and nil Member, author should be "".
	if msg.Author != "" {
		t.Errorf("Author = %q, want empty string", msg.Author)
	}
}

func TestDiscordListener_DebugLogging(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test",
		[]string{"C123"}, []string{"C999"},
		slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))

	// Connect — exercises the "connected" log line.
	if err := dl.Connect(context.Background()); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	// Bot message.
	dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C123", Content: "bot",
			Author: &discordgo.User{ID: "U1", Bot: true},
		},
	})
	// Ignored channel.
	dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C999", Content: "ignored",
			Author: &discordgo.User{ID: "U1", Username: "u"},
		},
	})
	// Unconfigured channel.
	dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "C_OTHER", Content: "other",
			Author: &discordgo.User{ID: "U1", Username: "u"},
		},
	})
	// Delivered message.
	msg, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID: "msg-1", ChannelID: "C123", Content: "hello",
			Author: &discordgo.User{ID: "U1", Username: "alice"},
		},
	})
	if !ok {
		t.Fatal("expected message to be delivered")
	}
	if msg.Body != "hello" {
		t.Errorf("Body = %q", msg.Body)
	}
}

// Verify DiscordListener implements the Listener interface.
var _ comms.Listener = (*comms.DiscordListener)(nil)
