package comms_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/slack-go/slack/slackevents"
)

// newMockSlackServer returns an httptest.Server that handles the Slack
// API endpoints used by SlackListener.
func newMockSlackServer() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"channel": map[string]any{
				"id":   "C123",
				"name": "backend",
			},
		})
	})

	mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"user": map[string]any{
				"id":        "U456",
				"name":      "alice",
				"real_name": "Alice Smith",
			},
		})
	})

	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": r.FormValue("channel"),
			"ts":      "1234567890.123456",
		})
	})

	// Catch-all for auth.test and other endpoints.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	return httptest.NewServer(mux)
}

func TestSlackListener_ResolveChannelAndAuthor(t *testing.T) {
	srv := newMockSlackServer()
	defer srv.Close()

	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger())
	sl.SetAPIURL(srv.URL + "/")

	if err := sl.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	msg, ok := sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User:      "U456",
		Channel:   "C123",
		Text:      "Hello from integration test",
		TimeStamp: "1234567890.000001",
	})
	if !ok {
		t.Fatal("expected message to be delivered")
	}

	// With the mock API, channel should be resolved to "#backend".
	if msg.Channel != "#backend" {
		t.Errorf("Channel = %q, want '#backend'", msg.Channel)
	}

	// Author should be resolved to "Alice Smith".
	if msg.Author != "Alice Smith" {
		t.Errorf("Author = %q, want 'Alice Smith'", msg.Author)
	}

	if msg.Body != "Hello from integration test" {
		t.Errorf("Body = %q", msg.Body)
	}
}

func TestSlackListener_ResolveAuthorFallbackToName(t *testing.T) {
	// Server returns a user with empty real_name.
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": map[string]any{"id": "C1", "name": "ch"},
		})
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"user": map[string]any{
				"id":        "U1",
				"name":      "bob_handle",
				"real_name": "",
			},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger())
	sl.SetAPIURL(srv.URL + "/")
	sl.Connect(context.Background())

	msg, ok := sl.ProcessMessageEvent(&slackevents.MessageEvent{
		User:    "U1",
		Channel: "C1",
		Text:    "test",
	})
	if !ok {
		t.Fatal("expected delivery")
	}
	if msg.Author != "bob_handle" {
		t.Errorf("Author = %q, want 'bob_handle' (fallback from empty real_name)", msg.Author)
	}
}

func TestSlackListener_Send(t *testing.T) {
	srv := newMockSlackServer()
	defer srv.Close()

	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger())
	sl.SetAPIURL(srv.URL + "/")
	sl.Connect(context.Background())

	err := sl.Send(context.Background(), comms.OutgoingMessage{
		Channel: "#backend",
		Body:    "Test message from Aileron",
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
}

func TestSlackListener_SendConnected(t *testing.T) {
	srv := newMockSlackServer()
	defer srv.Close()

	sl := comms.NewSlackListener("xapp-test", "xoxb-test", "", nil, nil, nopLogger())
	sl.SetAPIURL(srv.URL + "/")
	sl.Connect(context.Background())

	// Second send to exercise the connected path.
	err := sl.Send(context.Background(), comms.OutgoingMessage{
		Channel: "C123",
		Body:    "Another message",
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
}
