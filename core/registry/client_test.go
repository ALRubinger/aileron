package registry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/registry"
)

func fixtureServers() []registry.RegistryServer {
	return []registry.RegistryServer{
		{
			Name:        "io.github.example/filesystem",
			Description: "Access local filesystem",
			VersionDetail: registry.VersionDetail{
				Version: "1.0.0",
				Packages: []registry.Package{
					{
						RegistryType: "npm",
						Name:         "@example/mcp-filesystem",
						Runtime:      registry.RuntimeConfig{Type: "node", Command: "npx"},
						EnvVars: []registry.EnvVar{
							{Name: "FS_ROOT", Description: "Root directory", Required: true},
						},
					},
				},
			},
		},
		{
			Name:        "io.github.example/github",
			Description: "GitHub API integration",
			VersionDetail: registry.VersionDetail{
				Version: "2.1.0",
				Packages: []registry.Package{
					{
						RegistryType: "npm",
						Name:         "@example/mcp-github",
						Runtime:      registry.RuntimeConfig{Type: "node", Command: "npx"},
						EnvVars: []registry.EnvVar{
							{Name: "GITHUB_TOKEN", Description: "GitHub PAT", Required: true},
						},
					},
				},
			},
		},
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/servers" {
			http.NotFound(w, r)
			return
		}
		resp := registry.RegistryResponse{Servers: fixtureServers()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestClient_FetchAll(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := registry.NewClient(ts.Client()).WithBaseURL(ts.URL)
	servers, err := client.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(servers))
	}
	if servers[0].Name != "io.github.example/filesystem" {
		t.Errorf("first server name = %q", servers[0].Name)
	}
}

func TestClient_Search(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := registry.NewClient(ts.Client()).WithBaseURL(ts.URL)
	ctx := context.Background()

	// Search by description.
	results, err := client.Search(ctx, "GitHub API")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Name != "io.github.example/github" {
		t.Errorf("result name = %q", results[0].Name)
	}

	// Search by description keyword.
	results, err = client.Search(ctx, "local filesystem")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	// Empty query returns all.
	results, err = client.Search(ctx, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
}

func TestClient_Get(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := registry.NewClient(ts.Client()).WithBaseURL(ts.URL)
	ctx := context.Background()

	srv, err := client.Get(ctx, "io.github.example/filesystem")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
	if srv.Version != "1.0.0" {
		t.Errorf("version = %q, want 1.0.0", srv.Version)
	}

	// Not found.
	srv, err = client.Get(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Get nonexistent: %v", err)
	}
	if srv != nil {
		t.Error("expected nil for nonexistent server")
	}
}

func TestClient_Caching(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := registry.RegistryResponse{Servers: fixtureServers()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := registry.NewClient(ts.Client()).WithBaseURL(ts.URL)
	ctx := context.Background()

	// First call hits the server.
	_, _ = client.FetchAll(ctx)
	if callCount != 1 {
		t.Fatalf("call count after first fetch = %d, want 1", callCount)
	}

	// Second call should be cached.
	_, _ = client.FetchAll(ctx)
	if callCount != 1 {
		t.Fatalf("call count after second fetch = %d, want 1 (cached)", callCount)
	}
}

func TestClient_ErrorHandling(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := registry.NewClient(&http.Client{Timeout: 2 * time.Second}).WithBaseURL(ts.URL)
	_, err := client.FetchAll(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}
