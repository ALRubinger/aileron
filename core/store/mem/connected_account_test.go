package mem_test

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
	"github.com/ALRubinger/aileron/core/store/mem"
)

func newTestAccount(id, userID string, provider model.ConnectedAccountProvider) model.ConnectedAccount {
	return model.ConnectedAccount{
		ID:        id,
		UserID:    userID,
		Provider:  provider,
		Scopes:    []string{"email", "calendar"},
		Status:    model.ConnectedAccountStatusActive,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
}

func TestConnectedAccountStore_CreateAndGet(t *testing.T) {
	s := mem.NewConnectedAccountStore()
	ctx := context.Background()

	acc := newTestAccount("conn_1", "usr_1", model.ConnectedAccountProviderGmail)
	if err := s.Create(ctx, acc); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "conn_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "conn_1" || got.UserID != "usr_1" || got.Provider != model.ConnectedAccountProviderGmail {
		t.Fatalf("unexpected account: %+v", got)
	}
}

func TestConnectedAccountStore_GetNotFound(t *testing.T) {
	s := mem.NewConnectedAccountStore()
	_, err := s.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
	var nf *store.ErrNotFound
	if !isErrNotFound(err, &nf) {
		t.Fatalf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestConnectedAccountStore_List(t *testing.T) {
	s := mem.NewConnectedAccountStore()
	ctx := context.Background()

	s.Create(ctx, newTestAccount("conn_1", "usr_1", model.ConnectedAccountProviderGmail))
	s.Create(ctx, newTestAccount("conn_2", "usr_1", model.ConnectedAccountProviderGoogleCalendar))
	s.Create(ctx, newTestAccount("conn_3", "usr_2", model.ConnectedAccountProviderGmail))

	// List all for user 1.
	accounts, err := s.List(ctx, store.ConnectedAccountFilter{UserID: "usr_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts for usr_1, got %d", len(accounts))
	}

	// Filter by provider.
	gmail := model.ConnectedAccountProviderGmail
	accounts, err = s.List(ctx, store.ConnectedAccountFilter{UserID: "usr_1", Provider: &gmail})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 gmail account for usr_1, got %d", len(accounts))
	}

	// Filter by status.
	expired := model.ConnectedAccountStatusExpired
	accounts, err = s.List(ctx, store.ConnectedAccountFilter{UserID: "usr_1", Status: &expired})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("expected 0 expired accounts, got %d", len(accounts))
	}
}

func TestConnectedAccountStore_ListByExternalIDs(t *testing.T) {
	s := mem.NewConnectedAccountStore()
	ctx := context.Background()

	slack := model.ConnectedAccountProviderSlack

	// Create Slack accounts with external IDs.
	acct1 := newTestAccount("conn_s1", "usr_1", slack)
	acct1.ExternalUserID = "U111"
	acct1.ExternalTeamID = "T001"
	s.Create(ctx, acct1)

	acct2 := newTestAccount("conn_s2", "usr_2", slack)
	acct2.ExternalUserID = "U222"
	acct2.ExternalTeamID = "T001"
	s.Create(ctx, acct2)

	// Look up by team + user.
	accounts, err := s.List(ctx, store.ConnectedAccountFilter{
		Provider:       &slack,
		ExternalTeamID: "T001",
		ExternalUserID: "U111",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].ID != "conn_s1" {
		t.Errorf("expected conn_s1, got %s", accounts[0].ID)
	}

	// Team-only filter returns both.
	accounts, err = s.List(ctx, store.ConnectedAccountFilter{
		Provider:       &slack,
		ExternalTeamID: "T001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts for team T001, got %d", len(accounts))
	}

	// Unknown team returns empty.
	accounts, err = s.List(ctx, store.ConnectedAccountFilter{
		Provider:       &slack,
		ExternalTeamID: "T999",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("expected 0 accounts for unknown team, got %d", len(accounts))
	}
}

func TestConnectedAccountStore_Delete(t *testing.T) {
	s := mem.NewConnectedAccountStore()
	ctx := context.Background()

	acc := newTestAccount("conn_1", "usr_1", model.ConnectedAccountProviderGmail)
	s.Create(ctx, acc)

	if err := s.Delete(ctx, "conn_1"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get(ctx, "conn_1")
	if err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestConnectedAccountStore_DeleteNotFound(t *testing.T) {
	s := mem.NewConnectedAccountStore()
	err := s.Delete(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

