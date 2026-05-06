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
// their service name (e.g. "slack", "discord"). The daemon's HTTP comms
// handlers (`send_message`, `draft_reply`) consult it to dispatch to
// the right backend after the user approves; `StartListeners` populates
// it once the vault is unlocked.
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
// abort the loop so a hung Slack websocket can't block Discord teardown.
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

// StartOptions bundles the inputs StartListeners needs. Extracted so
// callers don't grow a long positional argument list as the
// daemon-owned listener layer accretes responsibilities.
type StartOptions struct {
	// Notifications is the user-scoped Slack/Discord configuration.
	// Nil or with no Slack/Discord block means "no listeners to
	// start"; StartListeners returns immediately with an empty
	// registry contribution.
	Notifications *config.NotifyConfig

	// Vault holds the resolved tokens. Required when Notifications
	// references vault paths (the production case); the constructor
	// resolves each `vault:<name>` ref at startup.
	Vault vault.Vault

	// Queue is where every incoming message lands. A bridge goroutine
	// is spawned per started listener that funnels [IncomingMessage]
	// into [Queue.Push] with the channel's auto-draft + priority
	// flags applied.
	Queue *NotifyQueue

	// AuditStateDir is the directory under which `audit-YYYY-MM-DD.jsonl`
	// gets the `message_received` event written for each inbound
	// message. Empty disables audit emission.
	AuditStateDir string

	// Log scopes per-listener structured log entries. Required.
	Log *slog.Logger
}

// StartListeners resolves vault tokens, constructs the Slack and
// Discord listeners that the user configured, connects them, and
// spawns a bridge goroutine per listener that pushes inbound messages
// into the [NotifyQueue]. Successful starts are written to the
// supplied registry — the daemon's comms handlers consult the
// registry to dispatch outbound `send_message` / `draft_reply` calls
// after the user approves.
//
// Best-effort: a single listener that fails to connect is logged and
// skipped; the others still start. Return value is the count of
// successfully-started listeners so the caller can log "0 of 2
// listeners up" when the vault has stale tokens.
func StartListeners(ctx context.Context, opts StartOptions, registry *ListenerRegistry) (int, error) {
	if opts.Notifications == nil {
		return 0, nil
	}
	if opts.Queue == nil {
		return 0, fmt.Errorf("StartListeners requires a NotifyQueue")
	}
	if opts.Log == nil {
		return 0, fmt.Errorf("StartListeners requires a logger")
	}

	created, autoDraft, priority, err := buildListeners(ctx, opts)
	if err != nil {
		return 0, err
	}

	started := 0
	for _, l := range created {
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
	return started, nil
}

// buildListeners constructs the concrete Slack/Discord listeners from
// the user's notification config and returns the autoDraft + priority
// channel maps the bridge goroutine reads.
func buildListeners(ctx context.Context, opts StartOptions) ([]Listener, map[string]bool, map[string]string, error) {
	autoDraft := make(map[string]bool)
	priority := make(map[string]string)
	var listeners []Listener

	if cfg := opts.Notifications.Slack; cfg != nil && cfg.AppToken != "" && cfg.BotToken != "" {
		appToken, err := ResolveVaultRef(ctx, cfg.AppToken, opts.Vault)
		if err != nil {
			return nil, nil, nil, err
		}
		botToken, err := ResolveVaultRef(ctx, cfg.BotToken, opts.Vault)
		if err != nil {
			return nil, nil, nil, err
		}
		var userToken string
		if cfg.UserToken != "" {
			userToken, err = ResolveVaultRef(ctx, cfg.UserToken, opts.Vault)
			if err != nil {
				return nil, nil, nil, err
			}
		}
		channels := make([]string, 0, len(cfg.Channels))
		for _, ch := range cfg.Channels {
			channels = append(channels, ch.Name)
			if ch.AutoDraft {
				autoDraft[ch.Name] = true
			}
			if ch.Priority != "" {
				priority[ch.Name] = ch.Priority
			}
		}
		opts.Log.Info("slack listener configured",
			"channels", channels,
			"ignore", cfg.Ignore,
			"user_token", userToken != "",
		)
		listeners = append(listeners, NewSlackListener(appToken, botToken, userToken, channels, cfg.Ignore, opts.Log.With("component", "slack")))
	}

	if cfg := opts.Notifications.Discord; cfg != nil && cfg.BotToken != "" {
		botToken, err := ResolveVaultRef(ctx, cfg.BotToken, opts.Vault)
		if err != nil {
			return nil, nil, nil, err
		}
		channels := make([]string, 0, len(cfg.Channels))
		for _, ch := range cfg.Channels {
			channels = append(channels, ch.Name)
			if ch.Priority != "" {
				priority[ch.Name] = ch.Priority
			}
		}
		opts.Log.Info("discord listener configured",
			"channels", channels,
			"ignore", cfg.Ignore,
		)
		listeners = append(listeners, NewDiscordListener(botToken, channels, cfg.Ignore, opts.Log.With("component", "discord")))
	}

	return listeners, autoDraft, priority, nil
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
// notifications block is either empty or a vault reference. Plaintext
// tokens trip a fail-loud error so users don't accidentally commit
// secrets in `~/.aileron/config.yaml`.
//
// Called at daemon startup so the failure surfaces in the daemon log
// rather than mid-listener, and so `aileron status notifications` can
// surface the problem alongside the rest of the config snapshot.
func ValidateNotificationTokens(n *config.NotifyConfig) error {
	if n == nil {
		return nil
	}
	if cfg := n.Slack; cfg != nil {
		if err := validateTokenRef("slack.app_token", cfg.AppToken); err != nil {
			return err
		}
		if err := validateTokenRef("slack.bot_token", cfg.BotToken); err != nil {
			return err
		}
		if err := validateTokenRef("slack.user_token", cfg.UserToken); err != nil {
			return err
		}
	}
	if cfg := n.Discord; cfg != nil {
		if err := validateTokenRef("discord.bot_token", cfg.BotToken); err != nil {
			return err
		}
	}
	return nil
}

func validateTokenRef(field, value string) error {
	if value == "" || IsVaultRef(value) {
		return nil
	}
	return fmt.Errorf("%s contains a plaintext token — use 'aileron secret set <name>' and reference it as 'vault:<name>' instead", field)
}
