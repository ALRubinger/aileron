package mem_test

import (
	"context"
	"testing"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
)

func ptr[T any](v T) *T { return &v }

func TestMCPServerStore_CRUD(t *testing.T) {
	ctx := context.Background()
	s := mem.NewMCPServerStore()

	// Create
	srv := api.MCPServerConfig{
		Id:      ptr("mcp_1"),
		Name:    "test-server",
		Command: []string{"npx", "-y", "@example/mcp-server"},
		Status:  ptr(api.MCPServerConfigStatusStopped),
	}
	if err := s.Create(ctx, srv); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Get
	got, err := s.Get(ctx, "mcp_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "test-server" {
		t.Errorf("Name = %q, want %q", got.Name, "test-server")
	}
	if len(got.Command) != 3 {
		t.Errorf("Command length = %d, want 3", len(got.Command))
	}

	// List
	list, err := s.List(ctx, store.MCPServerFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List length = %d, want 1", len(list))
	}

	// Update
	srv.Name = "updated-server"
	if err := s.Update(ctx, srv); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = s.Get(ctx, "mcp_1")
	if got.Name != "updated-server" {
		t.Errorf("Name after update = %q, want %q", got.Name, "updated-server")
	}

	// Delete
	if err := s.Delete(ctx, "mcp_1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = s.Get(ctx, "mcp_1")
	if err == nil {
		t.Fatal("Get after Delete: expected error")
	}
}

func TestMCPServerStore_NotFound(t *testing.T) {
	ctx := context.Background()
	s := mem.NewMCPServerStore()

	_, err := s.Get(ctx, "nonexistent")
	var nf *store.ErrNotFound
	if err == nil {
		t.Fatal("expected ErrNotFound")
	}
	if !isErrNotFound(err, &nf) {
		t.Fatalf("expected ErrNotFound, got %T", err)
	}

	err = s.Update(ctx, api.MCPServerConfig{Id: ptr("nonexistent"), Name: "x", Command: []string{"x"}})
	if !isErrNotFound(err, &nf) {
		t.Fatalf("Update nonexistent: expected ErrNotFound, got %v", err)
	}

	err = s.Delete(ctx, "nonexistent")
	if !isErrNotFound(err, &nf) {
		t.Fatalf("Delete nonexistent: expected ErrNotFound, got %v", err)
	}
}

func TestMCPServerStore_ListWithStatusFilter(t *testing.T) {
	ctx := context.Background()
	s := mem.NewMCPServerStore()

	_ = s.Create(ctx, api.MCPServerConfig{
		Id:      ptr("mcp_a"),
		Name:    "running-server",
		Command: []string{"cmd"},
		Status:  ptr(api.MCPServerConfigStatusRunning),
	})
	_ = s.Create(ctx, api.MCPServerConfig{
		Id:      ptr("mcp_b"),
		Name:    "stopped-server",
		Command: []string{"cmd"},
		Status:  ptr(api.MCPServerConfigStatusStopped),
	})

	running := api.MCPServerConfigStatusRunning
	list, err := s.List(ctx, store.MCPServerFilter{Status: &running})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("filtered list length = %d, want 1", len(list))
	}
	if list[0].Name != "running-server" {
		t.Errorf("filtered Name = %q, want %q", list[0].Name, "running-server")
	}
}

func TestMCPServerStore_ListWithUserFilter(t *testing.T) {
	ctx := context.Background()
	s := mem.NewMCPServerStore()

	_ = s.Create(ctx, api.MCPServerConfig{
		Id:      ptr("mcp_a"),
		Name:    "user-a-server",
		Command: []string{"cmd"},
		UserId:  ptr("usr_a"),
	})
	_ = s.Create(ctx, api.MCPServerConfig{
		Id:      ptr("mcp_b"),
		Name:    "user-b-server",
		Command: []string{"cmd"},
		UserId:  ptr("usr_b"),
	})
	_ = s.Create(ctx, api.MCPServerConfig{
		Id:      ptr("mcp_c"),
		Name:    "no-owner-server",
		Command: []string{"cmd"},
	})

	// Filter by user A — should only get user A's server.
	list, err := s.List(ctx, store.MCPServerFilter{UserID: "usr_a"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("filtered list length = %d, want 1", len(list))
	}
	if list[0].Name != "user-a-server" {
		t.Errorf("filtered Name = %q, want %q", list[0].Name, "user-a-server")
	}

	// No user filter — returns all servers.
	all, err := s.List(ctx, store.MCPServerFilter{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered list length = %d, want 3", len(all))
	}
}

func isErrNotFound(err error, target **store.ErrNotFound) bool {
	if err == nil {
		return false
	}
	var nf *store.ErrNotFound
	ok := false
	if e, isNF := err.(*store.ErrNotFound); isNF {
		nf = e
		ok = true
	}
	if ok && target != nil {
		*target = nf
	}
	return ok
}
