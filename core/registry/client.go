package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultRegistryURL is the official MCP Registry API endpoint.
	DefaultRegistryURL = "https://registry.modelcontextprotocol.io"

	// DefaultRefreshInterval is how often the registry server list is
	// refreshed in the background. Override with REGISTRY_REFRESH_INTERVAL.
	DefaultRefreshInterval = 15 * time.Minute
)

// Client fetches and caches MCP server metadata from the official registry.
// Call Start to begin the background refresh loop and prefetch on startup.
type Client struct {
	baseURL         string
	httpClient      *http.Client
	refreshInterval time.Duration
	log             *slog.Logger

	mu     sync.RWMutex
	cached []RegistryServer
}

// NewClient creates a registry client with sensible defaults.
func NewClient(httpClient *http.Client, log *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		baseURL:         DefaultRegistryURL,
		httpClient:      httpClient,
		refreshInterval: DefaultRefreshInterval,
		log:             log,
	}
}

// WithBaseURL overrides the registry URL (useful for testing).
func (c *Client) WithBaseURL(url string) *Client {
	c.baseURL = url
	return c
}

// WithRefreshInterval overrides the background refresh interval.
func (c *Client) WithRefreshInterval(d time.Duration) *Client {
	c.refreshInterval = d
	return c
}

// Start prefetches the registry and starts a background goroutine that
// refreshes the cache at the configured interval. The goroutine stops
// when ctx is cancelled.
func (c *Client) Start(ctx context.Context) {
	go func() {
		// Prefetch asynchronously so the server can start accepting
		// requests immediately. Marketplace queries will return empty
		// results until the first refresh completes.
		if err := c.refresh(ctx); err != nil {
			c.log.Warn("registry prefetch failed; will retry on next interval", "error", err)
		} else {
			c.mu.RLock()
			count := len(c.cached)
			c.mu.RUnlock()
			c.log.Info("registry prefetch complete", "servers", count)
		}

		ticker := time.NewTicker(c.refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.refresh(ctx); err != nil {
					c.log.Warn("registry refresh failed", "error", err)
				} else {
					c.mu.RLock()
					count := len(c.cached)
					c.mu.RUnlock()
					c.log.Info("registry refresh complete", "servers", count)
				}
			}
		}
	}()
}

// refresh fetches all servers from the registry and replaces the cache.
func (c *Client) refresh(ctx context.Context) error {
	servers, err := c.fetchFromRegistry(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.cached = servers
	c.mu.Unlock()
	return nil
}

// FetchAll returns every server from the registry cache. If the cache is
// empty (Start was not called or prefetch failed), it fetches on demand.
func (c *Client) FetchAll(ctx context.Context) ([]RegistryServer, error) {
	c.mu.RLock()
	if c.cached != nil {
		result := make([]RegistryServer, len(c.cached))
		copy(result, c.cached)
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()

	// Fallback: cache is empty, fetch on demand.
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	result := make([]RegistryServer, len(c.cached))
	copy(result, c.cached)
	c.mu.RUnlock()
	return result, nil
}

// Search filters servers by a case-insensitive substring match on name or description.
func (c *Client) Search(ctx context.Context, query string) ([]RegistryServer, error) {
	all, err := c.FetchAll(ctx)
	if err != nil {
		return nil, err
	}
	if query == "" {
		return all, nil
	}

	q := strings.ToLower(query)
	var matches []RegistryServer
	for _, srv := range all {
		if strings.Contains(strings.ToLower(srv.Name), q) ||
			strings.Contains(strings.ToLower(srv.Description), q) {
			matches = append(matches, srv)
		}
	}
	return matches, nil
}

// Get returns a single server by registry ID (name).
func (c *Client) Get(ctx context.Context, registryID string) (*RegistryServer, error) {
	all, err := c.FetchAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, srv := range all {
		if srv.Name == registryID {
			return &srv, nil
		}
	}
	return nil, nil
}

// pageSize is the maximum number of servers per page (registry max is 100).
const pageSize = 100

func (c *Client) fetchFromRegistry(ctx context.Context) ([]RegistryServer, error) {
	var all []RegistryServer
	var cursor string

	for {
		page, nextCursor, err := c.fetchPage(ctx, cursor)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if nextCursor == "" || len(page) == 0 {
			break
		}
		cursor = nextCursor
	}
	return all, nil
}

func (c *Client) fetchPage(ctx context.Context, cursor string) ([]RegistryServer, string, error) {
	u := c.baseURL + "/v0/servers?limit=" + fmt.Sprintf("%d", pageSize)
	if cursor != "" {
		u += "&cursor=" + url.QueryEscape(cursor)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", fmt.Errorf("registry: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("registry: fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("registry: unexpected status %d", resp.StatusCode)
	}

	var result RegistryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, "", fmt.Errorf("registry: decode: %w", err)
	}

	servers := make([]RegistryServer, len(result.Servers))
	for i, entry := range result.Servers {
		servers[i] = entry.Server
	}

	var nextCursor string
	if result.Metadata != nil {
		nextCursor = result.Metadata.NextCursor
	}
	return servers, nextCursor, nil
}
