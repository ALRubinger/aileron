// Package main is the entry point for the Aileron control plane daemon.
//
// Under ADR-0012 the daemon runs as one user-scoped long-lived process,
// auto-spawned on first need by CLI / launch. By default it binds an
// ephemeral port on 127.0.0.1 and advertises its URL via
// `~/.aileron/daemon.json`; cloud / container deployments override the
// bind address via `--bind`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/app"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/sessions/jsonl"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/version"
	"golang.org/x/term"
)

func main() {
	var bind string
	flag.StringVar(&bind, "bind", "", "TCP bind address (default 127.0.0.1:0 ephemeral; cloud sets e.g. 0.0.0.0:8080)")
	flag.Parse()

	log := newLogger(os.Getenv("AILERON_LOG_LEVEL"))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := startDaemon(ctx, bind, log); err != nil {
		log.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}
}

// startDaemon resolves the production state directory + vault path and
// drives [run] under the supplied context. Extracted from main so
// tests can exercise the wiring with a cancellable ctx and a
// HOME-overridden state directory.
func startDaemon(ctx context.Context, bind string, log *slog.Logger) error {
	stateDir, err := defaultStateDir()
	if err != nil {
		return err
	}
	return run(ctx, log, options{
		BindAddr:  bind,
		StateDir:  stateDir,
		VaultPath: launch.DefaultVaultPath(),
		IsTTY:     term.IsTerminal(int(os.Stdin.Fd())),
		Stderr:    os.Stderr,
	})
}

// newLogger builds the daemon's structured JSON logger at the level
// implied by env ("debug" / "warn" / "error"; anything else → info).
func newLogger(env string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(env)}))
}

// parseLogLevel maps the AILERON_LOG_LEVEL env value to a slog.Level.
func parseLogLevel(env string) slog.Level {
	switch env {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// options holds the inputs run needs. Extracted so tests can construct
// it with a t.TempDir()-scoped state directory.
type options struct {
	// BindAddr overrides the daemon's TCP bind address. Empty means
	// "127.0.0.1:0" — an OS-assigned ephemeral port, the right choice
	// for the local-daemon model where the URL is discovered via
	// daemon.json rather than typed by users.
	BindAddr string
	// StateDir is the directory holding daemon.json, daemon.pid, and
	// daemon.lock. ~/.aileron in production; t.TempDir() in tests.
	StateDir string
	// VaultPath is the file path for the persistent vault.
	VaultPath string
	// IsTTY controls whether the vault prompt may run interactively.
	IsTTY bool
	// Stderr is the stream selectVault uses for prompts. Set to
	// os.Stderr in production; bytes.Buffer in tests.
	Stderr io.Writer
}

// run owns the daemon lifecycle: acquire the singleton lock, bind,
// publish discovery files, serve until ctx is canceled, then shut down
// cleanly and remove the discovery files.
//
// Lock ordering is deliberate. Defers run LIFO, so the lock release
// runs *after* discovery.Remove, ensuring no overlap window where a
// fresh daemon could acquire the lock and observe stale daemon.json.
func run(ctx context.Context, log *slog.Logger, opts options) error {
	lockCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	releaseLock, err := discovery.Lock(lockCtx, opts.StateDir)
	if err != nil {
		return fmt.Errorf("daemon already running, or could not acquire %s/%s: %w",
			opts.StateDir, discovery.LockFile, err)
	}
	defer func() { _ = releaseLock() }()

	cfg, err := selectVault(log, opts.VaultPath, opts.IsTTY, nil, opts.Stderr)
	if err != nil {
		return err
	}

	// Persistent launch-session records (ADR-0012). One JSONL file per
	// daemon, lives in stateDir alongside the discovery files. Reaped
	// of any sessions left running (EndedAt nil) on Open — see the
	// jsonl package for the orphan-reaping contract.
	sessionStore, err := jsonl.New(filepath.Join(opts.StateDir, "sessions.jsonl"))
	if err != nil {
		return fmt.Errorf("open sessions store: %w", err)
	}
	defer func() {
		if err := sessionStore.Close(); err != nil {
			log.Warn("closing sessions store", "error", err)
		}
	}()
	cfg.Sessions = sessionStore

	// Comms wiring (ADR-0012 step 9B-2). The daemon owns Slack/Discord
	// listeners now; the launch product no longer binds a per-session
	// unix socket. Lazy startup: listener tokens come from the user's
	// vault, which is locked at this point. The OnVaultUnlock callback
	// fires from POST /v1/vault/unlock and is where listener startup
	// actually runs — until the user unlocks, /comms/messages returns
	// an empty queue and /comms/send returns "no listener for service".
	aileronCfg, err := config.LoadAileronConfig(config.DefaultAileronConfigPath())
	if err != nil {
		// Surface but don't abort — a malformed config.yaml shouldn't
		// stop the daemon serving the rest of its endpoints. Vault,
		// actions, and approvals all still work without notifications.
		log.Warn("loading aileron config", "error", err)
		aileronCfg = &config.AileronConfig{}
	}
	if err := comms.ValidateNotificationTokens(aileronCfg.Notifications); err != nil {
		log.Warn("notifications config rejected", "error", err)
		aileronCfg.Notifications = nil
	}

	notifyQueue := comms.NewNotifyQueue(100, nil)
	if aileronCfg.Notifications != nil && aileronCfg.Notifications.QuietHours != nil {
		notifyQueue.SetQuietHours(aileronCfg.Notifications.QuietHours)
	}
	listenerRegistry := comms.NewListenerRegistry()
	cfg.NotifyQueue = notifyQueue
	cfg.Listeners = listenerRegistry
	cfg.AuditStateDir = opts.StateDir

	// Vault-unlock callback: when the user unlocks the local vault via
	// the webapp passphrase modal, resolve Slack/Discord tokens and
	// start listeners. No-op when the user hasn't configured
	// notifications, when listeners are already running (Set replaces
	// rather than duplicates per-service), or when the unlocked vault
	// is the same one we already saw — relock teardown is deliberately
	// out of scope (#454 acceptance criteria).
	cfg.OnVaultUnlock = func(v vault.Vault) {
		if listenerRegistry.Len() > 0 {
			log.Debug("listeners already running; skipping startup")
			return
		}
		started, err := comms.StartListeners(ctx, comms.StartOptions{
			Notifications: aileronCfg.Notifications,
			Vault:         v,
			Queue:         notifyQueue,
			AuditStateDir: opts.StateDir,
			Log:           log,
		}, listenerRegistry)
		if err != nil {
			log.Warn("listener startup failed", "error", err)
			return
		}
		log.Info("comms listeners started after vault unlock", "started", started)
	}
	defer listenerRegistry.CloseAll(log)

	// If the daemon launched with a pre-unlocked vault (the
	// `selectVault` interactive path on a TTY), fire the callback
	// immediately so listeners come up before the first /comms/*
	// request lands. The webapp-driven unlock path drives the same
	// callback later via UnlockLocalVault.
	if cfg.Vault != nil {
		cfg.OnVaultUnlock(cfg.Vault)
	}

	bindAddr := opts.BindAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", bindAddr, err)
	}

	url := "http://" + listener.Addr().String()
	info := discovery.Info{
		URL:       url,
		PID:       os.Getpid(),
		Version:   version.Version,
		StartedAt: time.Now().UTC(),
	}
	if err := discovery.Write(opts.StateDir, info); err != nil {
		_ = listener.Close()
		return fmt.Errorf("publish discovery: %w", err)
	}
	defer func() {
		if err := discovery.Remove(opts.StateDir); err != nil {
			log.Warn("removing discovery files", "error", err)
		}
	}()

	handler, err := app.NewHandlerWithConfig(log, cfg)
	if err != nil {
		_ = listener.Close()
		return err
	}

	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("daemon listening", "url", url, "pid", info.PID, "version", info.Version)
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down daemon", "url", url)
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}

// defaultStateDir returns the canonical user state directory
// (~/.aileron). The discovery file utilities create it on first write.
func defaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home dir: %w", err)
	}
	return filepath.Join(home, ".aileron"), nil
}

