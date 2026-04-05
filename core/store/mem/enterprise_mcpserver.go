package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// EnterpriseMCPServerStore is a thread-safe in-memory implementation of store.EnterpriseMCPServerStore.
type EnterpriseMCPServerStore struct {
	mu      sync.RWMutex
	servers map[string]api.EnterpriseMCPServer
}

// NewEnterpriseMCPServerStore returns an empty in-memory enterprise MCP server store.
func NewEnterpriseMCPServerStore() *EnterpriseMCPServerStore {
	return &EnterpriseMCPServerStore{servers: make(map[string]api.EnterpriseMCPServer)}
}

func (s *EnterpriseMCPServerStore) Create(_ context.Context, server api.EnterpriseMCPServer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.servers[*server.Id] = server
	return nil
}

func (s *EnterpriseMCPServerStore) Get(_ context.Context, serverID string) (api.EnterpriseMCPServer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	srv, ok := s.servers[serverID]
	if !ok {
		return api.EnterpriseMCPServer{}, &store.ErrNotFound{Entity: "enterprise_mcp_server", ID: serverID}
	}
	return srv, nil
}

func (s *EnterpriseMCPServerStore) List(_ context.Context, filter store.EnterpriseMCPServerFilter) ([]api.EnterpriseMCPServer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []api.EnterpriseMCPServer
	for _, srv := range s.servers {
		if filter.EnterpriseID != "" && (srv.EnterpriseId == nil || *srv.EnterpriseId != filter.EnterpriseID) {
			continue
		}
		if filter.AutoEnabled != nil && (srv.AutoEnabled == nil || *srv.AutoEnabled != *filter.AutoEnabled) {
			continue
		}
		result = append(result, srv)
	}
	return result, nil
}

func (s *EnterpriseMCPServerStore) Update(_ context.Context, server api.EnterpriseMCPServer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.servers[*server.Id]; !ok {
		return &store.ErrNotFound{Entity: "enterprise_mcp_server", ID: *server.Id}
	}
	s.servers[*server.Id] = server
	return nil
}

func (s *EnterpriseMCPServerStore) Delete(_ context.Context, serverID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.servers[serverID]; !ok {
		return &store.ErrNotFound{Entity: "enterprise_mcp_server", ID: serverID}
	}
	delete(s.servers, serverID)
	return nil
}
