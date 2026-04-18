package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	githubsource "github.com/ALRubinger/aileron/core/source/github"
)

func testToken() []byte {
	b, _ := json.Marshal(map[string]string{
		"access_token": "ghp_test_token",
		"token_type":   "bearer",
	})
	return b
}

func newMockGitHubServer() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/search/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"items": []map[string]any{
				{
					"name": "jwt.go",
					"path": "auth/jwt.go",
					"repository": map[string]any{
						"full_name": "ALRubinger/aileron",
					},
				},
			},
		})
	})

	mux.HandleFunc("/api/v3/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"items": []map[string]any{
				{
					"number":   247,
					"title":    "Refactor JWT validation into middleware",
					"state":    "closed",
					"html_url": "https://github.com/ALRubinger/aileron/pull/247",
					"user":     map[string]any{"login": "alr"},
					"pull_request": map[string]any{
						"url": "https://api.github.com/repos/ALRubinger/aileron/pulls/247",
					},
				},
			},
		})
	})

	mux.HandleFunc("/api/v3/repos/ALRubinger/aileron/pulls/247", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/vnd.github.diff" {
			w.Write([]byte("diff --git a/auth/jwt.go b/auth/jwt.go\n--- a/auth/jwt.go\n+++ b/auth/jwt.go\n@@ -1 +1 @@\n-old\n+new\n"))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"number":    247,
			"title":     "Refactor JWT validation into middleware",
			"body":      "Moves validation from handler into middleware",
			"state":     "closed",
			"merged":    true,
			"additions": 50,
			"deletions": 30,
			"user":      map[string]any{"login": "alr"},
		})
	})

	mux.HandleFunc("/api/v3/repos/ALRubinger/aileron/pulls/247/files", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{
				"filename":  "auth/jwt.go",
				"status":    "modified",
				"additions": 30,
				"deletions": 20,
			},
			{
				"filename":  "auth/middleware.go",
				"status":    "added",
				"additions": 20,
				"deletions": 0,
			},
		})
	})

	mux.HandleFunc("/api/v3/repos/ALRubinger/aileron/contents/auth/jwt.go", func(w http.ResponseWriter, r *http.Request) {
		// GitHub returns base64-encoded content.
		json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"path":     "auth/jwt.go",
			"size":     100,
			"encoding": "base64",
			"content":  "cGFja2FnZSBhdXRoCg==", // "package auth\n"
		})
	})

	return httptest.NewServer(mux)
}

func TestConnector_Provider(t *testing.T) {
	c := githubsource.New()
	if c.Provider() != "github_repos" {
		t.Errorf("expected github_repos, got %s", c.Provider())
	}
}

func TestConnector_Tools(t *testing.T) {
	c := githubsource.New()
	tools := c.Tools()
	if len(tools) != 5 {
		t.Fatalf("expected 5 tools, got %d", len(tools))
	}
	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}
	for _, expected := range []string{"github_search_code", "github_search_issues", "github_get_pr", "github_get_pr_diff", "github_get_file"} {
		if !names[expected] {
			t.Errorf("expected tool %s", expected)
		}
	}
}

