// Package github implements the git connector for GitHub.
//
// This connector executes git.pull_request.* and git.commit.* actions via
// the GitHub REST API. Credentials (personal access token or GitHub App
// installation token) are injected at execution time from the vault.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ALRubinger/aileron/core/connector"
)

const (
	connectorType     = "git"
	connectorProvider = "github"
	githubAPI         = "https://api.github.com"
)

// Connector executes git actions via GitHub.
type Connector struct{}

// New returns a new GitHub connector.
func New() *Connector {
	return &Connector{}
}

// Type implements connector.Connector.
func (c *Connector) Type() string { return connectorType }

// Provider implements connector.Connector.
func (c *Connector) Provider() string { return connectorProvider }

// Execute implements connector.Connector.
func (c *Connector) Execute(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	if req.Credential == nil {
		return connector.ExecutionResult{}, errors.New("github: no credential injected")
	}

	switch req.ActionType {
	case "git.pull_request.create":
		return c.createPullRequest(ctx, req)
	case "git.pull_request.merge":
		return c.mergePullRequest(ctx, req)
	case "git.pull_request.close":
		return c.closePullRequest(ctx, req)
	default:
		return connector.ExecutionResult{
			Status: connector.ExecutionStatusFailed,
			Error:  "github: unsupported action type: " + req.ActionType,
		}, nil
	}
}

func (c *Connector) createPullRequest(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	repo := paramStr(req.Parameters, "repository")
	if repo == "" {
		return failResult("github: repository parameter required"), nil
	}
	head := paramStr(req.Parameters, "branch")
	base := paramStr(req.Parameters, "base_branch")
	title := paramStr(req.Parameters, "pr_title")
	body := paramStr(req.Parameters, "pr_body")

	if head == "" || base == "" || title == "" {
		return failResult("github: branch, base_branch, and pr_title parameters required"), nil
	}

	payload := map[string]interface{}{
		"title": title,
		"head":  head,
		"base":  base,
	}
	if body != "" {
		payload["body"] = body
	}

	url := fmt.Sprintf("%s/repos/%s/pulls", githubAPI, repo)
	result, err := githubPost(ctx, url, string(req.Credential.Value), payload)
	if err != nil {
		return failResult("github: " + err.Error()), nil
	}

	prURL, _ := result["html_url"].(string)
	prNumber, _ := result["number"].(float64)

	return connector.ExecutionResult{
		Status:     connector.ExecutionStatusSucceeded,
		ReceiptRef: prURL,
		Output: map[string]any{
			"pr_url":    prURL,
			"pr_number": int(prNumber),
		},
	}, nil
}

func (c *Connector) mergePullRequest(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	repo := paramStr(req.Parameters, "repository")
	prNumber := paramStr(req.Parameters, "pr_number")
	if repo == "" || prNumber == "" {
		return failResult("github: repository and pr_number parameters required"), nil
	}

	url := fmt.Sprintf("%s/repos/%s/pulls/%s/merge", githubAPI, repo, prNumber)
	_, err := githubPut(ctx, url, string(req.Credential.Value), map[string]interface{}{})
	if err != nil {
		return failResult("github: " + err.Error()), nil
	}

	return connector.ExecutionResult{
		Status: connector.ExecutionStatusSucceeded,
		Output: map[string]any{"merged": true},
	}, nil
}

func (c *Connector) closePullRequest(ctx context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	repo := paramStr(req.Parameters, "repository")
	prNumber := paramStr(req.Parameters, "pr_number")
	if repo == "" || prNumber == "" {
		return failResult("github: repository and pr_number parameters required"), nil
	}

	url := fmt.Sprintf("%s/repos/%s/pulls/%s", githubAPI, repo, prNumber)
	_, err := githubPatch(ctx, url, string(req.Credential.Value), map[string]interface{}{
		"state": "closed",
	})
	if err != nil {
		return failResult("github: " + err.Error()), nil
	}

	return connector.ExecutionResult{
		Status: connector.ExecutionStatusSucceeded,
		Output: map[string]any{"closed": true},
	}, nil
}

// --- HTTP helpers ---

func githubPost(ctx context.Context, url, token string, payload map[string]interface{}) (map[string]interface{}, error) {
	return githubRequest(ctx, http.MethodPost, url, token, payload)
}

func githubPut(ctx context.Context, url, token string, payload map[string]interface{}) (map[string]interface{}, error) {
	return githubRequest(ctx, http.MethodPut, url, token, payload)
}

func githubPatch(ctx context.Context, url, token string, payload map[string]interface{}) (map[string]interface{}, error) {
	return githubRequest(ctx, http.MethodPatch, url, token, payload)
}

func githubRequest(ctx context.Context, method, url, token string, payload map[string]interface{}) (map[string]interface{}, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	token = strings.TrimSpace(token)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]interface{}
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return result, nil
}

func paramStr(params map[string]any, key string) string {
	v, ok := params[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

func failResult(msg string) connector.ExecutionResult {
	return connector.ExecutionResult{
		Status: connector.ExecutionStatusFailed,
		Error:  msg,
	}
}
