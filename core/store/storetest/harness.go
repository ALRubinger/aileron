// Package storetest provides implementation-agnostic integration tests for
// store interfaces. Tests are written against the store.* interfaces so they
// work with any backend (Postgres, SQLite, etc.) — only the Harness wiring
// changes.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store"
)

// Harness holds all store implementations under test. Each test function
// draws the stores it needs from the harness.
type Harness struct {
	Enterprises       store.EnterpriseStore
	Users             store.UserStore
	UserAuthProviders store.UserAuthProviderStore
	Sessions          store.SessionStore
	VerificationCodes store.VerificationCodeStore
	SSOConfigs        store.SSOConfigStore
	ConnectedAccounts store.ConnectedAccountStore
	Drafts            store.DraftStore
	DraftFeedback     store.DraftFeedbackStore
	UserInstructions  store.UserInstructionStore
	UserKeyMaterials  store.UserKeyMaterialStore
	LLMConfigs        store.LLMConfigStore
	EscrowIndex       store.EscrowIndexStore
}

var seq atomic.Int64

// uid returns a unique ID with the given prefix.
func uid(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, seq.Add(1))
}

// now returns the current time truncated to microsecond precision to match
// PostgreSQL's timestamptz resolution.
func now() time.Time {
	return time.Now().Truncate(time.Microsecond)
}

// SeedEnterprise creates a minimal enterprise for FK dependencies.
func (h *Harness) SeedEnterprise(t *testing.T, ctx context.Context) model.Enterprise {
	t.Helper()
	id := uid("ent")
	n := now()
	e := model.Enterprise{
		ID:           id,
		Name:         "Test Enterprise",
		Slug:         id,
		Plan:         model.EnterprisePlanFree,
		BillingEmail: id + "@test.com",
		CreatedAt:    n,
		UpdatedAt:    n,
	}
	if err := h.Enterprises.Create(ctx, e); err != nil {
		t.Fatalf("SeedEnterprise: %v", err)
	}
	return e
}

// SeedUser creates a minimal user for FK dependencies.
func (h *Harness) SeedUser(t *testing.T, ctx context.Context, enterpriseID string) model.User {
	t.Helper()
	id := uid("usr")
	n := now()
	u := model.User{
		ID:           id,
		EnterpriseID: enterpriseID,
		Email:        id + "@test.com",
		DisplayName:  "Test User",
		Role:         model.UserRoleMember,
		Status:       model.UserStatusActive,
		CreatedAt:    n,
		UpdatedAt:    n,
	}
	if err := h.Users.Create(ctx, u); err != nil {
		t.Fatalf("SeedUser: %v", err)
	}
	return u
}

// assertNotFound asserts that err is a *store.ErrNotFound.
func assertNotFound(t *testing.T, err error) {
	t.Helper()
	var notFound *store.ErrNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
