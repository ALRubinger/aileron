package storetest

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
)

// TestUserAuthProviderStore exercises the store.UserAuthProviderStore contract.
func TestUserAuthProviderStore(t *testing.T, h *Harness) {
	ctx := context.Background()
	ent := h.SeedEnterprise(t, ctx)
	user := h.SeedUser(t, ctx, ent.ID)

	t.Run("CreateAndGetByProviderSubject", func(t *testing.T) {
		id := uid("uap")
		subjectID := uid("sub")
		link := model.UserAuthProvider{
			ID:        id,
			UserID:    user.ID,
			Provider:  "google",
			SubjectID: subjectID,
			CreatedAt: now(),
		}
		if err := h.UserAuthProviders.Create(ctx, link); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := h.UserAuthProviders.GetByProviderSubject(ctx, "google", subjectID)
		if err != nil {
			t.Fatalf("GetByProviderSubject: %v", err)
		}
		if got.ID != id {
			t.Errorf("ID = %q, want %q", got.ID, id)
		}
		if got.UserID != user.ID {
			t.Errorf("UserID = %q, want %q", got.UserID, user.ID)
		}
	})

	t.Run("GetByProviderSubjectNotFound", func(t *testing.T) {
		_, err := h.UserAuthProviders.GetByProviderSubject(ctx, "github", "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("ListByUser", func(t *testing.T) {
		u2 := h.SeedUser(t, ctx, ent.ID)
		for _, p := range []string{"google", "github"} {
			link := model.UserAuthProvider{
				ID: uid("uap"), UserID: u2.ID,
				Provider: p, SubjectID: uid("sub"),
				CreatedAt: now(),
			}
			if err := h.UserAuthProviders.Create(ctx, link); err != nil {
				t.Fatalf("Create %s: %v", p, err)
			}
		}

		links, err := h.UserAuthProviders.ListByUser(ctx, u2.ID)
		if err != nil {
			t.Fatalf("ListByUser: %v", err)
		}
		if len(links) != 2 {
			t.Errorf("got %d links, want 2", len(links))
		}
	})

	t.Run("Delete", func(t *testing.T) {
		id := uid("uap")
		link := model.UserAuthProvider{
			ID: id, UserID: user.ID,
			Provider: "okta", SubjectID: uid("sub"),
			CreatedAt: now(),
		}
		if err := h.UserAuthProviders.Create(ctx, link); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := h.UserAuthProviders.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		links, err := h.UserAuthProviders.ListByUser(ctx, user.ID)
		if err != nil {
			t.Fatalf("ListByUser: %v", err)
		}
		for _, l := range links {
			if l.ID == id {
				t.Error("deleted link still returned by ListByUser")
			}
		}
	})

	t.Run("DeleteNotFound", func(t *testing.T) {
		err := h.UserAuthProviders.Delete(ctx, "nonexistent")
		assertNotFound(t, err)
	})

	t.Run("DeleteByUserAndProvider", func(t *testing.T) {
		u3 := h.SeedUser(t, ctx, ent.ID)
		link := model.UserAuthProvider{
			ID: uid("uap"), UserID: u3.ID,
			Provider: "saml", SubjectID: uid("sub"),
			CreatedAt: now(),
		}
		if err := h.UserAuthProviders.Create(ctx, link); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := h.UserAuthProviders.DeleteByUserAndProvider(ctx, u3.ID, "saml"); err != nil {
			t.Fatalf("DeleteByUserAndProvider: %v", err)
		}

		links, err := h.UserAuthProviders.ListByUser(ctx, u3.ID)
		if err != nil {
			t.Fatalf("ListByUser: %v", err)
		}
		if len(links) != 0 {
			t.Errorf("expected 0 links after delete, got %d", len(links))
		}
	})

	t.Run("DeleteByUserAndProviderNotFound", func(t *testing.T) {
		err := h.UserAuthProviders.DeleteByUserAndProvider(ctx, user.ID, "nonexistent_provider")
		assertNotFound(t, err)
	})
}
