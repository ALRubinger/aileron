package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultRegistryURL is the official MCP Registry API endpoint.
	DefaultRegistryURL = "https://registry.modelcontextprotocol.io"

	// DefaultCacheTTL is how long the server list is cached.
	DefaultCacheTTL = 15 * time.Minute
)

// Client fetches and caches MCP server metadata from the official registry.
type Client struct {
	baseURL    string
	httpClient *http.Client
	cacheTTL   time.Duration

	mu        sync.RWMutex
	cached    []RegistryServer
	cachedAt  time.Time
}

// NewClient creates a registry client with sensible defaults.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		baseURL:    DefaultRegistryURL,
		httpClient: httpClient,
		cacheTTL:   DefaultCacheTTL,
	}
}

// WithBaseURL overrides the registry URL (useful for testing).
func (c *Client) WithBaseURL(url string) *Client {
	c.baseURL = url
	return c
}

// FetchAll returns every server from the registry, using the cache if fresh.
func (c *Client) FetchAll(ctx context.Context) ([]RegistryServer, error) {
	c.mu.RLock()
	if c.cached != nil && time.Since(c.cachedAt) < c.cacheTTL {
		result := make([]RegistryServer, len(c.cached))
		copy(result, c.cached)
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()

	servers, err := c.fetchFromRegistry(ctx)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.cached = servers
	c.cachedAt = time.Now()
	c.mu.Unlock()

	result := make([]RegistryServer, len(servers))
	copy(result, servers)
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

func (c *Client) fetchFromRegistry(ctx context.Context) ([]RegistryServer, error) {
	url := c.baseURL + "/v0/servers"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry: unexpected status %d", resp.StatusCode)
	}

	var result RegistryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("registry: decode: %w", err)
	}
	return result.Servers, nil
}
