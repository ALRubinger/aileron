// Package github implements a SourceConnector for GitHub, providing read-only
// tools for searching code, issues/PRs, getting PR details and diffs, and
// reading file contents.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/ALRubinger/aileron/core/source"
	gh "github.com/google/go-github/v69/github"
	"golang.org/x/oauth2"
)

// Connector implements source.SourceConnector for GitHub.
type Connector struct {
	baseURL string // override for testing
}

// New creates a new GitHub source connector.
func New() *Connector {
	return &Connector{}
}

// WithBaseURL returns a copy with a custom GitHub API URL (for testing).
func (c *Connector) WithBaseURL(url string) *Connector {
	cp := *c
	cp.baseURL = url
	return &cp
}

func (c *Connector) Provider() string { return "github_repos" }

func (c *Connector) Tools() []source.ToolDefinition {
	return []source.ToolDefinition{
		{
			Name:        "github_search_code",
			Description: "Search code across GitHub repositories the user has access to. Returns matching file paths and code snippets.",
			Parameters: []source.ToolParam{
				{Name: "query", Type: "string", Description: "Search query (GitHub code search syntax)", Required: true},
				{Name: "repo", Type: "string", Description: "Limit search to a specific repo (owner/name)", Required: false},
			},
		},
		{
			Name:        "github_search_issues",
			Description: "Search issues and pull requests. Returns titles, numbers, states, and authors.",
			Parameters: []source.ToolParam{
				{Name: "query", Type: "string", Description: "Search query (GitHub issue search syntax)", Required: true},
				{Name: "repo", Type: "string", Description: "Limit search to a specific repo (owner/name)", Required: false},
				{Name: "after", Type: "string", Description: "Only return items created/updated after this date (YYYY-MM-DD)", Required: false},
				{Name: "before", Type: "string", Description: "Only return items created/updated before this date (YYYY-MM-DD)", Required: false},
			},
		},
		{
			Name:        "github_get_pr",
			Description: "Get details of a pull request including title, description, status, author, and changed files.",
			Parameters: []source.ToolParam{
				{Name: "repo", Type: "string", Description: "Repository (owner/name)", Required: true},
				{Name: "number", Type: "integer", Description: "PR number", Required: true},
			},
		},
		{
			Name:        "github_get_pr_diff",
			Description: "Get the diff of a pull request showing exactly what code changed.",
			Parameters: []source.ToolParam{
				{Name: "repo", Type: "string", Description: "Repository (owner/name)", Required: true},
				{Name: "number", Type: "integer", Description: "PR number", Required: true},
			},
		},
		{
			Name:        "github_get_file",
			Description: "Get the contents of a file from a repository.",
			Parameters: []source.ToolParam{
				{Name: "repo", Type: "string", Description: "Repository (owner/name)", Required: true},
				{Name: "path", Type: "string", Description: "File path within the repo", Required: true},
				{Name: "ref", Type: "string", Description: "Branch, tag, or commit SHA (default: HEAD)", Required: false},
			},
		},
	}
}

func (c *Connector) Execute(ctx context.Context, tool string, params map[string]any, token []byte) (map[string]any, error) {
	accessToken, err := extractGitHubToken(token)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	client := c.newClient(ctx, accessToken)

	switch tool {
	case "github_search_code":
		return c.searchCode(ctx, client, params)
	case "github_search_issues":
		return c.searchIssues(ctx, client, params)
	case "github_get_pr":
		return c.getPR(ctx, client, params)
	case "github_get_pr_diff":
		return c.getPRDiff(ctx, client, params)
	case "github_get_file":
		return c.getFile(ctx, client, params)
	default:
		return nil, fmt.Errorf("unknown tool: %s", tool)
	}
}

func (c *Connector) newClient(ctx context.Context, token string) *gh.Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	client := gh.NewClient(tc)
	if c.baseURL != "" {
		client, _ = client.WithEnterpriseURLs(c.baseURL, c.baseURL)
	}
	return client
}

func (c *Connector) searchCode(ctx context.Context, client *gh.Client, params map[string]any) (map[string]any, error) {
	query, ok := params["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("query parameter is required")
	}
	if repo, ok := params["repo"].(string); ok && repo != "" {
		query = query + " repo:" + repo
	}

	result, _, err := client.Search.Code(ctx, query, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 10},
	})
	if err != nil {
		return nil, fmt.Errorf("github API error: %w", err)
	}

	items := make([]map[string]any, 0, len(result.CodeResults))
	for _, r := range result.CodeResults {
		items = append(items, map[string]any{
			"repo": r.GetRepository().GetFullName(),
			"path": r.GetPath(),
			"name": r.GetName(),
		})
	}

	return map[string]any{"results": items, "total": result.GetTotal()}, nil
}

