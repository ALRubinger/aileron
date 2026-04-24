package source_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ALRubinger/aileron/internal/source"
	"golang.org/x/oauth2"
)

// staticTokenSource returns the same token every time.
type staticTokenSource struct {
	tok *oauth2.Token
}

func (s *staticTokenSource) Token() (*oauth2.Token, error) {
	return s.tok, nil
}

func TestPersistingTokenSource_SavesOnRefresh(t *testing.T) {
	original := &oauth2.Token{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
	}
	refreshed := &oauth2.Token{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		TokenType:    "bearer",
	}

	var saved atomic.Bool
	var savedJSON []byte
	saver := func(_ context.Context, newToken []byte) error {
		saved.Store(true)
		savedJSON = newToken
		return nil
	}

	pts := source.NewPersistingTokenSource(
		&staticTokenSource{tok: refreshed},
		original,
		saver,
		context.Background(),
	)

	tok, err := pts.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "new-access" {
		t.Errorf("expected new-access, got %s", tok.AccessToken)
	}
	if !saved.Load() {
		t.Fatal("expected saver to be called")
	}

	// Verify the saved JSON contains the refreshed token.
	var parsed oauth2.Token
	if err := json.Unmarshal(savedJSON, &parsed); err != nil {
		t.Fatalf("failed to unmarshal saved token: %v", err)
	}
	if parsed.RefreshToken != "new-refresh" {
		t.Errorf("expected new-refresh in saved token, got %s", parsed.RefreshToken)
	}
}

func TestPersistingTokenSource_NoSaveWhenUnchanged(t *testing.T) {
	original := &oauth2.Token{
		AccessToken:  "same-access",
		RefreshToken: "same-refresh",
	}

	var saved atomic.Bool
	saver := func(_ context.Context, _ []byte) error {
		saved.Store(true)
		return nil
	}

	pts := source.NewPersistingTokenSource(
		&staticTokenSource{tok: original},
		original,
		saver,
		context.Background(),
	)

	_, err := pts.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if saved.Load() {
		t.Error("saver should not be called when token is unchanged")
	}
}

func TestPersistingTokenSource_SavesOnlyOnce(t *testing.T) {
	original := &oauth2.Token{AccessToken: "old"}
	refreshed := &oauth2.Token{AccessToken: "new"}

	var saveCount atomic.Int32
	saver := func(_ context.Context, _ []byte) error {
		saveCount.Add(1)
		return nil
	}

	pts := source.NewPersistingTokenSource(
		&staticTokenSource{tok: refreshed},
		original,
		saver,
		context.Background(),
	)

	// Call Token() multiple times — saver should fire only once.
	for range 5 {
		if _, err := pts.Token(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if saveCount.Load() != 1 {
		t.Errorf("expected saver called once, got %d", saveCount.Load())
	}
}

func TestPersistingTokenSource_SaveErrorDoesNotFailToken(t *testing.T) {
	original := &oauth2.Token{AccessToken: "old"}
	refreshed := &oauth2.Token{AccessToken: "new"}

	saver := func(_ context.Context, _ []byte) error {
		return context.DeadlineExceeded // simulate vault write failure
	}

	pts := source.NewPersistingTokenSource(
		&staticTokenSource{tok: refreshed},
		original,
		saver,
		context.Background(),
	)

	tok, err := pts.Token()
	if err != nil {
		t.Fatalf("save failure should not propagate, got: %v", err)
	}
	if tok.AccessToken != "new" {
		t.Errorf("expected new access token, got %s", tok.AccessToken)
	}
}

func TestWithTokenSaver_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if source.GetTokenSaver(ctx) != nil {
		t.Fatal("expected nil saver on bare context")
	}

	var called bool
	saver := func(_ context.Context, _ []byte) error {
		called = true
		return nil
	}
	ctx = source.WithTokenSaver(ctx, saver)

	got := source.GetTokenSaver(ctx)
	if got == nil {
		t.Fatal("expected non-nil saver after WithTokenSaver")
	}
	if err := got(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected saver to be invoked")
	}
}
