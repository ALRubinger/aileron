//go:build integration

package integration

import (
	"net/http"
	"os"
	"testing"
)

func apiURL() string {
	if u := os.Getenv("AILERON_API_URL"); u != "" {
		return u
	}
	return "http://localhost:8080"
}

func TestHealthEndpoint(t *testing.T) {
	resp, err := http.Get(apiURL() + "/v1/health")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
