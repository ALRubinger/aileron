package mem

import (
	"context"
	"sync"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
)

// ConnectorStore is a thread-safe in-memory implementation of store.ConnectorStore.
type ConnectorStore struct {
	mu         sync.RWMutex
	connectors map[string]api.Connector
}

// NewConnectorStore returns an empty in-memory connector store.
func NewConnectorStore() *ConnectorStore {
	return &ConnectorStore{connectors: make(map[string]api.Connector)}
}

func (s *ConnectorStore) Create(_ context.Context, conn api.Connector) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connectors[conn.ConnectorId] = conn
	return nil
}

func (s *ConnectorStore) Get(_ context.Context, connectorID string) (api.Connector, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.connectors[connectorID]
	if !ok {
		return api.Connector{}, &store.ErrNotFound{Entity: "connector", ID: connectorID}
	}
	return c, nil
}

func (s *ConnectorStore) List(_ context.Context, filter store.ConnectorFilter) ([]api.Connector, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []api.Connector
	for _, c := range s.connectors {
		if filter.WorkspaceID != "" && c.WorkspaceId != filter.WorkspaceID {
			continue
		}
		if filter.Type != nil && c.Type != *filter.Type {
			continue
		}
		result = append(result, c)
	}
	return result, nil
}

func (s *ConnectorStore) Update(_ context.Context, conn api.Connector) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.connectors[conn.ConnectorId]; !ok {
		return &store.ErrNotFound{Entity: "connector", ID: conn.ConnectorId}
	}
	s.connectors[conn.ConnectorId] = conn
	return nil
}