// selectVault returns an [app.Config] with `Vault` set to a persistent
// file vault unlocked from `vaultPath` when both conditions hold:
//
//  1. A vault file already exists at `vaultPath` (i.e. the user ran
//     `aileron launch` or `aileron binding setup` previously and
//     created one), and
//  2. `isTTY` is true — needed so the passphrase prompt has somewhere
//     to read from.
//
// Otherwise the returned config has `Vault: nil`, and
// [app.NewHandlerWithConfig] falls back to its in-memory dev vault per
// the comment in `internal/app/app.go`.
//
// Without this branch, the standalone server always took the in-memory
// path, which silently dropped every credential binding the moment the
// process exited — surfacing later as a `binding_required` error from
// the credential mediation path when the launch gateway tried to
// resolve the same binding against the persistent file vault.
//
// Extracted from `run` so unit tests can exercise the branching
// without bringing up an HTTP server. `prompter` is forwarded to
// [launch.EnsureVault]; nil means use the package default which reads
// from `/dev/tty`.
func selectVault(log *slog.Logger, vaultPath string, isTTY bool, prompter launch.PassphrasePrompter, w io.Writer) (app.Config, error) {
	state, err := vault.CheckState(vaultPath)
	if err != nil {
		// A vault file exists but is unreadable. This is a security
		// signal (tamper or corruption) — fail-loud rather than
		// silently dropping back to the in-memory dev vault, which
		// would mask the issue and hide whatever bindings the user
		// thought they had. Operators who want a clean memory-mode
		// start can `rm` the file themselves.
		return app.Config{}, fmt.Errorf("checking vault %q: %w", vaultPath, err)
	}
	if state == vault.StateMissing {
		log.Info("no persistent vault found; using in-memory dev vault", "path", vaultPath)
		return app.Config{}, nil
	}
	if !isTTY {
		log.Info("no controlling tty; using in-memory dev vault", "path", vaultPath)
		return app.Config{}, nil
	}
	log.Info("unlocking persistent vault", "path", vaultPath)
	v, vErr := launch.EnsureVault(vaultPath, prompter, w, 3)
	if vErr != nil {
		return app.Config{}, vErr
	}
	return app.Config{Vault: v}, nil
}
