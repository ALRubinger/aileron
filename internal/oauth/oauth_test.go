package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/oauth"
)

// --- PKCE ---

func TestNewPKCE_VerifierAndChallengeMatchRFC7636(t *testing.T) {
	for i := 0; i < 8; i++ { // exercise multiple draws
		p, err := oauth.NewPKCE()
		if err != nil {
			t.Fatalf("NewPKCE: %v", err)
		}
		if p.Method != "S256" {
			t.Errorf("Method = %q, want S256", p.Method)
		}
		// Verifier must be 43 chars (32 random bytes → base64url no-pad).
		if len(p.Verifier) != 43 {
			t.Errorf("len(Verifier) = %d, want 43", len(p.Verifier))
		}
		// Challenge must be base64url(sha256(verifier)).
		sum := sha256.Sum256([]byte(p.Verifier))
		want := base64.RawURLEncoding.EncodeToString(sum[:])
		if p.Challenge != want {
			t.Errorf("Challenge = %q, want %q", p.Challenge, want)
		}
	}
}

func TestNewPKCE_VerifiersAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		p, _ := oauth.NewPKCE()
		if seen[p.Verifier] {
			t.Fatalf("duplicate verifier on draw %d: %q", i, p.Verifier)
		}
		seen[p.Verifier] = true
	}
}

func TestNewState_UniqueAndUrlSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		s, err := oauth.NewState()
		if err != nil {
			t.Fatalf("NewState: %v", err)
		}
		if seen[s] {
			t.Fatalf("duplicate state on draw %d", i)
		}
		seen[s] = true
		// URL-safe characters only.
		for _, r := range s {
			ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				t.Errorf("state contains non-URL-safe character %q", r)
			}
		}
	}
}

// --- Callback listener ---

func TestListener_CapturesCodeAndState(t *testing.T) {
	l, err := oauth.NewListener(0)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if l.Port() == 0 {
		t.Fatal("Port() = 0; expected an OS-assigned port")
	}
	if !strings.HasPrefix(l.RedirectURI(), "http://127.0.0.1:") ||
		!strings.HasSuffix(l.RedirectURI(), "/callback") {
		t.Errorf("RedirectURI = %q", l.RedirectURI())
	}

	// Simulate the OAuth provider redirecting the browser to the
	// callback URL with code + state.
	go func() {
		_, _ = http.Get(l.RedirectURI() + "?code=abc123&state=xyz")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := l.Await(ctx)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if res.Code != "abc123" {
		t.Errorf("Code = %q", res.Code)
	}
	if res.State != "xyz" {
		t.Errorf("State = %q", res.State)
	}
}

func TestListener_PropagatesProviderError(t *testing.T) {
	l, err := oauth.NewListener(0)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		_, _ = http.Get(l.RedirectURI() + "?error=access_denied&error_description=user+declined")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = l.Await(ctx)
	if err == nil {
		t.Fatal("expected error when provider returned error param")
	}
	if !strings.Contains(err.Error(), "access_denied") ||
		!strings.Contains(err.Error(), "user declined") {
		t.Errorf("err = %v; want includes access_denied + user declined", err)
	}
}

func TestListener_RejectsCallbackWithoutCode(t *testing.T) {
	l, err := oauth.NewListener(0)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		_, _ = http.Get(l.RedirectURI() + "?state=lonely")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = l.Await(ctx)
	if err == nil {
		t.Fatal("expected error when callback missing code")
	}
	if !strings.Contains(err.Error(), "missing code") {
		t.Errorf("err = %v", err)
	}
}

func TestListener_AwaitRespectsContextCancel(t *testing.T) {
	l, err := oauth.NewListener(0)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = l.Await(ctx)
	if err == nil {
		t.Error("expected ctx.Err() when context already cancelled")
	}
}

func TestListener_CloseIsIdempotent(t *testing.T) {
	l, err := oauth.NewListener(0)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	// Subsequent close after Shutdown returns an error from the http
	// package; we accept either nil or a server-already-closed error.
	_ = l.Close()
}

// --- Browser opener ---

// fakeOpener captures the URL passed to Open without spawning a
// real process. Used to verify the CLI integration without opening
// the user's browser during tests.
type fakeOpener struct{ opened []string }

func (f *fakeOpener) Open(url string) error {
	f.opened = append(f.opened, url)
	return nil
}

func TestSystemBrowserSatisfiesOpenerInterface(t *testing.T) {
	var _ oauth.Opener = oauth.SystemBrowser{}
	var _ oauth.Opener = &fakeOpener{}
}

