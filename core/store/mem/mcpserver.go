package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// MCPServerStore is a thread-safe in-memory implementation of store.MCPServerStore.
type MCPServerStore struct {
	mu      sync.RWMutex
	servers map[string]api.MCPServerConfig
}

// NewMCPServerStore returns an empty in-memory MCP server store.
func NewMCPServerStore() *MCPServerStore {
	return &MCPServerStore{servers: make(map[string]api.MCPServerConfig)}
}

func (s *MCPServerStore) Create(_ context.Context, server api.MCPServerConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.servers[*server.Id] = server
	return nil
}

func (s *MCPServerStore) Get(_ context.Context, serverID string) (api.MCPServerConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	srv, ok := s.servers[serverID]
	if !ok {
		return api.MCPServerConfig{}, &store.ErrNotFound{Entity: "mcp_server", ID: serverID}
	}
	return srv, nil
}

func (s *MCPServerStore) List(_ context.Context, filter store.MCPServerFilter) ([]api.MCPServerConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []api.MCPServerConfig
	for _, srv := range s.servers {
		if filter.Status != nil && (srv.Status == nil || *srv.Status != *filter.Status) {
			continue
		}
		result = append(result, srv)
	}
	return result, nil
}

func (s *MCPServerStore) Update(_ context.Context, server api.MCPServerConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.servers[*server.Id]; !ok {
		return &store.ErrNotFound{Entity: "mcp_server", ID: *server.Id}
	}
	s.servers[*server.Id] = server
	return nil
}

func (s *MCPServerStore) Delete(_ context.Context, serverID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.servers[serverID]; !ok {
		return &store.ErrNotFound{Entity: "mcp_server", ID: serverID}
	}
	delete(s.servers, serverID)
	return nil
}
