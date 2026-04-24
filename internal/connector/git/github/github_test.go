package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALRubinger/aileron/internal/connector"
)

func TestConnector_TypeAndProvider(t *testing.T) {
	c := New()
	if c.Type() != "git" {
		t.Errorf("Type() = %q, want %q", c.Type(), "git")
	}
	if c.Provider() != "github" {
		t.Errorf("Provider() = %q, want %q", c.Provider(), "github")
	}
}

func TestConnector_NoCredential(t *testing.T) {
	c := New()
	_, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.issue.create",
	})
	if err == nil {
		t.Fatal("expected error for nil credential")
	}
}

func TestConnector_UnsupportedAction(t *testing.T) {
	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.unknown",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want %q", result.Status, connector.ExecutionStatusFailed)
	}
}

func TestCreatePullRequest_Success(t *testing.T) {
	var receivedPayload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/app/pulls" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"html_url": "https://github.com/acme/app/pull/7",
			"number":   7.0,
		})
	}))
	defer server.Close()

	origAPI := githubAPI
	githubAPI = server.URL
	defer func() { githubAPI = origAPI }()

	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.pull_request.create",
		Credential: &connector.InjectedCredential{Value: []byte("test-token")},
		Parameters: map[string]any{
			"repository":  "acme/app",
			"branch":      "feat/new",
			"base_branch": "main",
			"pr_title":    "Add feature",
			"pr_body":     "Description here",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q, want succeeded", result.Status)
	}
	if result.ReceiptRef != "https://github.com/acme/app/pull/7" {
		t.Errorf("ReceiptRef = %q", result.ReceiptRef)
	}
	if result.Output["pr_number"] != 7 {
		t.Errorf("pr_number = %v", result.Output["pr_number"])
	}
	if receivedPayload["title"] != "Add feature" {
		t.Errorf("title = %v", receivedPayload["title"])
	}
	if receivedPayload["head"] != "feat/new" {
		t.Errorf("head = %v", receivedPayload["head"])
	}
	if receivedPayload["base"] != "main" {
		t.Errorf("base = %v", receivedPayload["base"])
	}
	if receivedPayload["body"] != "Description here" {
		t.Errorf("body = %v", receivedPayload["body"])
	}
}

func TestCreatePullRequest_NoBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		json.NewDecoder(r.Body).Decode(&payload)
		if _, ok := payload["body"]; ok {
			t.Error("body should not be present when empty")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"html_url": "https://github.com/acme/app/pull/8",
			"number":   8.0,
		})
	}))
	defer server.Close()

	origAPI := githubAPI
	githubAPI = server.URL
	defer func() { githubAPI = origAPI }()

	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.pull_request.create",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{
			"repository":  "acme/app",
			"branch":      "feat/x",
			"base_branch": "main",
			"pr_title":    "Title only",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q", result.Status)
	}
}

func TestCreatePullRequest_MissingParams(t *testing.T) {
	c := New()
	tests := []struct {
		name   string
		params map[string]any
	}{
		{"missing repository", map[string]any{"branch": "feat", "base_branch": "main", "pr_title": "T"}},
		{"missing branch", map[string]any{"repository": "acme/app", "base_branch": "main", "pr_title": "T"}},
		{"missing base_branch", map[string]any{"repository": "acme/app", "branch": "feat", "pr_title": "T"}},
		{"missing pr_title", map[string]any{"repository": "acme/app", "branch": "feat", "base_branch": "main"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := c.Execute(context.Background(), connector.ExecutionRequest{
				ActionType: "git.pull_request.create",
				Credential: &connector.InjectedCredential{Value: []byte("token")},
				Parameters: tt.params,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Status != connector.ExecutionStatusFailed {
				t.Errorf("Status = %q, want failed for %s", result.Status, tt.name)
			}
		})
	}
}

func TestCreatePullRequest_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	}))
	defer server.Close()

	origAPI := githubAPI
	githubAPI = server.URL
	defer func() { githubAPI = origAPI }()

	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.pull_request.create",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{
			"repository":  "acme/app",
			"branch":      "feat/x",
			"base_branch": "main",
			"pr_title":    "Fail",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestMergePullRequest_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/repos/acme/app/pulls/7/merge" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"merged": true})
	}))
	defer server.Close()

	origAPI := githubAPI
	githubAPI = server.URL
	defer func() { githubAPI = origAPI }()

	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.pull_request.merge",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{
			"repository": "acme/app",
			"pr_number":  "7",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q, want succeeded", result.Status)
	}
	if result.Output["merged"] != true {
		t.Errorf("merged = %v", result.Output["merged"])
	}
}

