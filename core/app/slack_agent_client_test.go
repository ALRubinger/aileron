package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
)

func newTestSlackServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": "C123",
			"ts":      "999.001",
		})
	}))
}

func TestDefaultSlackAgentClient_SetStatus(t *testing.T) {
	server := newTestSlackServer()
	defer server.Close()
	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	c := defaultSlackAgentClient{}
	err := c.SetStatus(context.Background(), "xoxb-test", "C123", "123.456", "Thinking...")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDefaultSlackAgentClient_SetSuggestedPrompts(t *testing.T) {
	server := newTestSlackServer()
	defer server.Close()
	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	c := defaultSlackAgentClient{}
	err := c.SetSuggestedPrompts(context.Background(), "xoxb-test", "C123", "123.456", []comms.SlackAgentPrompt{
		{Title: "Test", Message: "Test prompt"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDefaultSlackAgentClient_SetTitle(t *testing.T) {
	server := newTestSlackServer()
	defer server.Close()
	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	c := defaultSlackAgentClient{}
	err := c.SetTitle(context.Background(), "xoxb-test", "C123", "123.456", "Aileron")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDefaultSlackAgentClient_StartStream(t *testing.T) {
	server := newTestSlackServer()
	defer server.Close()
	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	c := defaultSlackAgentClient{}
	ts, err := c.StartStream(context.Background(), "xoxb-test", "C123", "123.456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ts != "999.001" {
		t.Errorf("expected ts 999.001, got %q", ts)
	}
}

func TestDefaultSlackAgentClient_AppendStream(t *testing.T) {
	server := newTestSlackServer()
	defer server.Close()
	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	c := defaultSlackAgentClient{}
	err := c.AppendStream(context.Background(), "xoxb-test", "C123", "999.001", "chunk")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDefaultSlackAgentClient_StopStream(t *testing.T) {
	server := newTestSlackServer()
	defer server.Close()
	comms.SetAgentAPIURL(server.URL + "/")
	defer comms.SetAgentAPIURL("")

	c := defaultSlackAgentClient{}
	err := c.StopStream(context.Background(), "xoxb-test", "C123", "999.001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
