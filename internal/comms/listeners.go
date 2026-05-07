package comms

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/vault"
)

// vaultPrefix marks a config value as a reference to a vault entry
// (e.g. `vault:slack-app-token`). Tokens stored as plaintext are
// rejected at config-load time.
const vaultPrefix = "vault:"

// IsVaultRef reports whether v is a vault reference.
func IsVaultRef(v string) bool {
	return strings.HasPrefix(v, vaultPrefix)
}

// ResolveVaultRef returns the underlying value for a vault reference,
// or v itself if it is not a reference.
func ResolveVaultRef(ctx context.Context, v string, vlt vault.Vault) (string, error) {
	if !IsVaultRef(v) {
		return v, nil
	}
	if vlt == nil {
		return "", fmt.Errorf("vault reference %q requires a vault", v)
	}
	name := strings.TrimPrefix(v, vaultPrefix)
	secret, err := vlt.Get(ctx, name)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", v, err)
	}
	return string(secret.Value), nil
}

// ListenerRegistry is a thread-safe map of active listeners keyed by
// their service name. The daemon's HTTP comms handlers (`send_message`,
// `draft_reply`) consult it to dispatch to the right backend after
// the user approves.
//
// Empty registry is a valid steady state — when the user has not
// configured any listeners, send-shaped tools fail with 503 rather
// than registering an approval the daemon could never dispatch.
type ListenerRegistry struct {
	mu        sync.RWMutex
	listeners map[string]Listener
}

// NewListenerRegistry creates an empty registry.
func NewListenerRegistry() *ListenerRegistry {
	return &ListenerRegistry{listeners: make(map[string]Listener)}
}

// Set registers (or replaces) the listener for a service.
func (r *ListenerRegistry) Set(service string, l Listener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners[service] = l
}

// Get returns the listener for the given service.
func (r *ListenerRegistry) Get(service string) (Listener, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	l, ok := r.listeners[service]
	return l, ok
}

// Len returns the number of registered listeners. Useful for "listeners
// ready?" checks in handlers and tests.
func (r *ListenerRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.listeners)
}

// Services returns the names of all registered services. Sort order
// is unspecified.
func (r *ListenerRegistry) Services() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.listeners))
	for s := range r.listeners {
		out = append(out, s)
	}
	return out
}

// CloseAll shuts down every registered listener and clears the
// registry. Best-effort; per-listener errors are logged but do not
// abort the loop so a hung listener can't block teardown.
func (r *ListenerRegistry) CloseAll(log *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for service, l := range r.listeners {
		if err := l.Close(); err != nil && log != nil {
			log.Warn("listener close failed", "service", service, "error", err)
		}
	}
	r.listeners = make(map[string]Listener)
}

// StartOptions bundles the inputs StartListeners needs. With no
// channel implementations currently shipped (#525 removed the Slack
// and Discord listeners), this is a placeholder for future channel
// listeners; StartListeners is a no-op until one is wired in.
type StartOptions struct {
	// Notifications is the user-scoped channel configuration.
	Notifications *config.NotifyConfig

	// Vault holds resolved tokens for any future listener that needs them.
	Vault vault.Vault

	// Queue is where any incoming message would land. Required when
	// listeners are present.
	Queue *NotifyQueue

	// AuditStateDir is the directory under which `audit-YYYY-MM-DD.jsonl`
	// gets the `message_received` event written for each inbound
	// message. Empty disables audit emission.
	AuditStateDir string

	// Log scopes per-listener structured log entries. Required when
	// listeners are present.
	Log *slog.Logger
}

// StartListeners constructs and starts the channel listeners the user
// configured, registering each on the supplied registry. With no
// channel implementations currently in tree this is a no-op.
//
// Returns the count of successfully-started listeners.
func StartListeners(ctx context.Context, opts StartOptions, registry *ListenerRegistry) (int, error) {
	_ = ctx
	_ = opts
	_ = registry
	return 0, nil
}

// startBuiltListeners runs the connect / listen / bridge phase against
// a slice of listeners. Tests use this to drive fake [Listener]s.
//
// Best-effort: a single listener that fails Connect or Listen is
// logged and skipped; the others still start. Returns the count of
// successfully-started listeners.
func startBuiltListeners(ctx context.Context, listeners []Listener, autoDraft map[string]bool, priority map[string]string, opts StartOptions, registry *ListenerRegistry) int {
	started := 0
	for _, l := range listeners {
		if err := l.Connect(ctx); err != nil {
			opts.Log.Warn("listener connect failed", "service", l.Service(), "error", err)
			continue
		}
		msgs, err := l.Listen(ctx)
		if err != nil {
			opts.Log.Warn("listener listen failed", "service", l.Service(), "error", err)
			continue
		}
		opts.Log.Info("listener started", "service", l.Service())
		registry.Set(l.Service(), l)
		started++
		go bridgeMessages(msgs, opts.Queue, autoDraft, priority, opts.AuditStateDir, opts.Log)
	}
	return started
}

// bridgeMessages reads from a listener's IncomingMessage channel and
// pushes each message into the daemon-owned NotifyQueue with the
// configured auto-draft and priority flags applied. Also writes a
// `message_received` audit entry when an audit dir is configured.
func bridgeMessages(msgs <-chan IncomingMessage, queue *NotifyQueue, autoDraft map[string]bool, priority map[string]string, auditStateDir string, log *slog.Logger) {
	for msg := range msgs {
		preview := msg.Body
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}
		pri := priority[msg.Channel]
		if pri == "" {
			pri = "normal"
		}
		log.Debug("message received",
			"service", msg.Service,
			"channel", msg.Channel,
			"author", msg.Author,
			"priority", pri,
			"preview", preview,
		)
		queue.Push(Message{
			ID:        msg.ID,
			Source:    msg.Service,
			Channel:   msg.Channel,
			Author:    msg.Author,
			Preview:   preview,
			Body:      msg.Body,
			Timestamp: msg.Timestamp,
			AutoDraft: autoDraft[msg.Channel],
			Priority:  pri,
		})
		if auditStateDir != "" {
			audit.AppendMessageEntry(audit.DailyPath(auditStateDir), audit.MessageEntry{
				Timestamp: msg.Timestamp,
				Event:     "message_received",
				Service:   msg.Service,
				Channel:   msg.Channel,
				Author:    msg.Author,
				Body:      msg.Body,
			})
		}
	}
}

// ValidateNotificationTokens checks that every token in the
// notifications block is either empty or a vault reference. With no
// channel implementations in tree there are no token fields to
// validate; the function is a no-op kept for call-site stability.
func ValidateNotificationTokens(n *config.NotifyConfig) error {
	_ = n
	return nil
}