func TestConnector_SearchCode(t *testing.T) {
	server := newMockGitHubServer()
	defer server.Close()

	c := githubsource.New().WithBaseURL(server.URL + "/api/v3/")
	result, err := c.Execute(context.Background(), "github_search_code", map[string]any{
		"query": "JWT claims",
		"repo":  "ALRubinger/aileron",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	results := result["results"].([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0]["path"] != "auth/jwt.go" {
		t.Errorf("expected auth/jwt.go, got %v", results[0]["path"])
	}
}

func TestConnector_SearchIssues(t *testing.T) {
	server := newMockGitHubServer()
	defer server.Close()

	c := githubsource.New().WithBaseURL(server.URL + "/api/v3/")
	result, err := c.Execute(context.Background(), "github_search_issues", map[string]any{
		"query": "JWT refactor",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	results := result["results"].([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0]["number"] != 247 {
		t.Errorf("expected PR 247, got %v", results[0]["number"])
	}
	if results[0]["is_pr"] != true {
		t.Error("expected is_pr=true")
	}
}

func TestConnector_SearchIssues_WithDateFilters(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"items":       []map[string]any{},
		})
	}))
	defer server.Close()

	c := githubsource.New().WithBaseURL(server.URL + "/")
	_, err := c.Execute(context.Background(), "github_search_issues", map[string]any{
		"query":  "auth refactor",
		"after":  "2026-04-14",
		"before": "2026-04-16",
	}, testToken())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(gotQuery, "updated:>=2026-04-14") {
		t.Errorf("expected updated:>= in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "updated:<=2026-04-16") {
		t.Errorf("expected updated:<= in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "auth refactor") {
		t.Errorf("expected original query preserved, got %q", gotQuery)
	}
}

func TestConnector_GetPR(t *testing.T) {
	server := newMockGitHubServer()
	defer server.Close()

	c := githubsource.New().WithBaseURL(server.URL + "/api/v3/")
	result, err := c.Execute(context.Background(), "github_get_pr", map[string]any{
		"repo":   "ALRubinger/aileron",
		"number": 247,
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["title"] != "Refactor JWT validation into middleware" {
		t.Errorf("unexpected title: %v", result["title"])
	}
	if result["merged"] != true {
		t.Error("expected merged=true")
	}
	files := result["changed_files"].([]map[string]any)
	if len(files) != 2 {
		t.Fatalf("expected 2 changed files, got %d", len(files))
	}
}

func TestConnector_GetPRDiff(t *testing.T) {
	server := newMockGitHubServer()
	defer server.Close()

	c := githubsource.New().WithBaseURL(server.URL + "/api/v3/")
	result, err := c.Execute(context.Background(), "github_get_pr_diff", map[string]any{
		"repo":   "ALRubinger/aileron",
		"number": 247,
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	diff := result["diff"].(string)
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
}

func TestConnector_GetFile(t *testing.T) {
	server := newMockGitHubServer()
	defer server.Close()

	c := githubsource.New().WithBaseURL(server.URL + "/api/v3/")
	result, err := c.Execute(context.Background(), "github_get_file", map[string]any{
		"repo": "ALRubinger/aileron",
		"path": "auth/jwt.go",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content := result["content"].(string)
	if content == "" {
		t.Fatal("expected non-empty content")
	}
}

func TestConnector_UnknownTool(t *testing.T) {
	c := githubsource.New()
	_, err := c.Execute(context.Background(), "github_nonexistent", nil, testToken())
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestConnector_InvalidToken(t *testing.T) {
	c := githubsource.New()
	_, err := c.Execute(context.Background(), "github_search_code", map[string]any{
		"query": "test",
	}, []byte("not-json"))
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestConnector_MissingQuery(t *testing.T) {
	c := githubsource.New()
	_, err := c.Execute(context.Background(), "github_search_code", map[string]any{}, testToken())
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestConnector_MissingRepo(t *testing.T) {
	c := githubsource.New()
	_, err := c.Execute(context.Background(), "github_get_pr", map[string]any{
		"number": 1,
	}, testToken())
	if err == nil {
		t.Fatal("expected error for missing repo")
	}
}

func TestConnector_BadRepoFormat(t *testing.T) {
	c := githubsource.New()
	_, err := c.Execute(context.Background(), "github_get_pr", map[string]any{
		"repo":   "no-slash",
		"number": 1,
	}, testToken())
	if err == nil {
		t.Fatal("expected error for bad repo format")
	}
}

func TestConnector_MissingNumber(t *testing.T) {
	c := githubsource.New()
	_, err := c.Execute(context.Background(), "github_get_pr", map[string]any{
		"repo": "owner/name",
	}, testToken())
	if err == nil {
		t.Fatal("expected error for missing number")
	}
}

func TestConnector_SearchIssues_MissingQuery(t *testing.T) {
	c := githubsource.New()
	_, err := c.Execute(context.Background(), "github_search_issues", map[string]any{}, testToken())
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestConnector_GetPR_FloatNumber(t *testing.T) {
	server := newMockGitHubServer()
	defer server.Close()

	c := githubsource.New().WithBaseURL(server.URL + "/api/v3/")
	result, err := c.Execute(context.Background(), "github_get_pr", map[string]any{
		"repo":   "ALRubinger/aileron",
		"number": float64(247), // JSON numbers often come as float64
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["title"] == nil {
		t.Error("expected PR details")
	}
}

func TestConnector_GetFile_WithRef(t *testing.T) {
	server := newMockGitHubServer()
	defer server.Close()

	c := githubsource.New().WithBaseURL(server.URL + "/api/v3/")
	result, err := c.Execute(context.Background(), "github_get_file", map[string]any{
		"repo": "ALRubinger/aileron",
		"path": "auth/jwt.go",
		"ref":  "main",
	}, testToken())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["content"] == nil {
		t.Error("expected file content")
	}
}

func TestConnector_OAuthTokenFormat(t *testing.T) {
	// Token stored as golang.org/x/oauth2.Token JSON format.
	token, _ := json.Marshal(map[string]any{
		"access_token": "ghp_oauth2_format",
		"token_type":   "bearer",
		"expiry":       "2026-12-31T00:00:00Z",
	})

	server := newMockGitHubServer()
	defer server.Close()

	c := githubsource.New().WithBaseURL(server.URL + "/api/v3/")
	_, err := c.Execute(context.Background(), "github_search_code", map[string]any{
		"query": "test",
	}, token)

	if err != nil {
		t.Fatalf("unexpected error with oauth2 token format: %v", err)
	}
}

func TestConnector_EmptyAccessToken(t *testing.T) {
	token, _ := json.Marshal(map[string]string{"token_type": "bearer"})
	c := githubsource.New()
	_, err := c.Execute(context.Background(), "github_search_code", map[string]any{"query": "test"}, token)
	if err == nil {
		t.Fatal("expected error for missing access_token")
	}
}

func TestConnector_MissingPath(t *testing.T) {
	c := githubsource.New()
	_, err := c.Execute(context.Background(), "github_get_file", map[string]any{
		"repo": "owner/name",
	}, testToken())
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}