func TestMergePullRequest_MissingParams(t *testing.T) {
	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.pull_request.merge",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{"repository": "acme/app"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestMergePullRequest_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte(`{"message":"Not allowed"}`))
	}))
	defer server.Close()

	origAPI := githubAPI
	githubAPI = server.URL
	defer func() { githubAPI = origAPI }()

	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.pull_request.merge",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{"repository": "acme/app", "pr_number": "7"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestClosePullRequest_Success(t *testing.T) {
	var receivedPayload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/acme/app/pulls/7" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"state": "closed"})
	}))
	defer server.Close()

	origAPI := githubAPI
	githubAPI = server.URL
	defer func() { githubAPI = origAPI }()

	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.pull_request.close",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{
			"repository": "acme/app",
			"pr_number":  "7",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q, want succeeded", result.Status)
	}
	if result.Output["closed"] != true {
		t.Errorf("closed = %v", result.Output["closed"])
	}
	if receivedPayload["state"] != "closed" {
		t.Errorf("state = %v, want closed", receivedPayload["state"])
	}
}

func TestClosePullRequest_MissingParams(t *testing.T) {
	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.pull_request.close",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{"repository": "acme/app"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestClosePullRequest_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	origAPI := githubAPI
	githubAPI = server.URL
	defer func() { githubAPI = origAPI }()

	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.pull_request.close",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{"repository": "acme/app", "pr_number": "999"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestCreateIssue_Success(t *testing.T) {
	var receivedPayload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/repos/acme/app/issues" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&receivedPayload)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"html_url": "https://github.com/acme/app/issues/42",
			"number":   42.0,
		})
	}))
	defer server.Close()

	// Override the API URL for testing.
	origAPI := githubAPI
	githubAPI = server.URL
	defer func() { githubAPI = origAPI }()

	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.issue.create",
		Credential: &connector.InjectedCredential{Value: []byte("test-token")},
		Parameters: map[string]any{
			"repository":      "acme/app",
			"issue_title":     "Bug: login fails",
			"issue_body":      "Steps to reproduce...",
			"issue_labels":    []string{"bug", "priority-high"},
			"issue_assignees": []string{"alice"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q, want %q", result.Status, connector.ExecutionStatusSucceeded)
	}
	if result.ReceiptRef != "https://github.com/acme/app/issues/42" {
		t.Errorf("ReceiptRef = %q", result.ReceiptRef)
	}
	if result.Output["issue_number"] != 42 {
		t.Errorf("issue_number = %v", result.Output["issue_number"])
	}

	// Verify payload.
	if receivedPayload["title"] != "Bug: login fails" {
		t.Errorf("title = %v", receivedPayload["title"])
	}
	if receivedPayload["body"] != "Steps to reproduce..." {
		t.Errorf("body = %v", receivedPayload["body"])
	}
}

func TestCreateIssue_MissingParams(t *testing.T) {
	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.issue.create",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{
			"repository": "acme/app",
			// missing issue_title
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestCommentOnIssue_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/app/issues/42/comments" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"html_url": "https://github.com/acme/app/issues/42#issuecomment-1",
			"id":       1.0,
		})
	}))
	defer server.Close()

	origAPI := githubAPI
	githubAPI = server.URL
	defer func() { githubAPI = origAPI }()

	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.issue.comment",
		Credential: &connector.InjectedCredential{Value: []byte("test-token")},
		Parameters: map[string]any{
			"repository":   "acme/app",
			"issue_number": "42",
			"body":         "Fixed in PR #43",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusSucceeded {
		t.Errorf("Status = %q, want succeeded", result.Status)
	}
	if result.Output["comment_id"] != 1 {
		t.Errorf("comment_id = %v", result.Output["comment_id"])
	}
}

func TestCommentOnIssue_MissingParams(t *testing.T) {
	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.issue.comment",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{
			"repository": "acme/app",
			// missing issue_number and body
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}

func TestCreateIssue_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	}))
	defer server.Close()

	origAPI := githubAPI
	githubAPI = server.URL
	defer func() { githubAPI = origAPI }()

	c := New()
	result, err := c.Execute(context.Background(), connector.ExecutionRequest{
		ActionType: "git.issue.create",
		Credential: &connector.InjectedCredential{Value: []byte("token")},
		Parameters: map[string]any{
			"repository":  "acme/app",
			"issue_title": "Test",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != connector.ExecutionStatusFailed {
		t.Errorf("Status = %q, want failed", result.Status)
	}
}
