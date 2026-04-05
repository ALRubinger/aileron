package mem_test

import (
	"context"
	"testing"

	api "github.com/ALRubinger/aileron/core/api/gen"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
)

func TestEnterpriseMCPServerStore_CRUD(t *testing.T) {
	ctx := context.Background()
	s := mem.NewEnterpriseMCPServerStore()

	srv := api.EnterpriseMCPServer{
		Id:           ptr("emcp_1"),
		EnterpriseId: ptr("ent_1"),
		Name:         "test-enterprise-server",
		Command:      []string{"npx", "-y", "@example/mcp-server"},
		AutoEnabled:  ptr(false),
	}
	if err := s.Create(ctx, srv); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "emcp_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "test-enterprise-server" {
		t.Errorf("Name = %q, want %q", got.Name, "test-enterprise-server")
	}
	if got.EnterpriseId == nil || *got.EnterpriseId != "ent_1" {
		t.Errorf("EnterpriseId = %v, want %q", got.EnterpriseId, "ent_1")
	}

	list, err := s.List(ctx, store.EnterpriseMCPServerFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List length = %d, want 1", len(list))
	}

	srv.Name = "updated-enterprise-server"
	if err := s.Update(ctx, srv); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = s.Get(ctx, "emcp_1")
	if got.Name != "updated-enterprise-server" {
		t.Errorf("Name after update = %q, want %q", got.Name, "updated-enterprise-server")
	}

	if err := s.Delete(ctx, "emcp_1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = s.Get(ctx, "emcp_1")
	if err == nil {
		t.Fatal("Get after Delete: expected error")
	}
}

func TestEnterpriseMCPServerStore_NotFound(t *testing.T) {
	ctx := context.Background()
	s := mem.NewEnterpriseMCPServerStore()

	_, err := s.Get(ctx, "nonexistent")
	var nf *store.ErrNotFound
	if !isErrNotFound(err, &nf) {
		t.Fatalf("expected ErrNotFound, got %T", err)
	}

	err = s.Update(ctx, api.EnterpriseMCPServer{Id: ptr("nonexistent"), Name: "x", Command: []string{"x"}})
	if !isErrNotFound(err, &nf) {
		t.Fatalf("Update nonexistent: expected ErrNotFound, got %v", err)
	}

	err = s.Delete(ctx, "nonexistent")
	if !isErrNotFound(err, &nf) {
		t.Fatalf("Delete nonexistent: expected ErrNotFound, got %v", err)
	}
}

func TestEnterpriseMCPServerStore_ListWithEnterpriseFilter(t *testing.T) {
	ctx := context.Background()
	s := mem.NewEnterpriseMCPServerStore()

	_ = s.Create(ctx, api.EnterpriseMCPServer{
		Id:           ptr("emcp_a"),
		EnterpriseId: ptr("ent_1"),
		Name:         "ent1-server",
		Command:      []string{"cmd"},
	})
	_ = s.Create(ctx, api.EnterpriseMCPServer{
		Id:           ptr("emcp_b"),
		EnterpriseId: ptr("ent_2"),
		Name:         "ent2-server",
		Command:      []string{"cmd"},
	})

	list, err := s.List(ctx, store.EnterpriseMCPServerFilter{EnterpriseID: "ent_1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("filtered list length = %d, want 1", len(list))
	}
	if list[0].Name != "ent1-server" {
		t.Errorf("filtered Name = %q, want %q", list[0].Name, "ent1-server")
	}
}

func TestEnterpriseMCPServerStore_ListWithAutoEnabledFilter(t *testing.T) {
	ctx := context.Background()
	s := mem.NewEnterpriseMCPServerStore()

	_ = s.Create(ctx, api.EnterpriseMCPServer{
		Id:           ptr("emcp_auto"),
		EnterpriseId: ptr("ent_1"),
		Name:         "auto-enabled-server",
		Command:      []string{"cmd"},
		AutoEnabled:  ptr(true),
	})
	_ = s.Create(ctx, api.EnterpriseMCPServer{
		Id:           ptr("emcp_manual"),
		EnterpriseId: ptr("ent_1"),
		Name:         "manual-server",
		Command:      []string{"cmd"},
		AutoEnabled:  ptr(false),
	})

	autoEnabled := true
	list, err := s.List(ctx, store.EnterpriseMCPServerFilter{
		EnterpriseID: "ent_1",
		AutoEnabled:  &autoEnabled,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("auto-enabled filtered list length = %d, want 1", len(list))
	}
	if list[0].Name != "auto-enabled-server" {
		t.Errorf("filtered Name = %q, want %q", list[0].Name, "auto-enabled-server")
	}
}
