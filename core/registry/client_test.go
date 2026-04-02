package registry_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/registry"
)

func fixtureEntries() []registry.RegistryEntry {
	return []registry.RegistryEntry{
		{Server: registry.RegistryServer{
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
		}},
		{Server: registry.RegistryServer{
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
		}},
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/servers" {
			http.NotFound(w, r)
			return
		}
		resp := registry.RegistryResponse{Servers: fixtureEntries()}
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
		resp := registry.RegistryResponse{Servers: fixtureEntries()}
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

// TestClient_RealRegistryResponse verifies that our types can decode an actual
// response from the MCP Registry API. This prevents regressions like the
// server-envelope and headers-as-array issues that caused blank marketplace cards.
func TestClient_RealRegistryResponse(t *testing.T) {
	payload, err := os.ReadFile("testdata/registry_response.json")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	}))
	defer ts.Close()

	client := registry.NewClient(ts.Client()).WithBaseURL(ts.URL)
	servers, err := client.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll failed to decode real registry response: %v", err)
	}
	if len(servers) == 0 {
		t.Fatal("expected at least one server from real registry response")
	}

	// Every server must have a non-empty name — this was blank before the
	// RegistryEntry envelope fix.
	for i, srv := range servers {
		if srv.Name == "" {
			t.Errorf("server[%d] has empty name; response envelope likely not unwrapped", i)
		}
	}
}

func TestClient_Pagination(t *testing.T) {
	// Build 3 pages of servers: page1 (2 servers) -> page2 (2 servers) -> page3 (1 server, no cursor).
	pages := map[string]struct {
		entries []registry.RegistryEntry
		cursor  string
	}{
		"": {
			entries: []registry.RegistryEntry{
				{Server: registry.RegistryServer{Name: "server-1", Description: "First"}},
				{Server: registry.RegistryServer{Name: "server-2", Description: "Second"}},
			},
			cursor: "page2",
		},
		"page2": {
			entries: []registry.RegistryEntry{
				{Server: registry.RegistryServer{Name: "server-3", Description: "Third"}},
				{Server: registry.RegistryServer{Name: "server-4", Description: "Fourth"}},
			},
			cursor: "page3",
		},
		"page3": {
			entries: []registry.RegistryEntry{
				{Server: registry.RegistryServer{Name: "server-5", Description: "Fifth"}},
			},
			cursor: "",
		},
	}

	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		cursor := r.URL.Query().Get("cursor")
		page, ok := pages[cursor]
		if !ok {
			http.Error(w, "bad cursor", http.StatusBadRequest)
			return
		}
		resp := registry.RegistryResponse{
			Servers: page.entries,
		}
		if page.cursor != "" {
			resp.Metadata = &registry.RegistryMetadata{
				NextCursor: page.cursor,
				Count:      len(page.entries),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := registry.NewClient(ts.Client()).WithBaseURL(ts.URL)
	servers, err := client.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll with pagination: %v", err)
	}
	if len(servers) != 5 {
		t.Fatalf("got %d servers, want 5", len(servers))
	}
	if requestCount != 3 {
		t.Fatalf("made %d requests, want 3 (one per page)", requestCount)
	}
	for i, srv := range servers {
		want := fmt.Sprintf("server-%d", i+1)
		if srv.Name != want {
			t.Errorf("server[%d].Name = %q, want %q", i, srv.Name, want)
		}
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
