package comms_test

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/bwmarrin/discordgo"
)

func TestNewDiscordListener(t *testing.T) {
	dl := comms.NewDiscordListener(
		"bot-token-test",
		[]string{"123456", "789012"},
		[]string{"999999"},
	)
	if dl.Service() != "discord" {
		t.Errorf("Service() = %q, want 'discord'", dl.Service())
	}
}

func TestDiscordListener_ConnectMissingToken(t *testing.T) {
	dl := comms.NewDiscordListener("", nil, nil)
	err := dl.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestDiscordListener_ConnectValidToken(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil)
	err := dl.Connect(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestDiscordListener_ListenWithoutConnect(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil)
	_, err := dl.Listen(context.Background())
	if err == nil {
		t.Fatal("expected error when listening without connect")
	}
}

func TestDiscordListener_SendWithoutConnect(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil)
	err := dl.Send(context.Background(), comms.OutgoingMessage{
		Channel: "123456",
		Body:    "hello",
	})
	if err == nil {
		t.Fatal("expected error when sending without connect")
	}
}

func TestDiscordListener_Close(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test", nil, nil)
	// Close without connect should not panic.
	if err := dl.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestDiscordListener_ProcessMessageCreate(t *testing.T) {
	dl := comms.NewDiscordListener("bot-token-test",
		[]string{"C123"}, []string{"C999"})

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
	dl := comms.NewDiscordListener("bot-token-test", nil, nil)

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
	dl := comms.NewDiscordListener("bot-token-test", nil, nil)

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
	dl := comms.NewDiscordListener("bot-token-test", nil, []string{"C999"})

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
		[]string{"C123"}, nil) // only listen on C123

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
	dl := comms.NewDiscordListener("bot-token-test", nil, nil)

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
	dl := comms.NewDiscordListener("bot-token-test", nil, nil)

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
	dl := comms.NewDiscordListener("bot-token-test", nil, nil)

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

// Verify DiscordListener implements the Listener interface.
var _ comms.Listener = (*comms.DiscordListener)(nil)
