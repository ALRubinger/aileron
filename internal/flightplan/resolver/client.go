package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Fetcher fetches the installed actions the resolver matches refs against.
// It is the daemon seam: production wires it to GET /v1/actions; tests
// inject a fake so no live daemon is required (mirroring the syncFetcher /
// bindingAPIBaseURL seam discipline elsewhere in the codebase).
type Fetcher interface {
	FetchActions(ctx context.Context) ([]Action, error)
}

// FetcherFunc adapts a function to the Fetcher interface.
type FetcherFunc func(ctx context.Context) ([]Action, error)

// FetchActions calls the underlying function.
func (f FetcherFunc) FetchActions(ctx context.Context) ([]Action, error) {
	return f(ctx)
}

// listActionsResponse mirrors the daemon's GET /v1/actions envelope. Only
// the items array is decoded, and within each item only the fields the
// resolver matches on (Action's json tags ignore the rest).
type listActionsResponse struct {
	Items []Action `json:"items"`
}

// HTTPFetcher fetches actions from the daemon over HTTP.
type HTTPFetcher struct {
	// BaseURL is the daemon API base including the /v1 suffix.
	BaseURL string
	// Client is the HTTP client; nil uses a default with a 30s timeout.
	Client *http.Client
	// Authorize, when non-nil, sets the daemon bearer token on the request.
	Authorize func(*http.Request)
}

// FetchActions retrieves the installed actions from GET {BaseURL}/actions.
func (h HTTPFetcher) FetchActions(ctx context.Context) ([]Action, error) {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/actions", nil)
	if err != nil {
		return nil, fmt.Errorf("resolver: build actions request: %w", err)
	}
	if h.Authorize != nil {
		h.Authorize(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("resolver: fetch actions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("resolver: daemon returned %d: %s", resp.StatusCode, string(body))
	}
	var out listActionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("resolver: decode actions response: %w", err)
	}
	return out.Items, nil
}

// ResolveWith fetches the installed actions via the fetcher and resolves
// the refs against them. A fetch error is returned to the caller (the
// installer decides whether a daemon-down condition warns or fails; an
// instruction-only skill never reaches this code).
func ResolveWith(ctx context.Context, f Fetcher, refs []string) (Resolution, error) {
	actions, err := f.FetchActions(ctx)
	if err != nil {
		return Resolution{}, err
	}
	return Resolve(refs, actions), nil
}
