// Package github implements the git connector for GitHub.
//
// This connector executes git.pull_request.* and git.commit.* actions via
// the GitHub REST API. Credentials (personal access token or GitHub App
// installation token) are injected at execution time from the vault.
package github

import (
	"context"
	"errors"

	"github.com/ALRubinger/aileron/core/connector"
)

const (
	connectorType     = "git"
	connectorProvider = "github"
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

func (c *Connector) createPullRequest(_ context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	// TODO: implement via GitHub REST API POST /repos/{owner}/{repo}/pulls.
	// Steps:
	//   1. Parse owner/repo from req.Parameters["repository"].
	//   2. POST pull request with head/base branch, title, body, labels.
	//   3. Return the PR URL as ReceiptRef.
	_ = req
	return connector.ExecutionResult{}, errors.New("github: createPullRequest not yet implemented")
}

func (c *Connector) mergePullRequest(_ context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	// TODO: implement via GitHub REST API PUT /repos/{owner}/{repo}/pulls/{pull_number}/merge.
	_ = req
	return connector.ExecutionResult{}, errors.New("github: mergePullRequest not yet implemented")
}

func (c *Connector) closePullRequest(_ context.Context, req connector.ExecutionRequest) (connector.ExecutionResult, error) {
	// TODO: implement via GitHub REST API PATCH /repos/{owner}/{repo}/pulls/{pull_number}.
	_ = req
	return connector.ExecutionResult{}, errors.New("github: closePullRequest not yet implemented")
}
