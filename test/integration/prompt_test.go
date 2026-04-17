//go:build integration

package integration

import (
	"net/http"
	"testing"
)

func TestServer_StartsWithAILERONMD(t *testing.T) {
	// The server should be running — which means AILERON.md was found and
	// loaded successfully. If it were missing, the server would have panicked
	// at startup and this test wouldn't reach the health check.
	resp, err := http.Get(apiURL() + "/v1/health")
	if err != nil {
		t.Fatalf("server not running — AILERON.md may be missing from the Docker image: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from health check, got %d", resp.StatusCode)
	}
}

func TestServer_DraftEndpointAvailable(t *testing.T) {
	// The draft pipeline should be enabled (LLM configured) — which means
	// the system prompt was loaded from AILERON.md. Verify by checking that
	// the drafts endpoint works (returns 200, not 501).
	resp := authedGet(t, apiURL()+"/v1/drafts")
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		t.Fatal("drafts endpoint returned 501 — LLM/prompt may not be configured")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from drafts endpoint, got %d", resp.StatusCode)
	}
}