func (c *Connector) searchIssues(ctx context.Context, client *gh.Client, params map[string]any) (map[string]any, error) {
	query, ok := params["query"].(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("query parameter is required")
	}
	if repo, ok := params["repo"].(string); ok && repo != "" {
		query = query + " repo:" + repo
	}
	// Inject date range into GitHub search syntax.
	if after, ok := params["after"].(string); ok && after != "" {
		query += " updated:>=" + after
	}
	if before, ok := params["before"].(string); ok && before != "" {
		query += " updated:<=" + before
	}

	result, _, err := client.Search.Issues(ctx, query, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 10},
	})
	if err != nil {
		return nil, fmt.Errorf("github API error: %w", err)
	}

	items := make([]map[string]any, 0, len(result.Issues))
	for _, issue := range result.Issues {
		item := map[string]any{
			"number": issue.GetNumber(),
			"title":  issue.GetTitle(),
			"state":  issue.GetState(),
			"author": issue.GetUser().GetLogin(),
			"url":    issue.GetHTMLURL(),
		}
		if issue.PullRequestLinks != nil {
			item["is_pr"] = true
		}
		items = append(items, item)
	}

	return map[string]any{"results": items, "total": result.GetTotal()}, nil
}

func (c *Connector) getPR(ctx context.Context, client *gh.Client, params map[string]any) (map[string]any, error) {
	owner, repo, err := parseRepo(params)
	if err != nil {
		return nil, err
	}
	number, err := parseNumber(params)
	if err != nil {
		return nil, err
	}

	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("github API error: %w", err)
	}

	files, _, err := client.PullRequests.ListFiles(ctx, owner, repo, number, &gh.ListOptions{PerPage: 50})
	if err != nil {
		return nil, fmt.Errorf("github API error listing files: %w", err)
	}

	changedFiles := make([]map[string]any, 0, len(files))
	for _, f := range files {
		changedFiles = append(changedFiles, map[string]any{
			"filename":  f.GetFilename(),
			"status":    f.GetStatus(),
			"additions": f.GetAdditions(),
			"deletions": f.GetDeletions(),
		})
	}

	return map[string]any{
		"number":        pr.GetNumber(),
		"title":         pr.GetTitle(),
		"body":          pr.GetBody(),
		"state":         pr.GetState(),
		"author":        pr.GetUser().GetLogin(),
		"merged":        pr.GetMerged(),
		"additions":     pr.GetAdditions(),
		"deletions":     pr.GetDeletions(),
		"changed_files": changedFiles,
	}, nil
}

func (c *Connector) getPRDiff(ctx context.Context, client *gh.Client, params map[string]any) (map[string]any, error) {
	owner, repo, err := parseRepo(params)
	if err != nil {
		return nil, err
	}
	number, err := parseNumber(params)
	if err != nil {
		return nil, err
	}

	diff, _, err := client.PullRequests.GetRaw(ctx, owner, repo, number, gh.RawOptions{Type: gh.Diff})
	if err != nil {
		return nil, fmt.Errorf("github API error: %w", err)
	}

	// Truncate very large diffs to avoid blowing the context window.
	if len(diff) > 10000 {
		diff = diff[:10000] + "\n... (truncated, " + strconv.Itoa(len(diff)) + " bytes total)"
	}

	return map[string]any{"diff": diff}, nil
}

func (c *Connector) getFile(ctx context.Context, client *gh.Client, params map[string]any) (map[string]any, error) {
	owner, repo, err := parseRepo(params)
	if err != nil {
		return nil, err
	}
	path, ok := params["path"].(string)
	if !ok || path == "" {
		return nil, fmt.Errorf("path parameter is required")
	}

	opts := &gh.RepositoryContentGetOptions{}
	if ref, ok := params["ref"].(string); ok && ref != "" {
		opts.Ref = ref
	}

	fileContent, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return nil, fmt.Errorf("github API error: %w", err)
	}
	if resp != nil {
		defer resp.Body.Close()
	}

	if fileContent == nil {
		return nil, fmt.Errorf("path is a directory, not a file")
	}

	content, err := fileContent.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decoding file content: %w", err)
	}

	// Truncate large files.
	if len(content) > 20000 {
		content = content[:20000] + "\n... (truncated)"
	}

	return map[string]any{
		"path":    fileContent.GetPath(),
		"size":    fileContent.GetSize(),
		"content": content,
	}, nil
}

// parseRepo extracts owner and repo name from the "repo" parameter (format: "owner/name").
func parseRepo(params map[string]any) (string, string, error) {
	repo, ok := params["repo"].(string)
	if !ok || repo == "" {
		return "", "", fmt.Errorf("repo parameter is required (format: owner/name)")
	}
	for i, c := range repo {
		if c == '/' {
			return repo[:i], repo[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("repo must be in format owner/name, got: %s", repo)
}

// parseNumber extracts an integer from the "number" parameter.
func parseNumber(params map[string]any) (int, error) {
	switch n := params["number"].(type) {
	case int:
		return n, nil
	case float64:
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		return int(i), err
	default:
		return 0, fmt.Errorf("number parameter is required")
	}
}

// extractGitHubToken parses the stored token JSON and returns the access token.
func extractGitHubToken(token []byte) (string, error) {
	// Try OAuth2 token format first (from golang.org/x/oauth2).
	var oauthToken oauth2.Token
	if err := json.Unmarshal(token, &oauthToken); err == nil && oauthToken.AccessToken != "" {
		return oauthToken.AccessToken, nil
	}
	// Try simple map format.
	var data map[string]string
	if err := json.Unmarshal(token, &data); err == nil {
		if at := data["access_token"]; at != "" {
			return at, nil
		}
	}
	return "", fmt.Errorf("no access_token in token data")
}

