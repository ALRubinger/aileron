package comms_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
	"github.com/bwmarrin/discordgo"
)

// newMockDiscordServer returns an httptest.Server that handles the Discord
// REST API endpoints used by DiscordListener.
func newMockDiscordServer() *httptest.Server {
	mux := http.NewServeMux()

	// Channel info endpoint.
	mux.HandleFunc("/api/v9/channels/C123", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(discordgo.Channel{
				ID:   "C123",
				Name: "dev-chat",
			})
			return
		}
		// POST to /channels/C123/messages is handled below.
		w.WriteHeader(http.StatusNotFound)
	})

	// Send message endpoint.
	mux.HandleFunc("/api/v9/channels/C123/messages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(discordgo.Message{
			ID:        "sent-1",
			ChannelID: "C123",
			Content:   "ok",
		})
	})

	// Gateway endpoint.
	mux.HandleFunc("/api/v9/gateway", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"url": "wss://gateway.discord.gg",
		})
	})

	// Catch-all.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	return httptest.NewServer(mux)
}

// connectWithMockAPI creates a DiscordListener, connects it, and overrides
// endpoints to point at the mock server.
func connectWithMockAPI(t *testing.T, srv *httptest.Server, channels, ignore []string) *comms.DiscordListener {
	t.Helper()
	dl := comms.NewDiscordListener("bot-token-test", channels, ignore)
	if err := dl.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	dl.SetEndpointURL(srv.URL + "/")
	return dl
}

func TestDiscordListener_ResolveChannelName(t *testing.T) {
	srv := newMockDiscordServer()
	defer srv.Close()

	dl := connectWithMockAPI(t, srv, nil, nil)

	msg, ok := dl.ProcessMessageCreate(&discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-1",
			ChannelID: "C123",
			Content:   "Hello from integration test",
			Author:    &discordgo.User{ID: "U1", Username: "alice", GlobalName: "Alice Smith"},
		},
	})
	if !ok {
		t.Fatal("expected message to be delivered")
	}

	if msg.Channel != "#dev-chat" {
		t.Errorf("Channel = %q, want '#dev-chat'", msg.Channel)
	}
	if msg.Author != "Alice Smith" {
		t.Errorf("Author = %q, want 'Alice Smith'", msg.Author)
	}
}

func TestDiscordListener_Send(t *testing.T) {
	srv := newMockDiscordServer()
	defer srv.Close()

	dl := connectWithMockAPI(t, srv, nil, nil)

	err := dl.Send(context.Background(), comms.OutgoingMessage{
		Channel: "C123",
		Body:    "Test message from Aileron",
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
}

func TestDiscordListener_SendMultiple(t *testing.T) {
	srv := newMockDiscordServer()
	defer srv.Close()

	dl := connectWithMockAPI(t, srv, nil, nil)

	for i := 0; i < 3; i++ {
		err := dl.Send(context.Background(), comms.OutgoingMessage{
			Channel: "C123",
			Body:    "message",
		})
		if err != nil {
			t.Fatalf("Send #%d failed: %v", i, err)
		}
	}
}
