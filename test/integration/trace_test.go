//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestListTraces_AfterIntentCreation(t *testing.T) {
	// Create an intent to generate trace events.
	intentBody := map[string]any{
		"workspace_id":   "default",
		"agent_id":       "trace_test_agent",
		"idempotency_key": "trace-test-1",
		"action": map[string]any{
			"type":    "git.pull_request.create",
			"summary": "Trace test PR",
			"domain": map[string]any{
				"git": map[string]any{
					"repository":  "test/repo",
					"branch":      "feat/trace",
					"base_branch": "develop",
				},
			},
		},
	}

	intentResp := postJSON(t, apiURL()+"/v1/intents", intentBody)
	defer intentResp.Body.Close()

	if intentResp.StatusCode != http.StatusCreated {
		t.Fatalf("CreateIntent: expected 201, got %d", intentResp.StatusCode)
	}

	// List traces.
	traceResp, err := http.Get(apiURL() + "/v1/traces?workspace_id=default")
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	defer traceResp.Body.Close()

	if traceResp.StatusCode != http.StatusOK {
		t.Fatalf("ListTraces: expected 200, got %d", traceResp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(traceResp.Body).Decode(&result)

	items, ok := result["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatal("expected at least one trace")
	}

	// Find a trace with events.
	found := false
	for _, item := range items {
		trace := item.(map[string]any)
		events, _ := trace["events"].([]any)
		if len(events) >= 2 {
			found = true
			// Verify event types.
			eventTypes := map[string]bool{}
			for _, e := range events {
				evt := e.(map[string]any)
				eventTypes[evt["event_type"].(string)] = true
			}
			if !eventTypes["intent.submitted"] {
				t.Error("missing intent.submitted event")
			}
			if !eventTypes["policy.evaluated"] {
				t.Error("missing policy.evaluated event")
			}
			break
		}
	}
	if !found {
		t.Error("expected at least one trace with 2+ events")
	}
}
