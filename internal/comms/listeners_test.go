package comms_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/vault"
)

// ListenerRegistry contract:
//
//   - Set / Get round-trip a listener by service name.
//   - Len reflects the number of distinct services registered.
//   - CloseAll empties the registry and closes every listener; per-
//     listener Close errors are logged but don't abort the loop.
//   - Concurrent Set + Get is safe.

func TestListenerRegistry_SetGet(t *testing.T) {
	r := comms.NewListenerRegistry()
	l := &fakeListenerForReg{service: "slack"}
	r.Set("slack", l)
	got, ok := r.Get("slack")
	if !ok || got != l {
		t.Errorf("Get = (%v, %v), want fakeListener", got, ok)
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d, want 1", r.Len())
	}
}

func TestListenerRegistry_GetMissing(t *testing.T) {
	r := comms.NewListenerRegistry()
	if _, ok := r.Get("discord"); ok {
		t.Error("Get on missing service returned ok=true")
	}
}

func TestListenerRegistry_CloseAllClearsAndCloses(t *testing.T) {
	r := comms.NewListenerRegistry()
	a := &fakeListenerForReg{service: "slack"}
	b := &fakeListenerForReg{service: "discord"}
	r.Set("slack", a)
	r.Set("discord", b)

	r.CloseAll(slog.New(slog.NewTextHandler(io.Discard, nil)))

	if r.Len() != 0 {
		t.Errorf("Len after CloseAll = %d, want 0", r.Len())
	}
	if !a.closed || !b.closed {
		t.Error("listeners not closed")
	}
}

func TestListenerRegistry_CloseAllSurvivesPerListenerError(t *testing.T) {
	r := comms.NewListenerRegistry()
	bad := &fakeListenerForReg{service: "slack", closeErr: errors.New("boom")}
	good := &fakeListenerForReg{service: "discord"}
	r.Set("slack", bad)
	r.Set("discord", good)

	r.CloseAll(slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !good.closed {
		t.Error("good listener not closed when bad listener returned an error")
	}
	if r.Len() != 0 {
		t.Errorf("registry not cleared after CloseAll despite per-listener error")
	}
}

func TestListenerRegistry_ConcurrentSetGet(t *testing.T) {
	r := comms.NewListenerRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r.Set("svc", &fakeListenerForReg{service: "svc"})
		}()
		go func() {
			defer wg.Done()
			_, _ = r.Get("svc")
		}()
	}
	wg.Wait()
}

// StartListeners contract: with no channel implementations in tree
// (#525 removed Slack and Discord), it is a no-op that returns 0.
func TestStartListeners_NoOp(t *testing.T) {
	r := comms.NewListenerRegistry()
	q := comms.NewNotifyQueue(10, nil)
	started, err := comms.StartListeners(context.Background(), comms.StartOptions{
		Notifications: nil,
		Queue:         q,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, r)
	if err != nil {
		t.Fatalf("StartListeners: %v", err)
	}
	if started != 0 {
		t.Errorf("started = %d, want 0", started)
	}
}

func TestValidateNotificationTokens_NoOp(t *testing.T) {
	if err := comms.ValidateNotificationTokens(nil); err != nil {
		t.Errorf("nil config: %v", err)
	}
	if err := comms.ValidateNotificationTokens(&config.NotifyConfig{}); err != nil {
		t.Errorf("empty config: %v", err)
	}
}

func TestIsVaultRef(t *testing.T) {
	if !comms.IsVaultRef("vault:foo") {
		t.Error("IsVaultRef(vault:foo) should be true")
	}
	if comms.IsVaultRef("plaintext") {
		t.Error("IsVaultRef(plaintext) should be false")
	}
	if comms.IsVaultRef("") {
		t.Error("IsVaultRef(empty) should be false")
	}
}

func TestResolveVaultRef_Passthrough(t *testing.T) {
	got, err := comms.ResolveVaultRef(context.Background(), "plaintext", nil)
	if err != nil {
		t.Fatalf("ResolveVaultRef on plaintext: %v", err)
	}
	if got != "plaintext" {
		t.Errorf("got %q, want plaintext", got)
	}
}

func TestResolveVaultRef_LooksUpInVault(t *testing.T) {
	v := vault.NewMemVault()
	_ = v.Put(context.Background(), "app-token", []byte("xapp-resolved"), vault.Metadata{})
	got, err := comms.ResolveVaultRef(context.Background(), "vault:app-token", v)
	if err != nil {
		t.Fatalf("ResolveVaultRef: %v", err)
	}
	if got != "xapp-resolved" {
		t.Errorf("got %q, want xapp-resolved", got)
	}
}

// --- helpers ---

type fakeListenerForReg struct {
	service  string
	closed   bool
	closeErr error
}

func (f *fakeListenerForReg) Service() string                                                { return f.service }
func (f *fakeListenerForReg) Connect(context.Context) error                                  { return nil }
func (f *fakeListenerForReg) Listen(context.Context) (<-chan comms.IncomingMessage, error)   { return nil, nil }
func (f *fakeListenerForReg) Send(_ context.Context, _ comms.OutgoingMessage) error          { return nil }
func (f *fakeListenerForReg) Close() error {
	f.closed = true
	return f.closeErr
}
