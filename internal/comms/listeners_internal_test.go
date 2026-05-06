package comms

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/vault"
)

// bridgeMessages contract:
//   - Pushes incoming messages onto the queue with the autoDraft +
//     priority maps applied.
//   - Truncates Preview to 80 chars.
//   - Writes a `message_received` audit entry when AuditStateDir is
//     set; skips audit when empty.

func TestBridgeMessages_PushesAndAppliesFlags(t *testing.T) {
	q := NewNotifyQueue(10, nil)
	msgs := make(chan IncomingMessage, 3)

	auto := map[string]bool{"#dev": true}
	pri := map[string]string{"#alerts": "high"}
	go bridgeMessages(msgs, q, auto, pri, "", nopLogger())

	msgs <- IncomingMessage{ID: "1", Service: "slack", Channel: "#dev", Author: "alice", Body: "hi"}
	msgs <- IncomingMessage{ID: "2", Service: "slack", Channel: "#alerts", Author: "bob", Body: "WAKE UP"}
	msgs <- IncomingMessage{ID: "3", Service: "discord", Channel: "general", Author: "carol", Body: "elsewhere"}
	close(msgs)
	time.Sleep(50 * time.Millisecond)

	all := q.Messages()
	if len(all) != 3 {
		t.Fatalf("queue size = %d, want 3", len(all))
	}
	if !all[0].AutoDraft {
		t.Error("first message should have AutoDraft=true (#dev)")
	}
	if all[1].Priority != "high" {
		t.Errorf("second message Priority = %q, want high (#alerts)", all[1].Priority)
	}
	if all[2].Priority != "normal" {
		t.Errorf("third message default Priority = %q, want normal", all[2].Priority)
	}
}

func TestBridgeMessages_TruncatesPreview(t *testing.T) {
	q := NewNotifyQueue(10, nil)
	msgs := make(chan IncomingMessage, 1)
	go bridgeMessages(msgs, q, nil, nil, "", nopLogger())

	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	msgs <- IncomingMessage{ID: "1", Body: string(long)}
	close(msgs)
	time.Sleep(50 * time.Millisecond)

	all := q.Messages()
	if len(all) != 1 {
		t.Fatal("queue empty")
	}
	if len(all[0].Preview) > 80 {
		t.Errorf("preview length %d > 80", len(all[0].Preview))
	}
	if all[0].Body != string(long) {
		t.Error("full body should be preserved on the queue")
	}
}

func TestBridgeMessages_AuditWrite(t *testing.T) {
	dir := t.TempDir()
	q := NewNotifyQueue(10, nil)
	msgs := make(chan IncomingMessage, 1)
	go bridgeMessages(msgs, q, nil, nil, dir, nopLogger())

	msgs <- IncomingMessage{ID: "1", Service: "slack", Channel: "#dev", Author: "alice", Body: "hi", Timestamp: time.Now()}
	close(msgs)
	time.Sleep(50 * time.Millisecond)

	entries, err := audit.ReadMessageEntries(filepath.Join(dir, "audit"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Event != "message_received" {
		t.Errorf("audit entries = %+v", entries)
	}
}

// buildListeners contract: only builds a listener when both required
// tokens for the service are populated. Empty / partial config yields
// a zero-listener slice.

func TestBuildListeners_SkipsIncompleteSlackConfig(t *testing.T) {
	v := vault.NewMemVault()
	_ = v.Put(context.Background(), "slack-app", []byte("xapp"), vault.Metadata{})

	listeners, _, _, err := buildListeners(context.Background(), StartOptions{
		Notifications: &config.NotifyConfig{
			Slack: &config.SlackNotifyConfig{AppToken: "vault:slack-app"}, // bot token missing
		},
		Vault: v,
		Queue: NewNotifyQueue(10, nil),
		Log:   nopLogger(),
	})
	if err != nil {
		t.Fatalf("buildListeners: %v", err)
	}
	if len(listeners) != 0 {
		t.Errorf("expected 0 listeners for incomplete Slack config, got %d", len(listeners))
	}
}

func TestBuildListeners_BuildsSlackListener(t *testing.T) {
	v := vault.NewMemVault()
	_ = v.Put(context.Background(), "slack-app", []byte("xapp"), vault.Metadata{})
	_ = v.Put(context.Background(), "slack-bot", []byte("xoxb"), vault.Metadata{})

	listeners, autoDraft, priority, err := buildListeners(context.Background(), StartOptions{
		Notifications: &config.NotifyConfig{
			Slack: &config.SlackNotifyConfig{
				AppToken: "vault:slack-app",
				BotToken: "vault:slack-bot",
				Channels: []config.ChannelConfig{
					{Name: "#dev", AutoDraft: true},
					{Name: "#alerts", Priority: "high"},
				},
			},
		},
		Vault: v,
		Queue: NewNotifyQueue(10, nil),
		Log:   nopLogger(),
	})
	if err != nil {
		t.Fatalf("buildListeners: %v", err)
	}
	if len(listeners) != 1 {
		t.Fatalf("expected 1 Slack listener, got %d", len(listeners))
	}
	if listeners[0].Service() != "slack" {
		t.Errorf("listener service = %q, want slack", listeners[0].Service())
	}
	if !autoDraft["#dev"] {
		t.Error("expected #dev in autoDraft map")
	}
	if priority["#alerts"] != "high" {
		t.Errorf("priority[#alerts] = %q, want high", priority["#alerts"])
	}
}

func TestBuildListeners_BuildsDiscordListener(t *testing.T) {
	v := vault.NewMemVault()
	_ = v.Put(context.Background(), "discord-bot", []byte("token"), vault.Metadata{})

	listeners, _, _, err := buildListeners(context.Background(), StartOptions{
		Notifications: &config.NotifyConfig{
			Discord: &config.DiscordNotifyConfig{
				BotToken: "vault:discord-bot",
				Channels: []config.ChannelConfig{{Name: "general"}},
			},
		},
		Vault: v,
		Queue: NewNotifyQueue(10, nil),
		Log:   nopLogger(),
	})
	if err != nil {
		t.Fatalf("buildListeners: %v", err)
	}
	if len(listeners) != 1 || listeners[0].Service() != "discord" {
		t.Errorf("expected 1 Discord listener, got %+v", listeners)
	}
}

// Services contract: returns every registered service name.

func TestListenerRegistry_Services(t *testing.T) {
	r := NewListenerRegistry()
	r.Set("slack", &fakeListener{service: "slack"})
	r.Set("discord", &fakeListener{service: "discord"})
	got := r.Services()
	sort.Strings(got)
	want := []string{"discord", "slack"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Services = %v, want %v", got, want)
	}
}

// --- helpers ---

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeListener struct {
	service string
}

func (f *fakeListener) Service() string                                              { return f.service }
func (f *fakeListener) Connect(context.Context) error                                { return nil }
func (f *fakeListener) Listen(context.Context) (<-chan IncomingMessage, error)       { return nil, nil }
func (f *fakeListener) Send(_ context.Context, _ OutgoingMessage) error              { return nil }
func (f *fakeListener) Close() error                                                 { return nil }
