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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/app"
	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/launch"
	approvaljsonl "github.com/ALRubinger/aileron/internal/approval/jsonl"
	"github.com/ALRubinger/aileron/internal/sessions/jsonl"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/version"
	"golang.org/x/term"
)

// lockInodeWatchInterval is how often the daemon stats <stateDir>/daemon.lock
// to detect inode changes. Picked at 5s as a tradeoff: long enough to cost
// nothing under steady-state, short enough that a duplicate-daemon situation
// resolves quickly. Variable rather than const so tests can shorten it.
var lockInodeWatchInterval = 5 * time.Second

func main() {
	var bind string
	var seedApprovalsPath string
	flag.StringVar(&bind, "bind", "", "TCP bind address (default 127.0.0.1:0 ephemeral; cloud sets e.g. 0.0.0.0:8080)")
	flag.StringVar(&seedApprovalsPath, "dev-seed-approvals", "", "Dev-only: path to a JSON file of pending action approvals to seed at startup. Refused unless the bind address is loopback.")
	flag.Parse()

	log := newLogger(os.Getenv("AILERON_LOG_LEVEL"))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := startDaemon(ctx, bind, seedApprovalsPath, log); err != nil {
		log.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}
}

// startDaemon resolves the production state directory + vault path and
// drives [run] under the supplied context. Extracted from main so
// tests can exercise the wiring with a cancellable ctx and a
// HOME-overridden state directory.
func startDaemon(ctx context.Context, bind, seedApprovalsPath string, log *slog.Logger) error {
	stateDir, err := defaultStateDir()
	if err != nil {
		return err
	}
	return run(ctx, log, options{
		BindAddr:          bind,
		StateDir:          stateDir,
		VaultPath:         launch.DefaultVaultPath(),
		IsTTY:             term.IsTerminal(int(os.Stdin.Fd())),
		Stderr:            os.Stderr,
		SeedApprovalsPath: seedApprovalsPath,
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
	// SeedApprovalsPath, when set, points at a JSON file of pending
	// action approvals to register on the queue at startup. Developer-
	// only — Playwright integration tests drive this so the approvals
	// page has something to list/approve/deny without standing up a
	// real agent. Refused unless BindAddr resolves to a loopback
	// address.
	SeedApprovalsPath string
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

	// Capture the inode of daemon.lock at the moment we acquired its
	// flock. The watcher goroutine below polls the same path and shuts
	// us down if the inode changes — i.e. an external `rm -rf
	// <stateDir>` (or any unlink-and-recreate) has orphaned our flock'd
	// inode and a fresh daemon now owns the path. Without this watch a
	// stranded daemon survives invisibly, breaking the singleton
	// invariant (#528 finding 3b).
	originalLockInode, err := discovery.LockFileInode(opts.StateDir)
	if err != nil {
		return fmt.Errorf("capture lock-file inode: %w", err)
	}

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

	// Persistent action-approval queue (#649). The JSONL store lives
	// alongside sessions.jsonl in stateDir. New() replays the file so
	// the daemon resumes pending and in-flight approvals on restart;
	// entries left in OutcomeRunning are reaped as failed with a
	// reconciliation message because the runtime can't safely
	// auto-rerun a non-idempotent action whose execution may have
	// crashed mid-flight.
	//
	// The reap pass writes a new "running → failed" record back to
	// the store before NewHandlerWithConfig loads the snapshot into
	// the queue, so the queue's in-memory state and the on-disk file
	// agree from the first instant. Without this ordering a fresh
	// /result poll could see the stale "running" state before the
	// queue had a chance to reconcile it.
	approvalStore, approvalRecords, err := approvaljsonl.New(filepath.Join(opts.StateDir, "approvals.jsonl"))
	if err != nil {
		return fmt.Errorf("open approvals store: %w", err)
	}
	defer func() {
		if err := approvalStore.Close(); err != nil {
			log.Warn("closing approvals store", "error", err)
		}
	}()
	for i, rec := range approvalRecords {
		if rec.Status != approval.OutcomeRunning {
			continue
		}
		reaped := rec
		reaped.Status = approval.OutcomeFailed
		reaped.ErrorMessage = "daemon crashed during execution; manual reconciliation required"
		if err := approvalStore.Append(reaped); err != nil {
			log.Warn("persist running-entry reap", "approval_id", rec.ID, "error", err)
		}
		approvalRecords[i] = reaped
		log.Warn("approval reaped after crash",
			"approval_id", rec.ID,
			"action", rec.ActionName,
			"reason", reaped.ErrorMessage)
	}
	cfg.ActionApprovalPersister = approvalStore
	cfg.LoadedActionApprovals = approvalRecords

	// --dev-seed-approvals: developer-only path to a JSON fixture of
	// pending action approvals. Wired in before the handler is built so
	// the seeded queue is the one cfg.ActionApprovals carries.
	// Loopback-only: refusing on a non-loopback bind keeps a careless
	// `aileron --bind 0.0.0.0 --dev-seed-approvals=...` from exposing a
	// pre-populated approval surface to the network.
	if opts.SeedApprovalsPath != "" {
		if !isLoopbackBind(bindAddr) {
			return fmt.Errorf("--dev-seed-approvals requires a loopback bind address (got %q)", bindAddr)
		}
		entries, err := approval.LoadSeedFile(opts.SeedApprovalsPath)
		if err != nil {
			return fmt.Errorf("load seed approvals: %w", err)
		}
		queue := approval.NewActionApprovalQueue(nil, nil)
		ids := approval.SeedQueue(queue, entries)
		cfg.ActionApprovals = queue
		log.Info("seeded action approvals", "count", len(ids), "ids", ids)
	}

	listener, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", bindAddr, err)
	}

	url := "http://" + listener.Addr().String()

	// The daemon serves the webapp itself (see internal/app/webapp_embed.go),
	// so its own bound URL is the right default for the approval review URL
	// surfaced in agent-facing messages. Operators running behind a reverse
	// proxy or on a non-loopback bind (`--bind 0.0.0.0:...`) must override
	// via `AILERON_WEBAPP_URL` so the message names the hostname users
	// actually reach. Defaulting here means the common single-user local
	// case works with no configuration.
	if cfg.WebappURL == "" && os.Getenv("AILERON_WEBAPP_URL") == "" {
		cfg.WebappURL = url
	}

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
	// skipDiscoveryRemove gates the deferred discovery.Remove. The
	// inode-watcher sets it on shutdown caused by lock-inode mismatch:
	// at that point daemon.json/pid belong to the daemon that took
	// over, and removing them would erase the surviving daemon's
	// advertisement.
	var skipDiscoveryRemove atomic.Bool
	defer func() {
		if skipDiscoveryRemove.Load() {
			log.Info("inode-mismatch shutdown: leaving discovery files for the daemon that owns them")
			return
		}
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

	// Lock-inode watcher. Closes inodeMismatch when the daemon.lock
	// path's inode changes from originalLockInode (or the file is
	// gone). See the originalLockInode capture above for the why.
	inodeMismatch := make(chan struct{})
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	go watchLockInode(watchCtx, log, opts.StateDir, originalLockInode, inodeMismatch)

	select {
	case <-ctx.Done():
		log.Info("shutting down daemon", "url", url)
	case <-inodeMismatch:
		log.Warn("daemon.lock inode changed; another daemon owns this path now — shutting down", "url", url)
		skipDiscoveryRemove.Store(true)
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

// watchLockInode polls daemon.lock at lockInodeWatchInterval and closes
// trigger when the inode no longer matches original. Stops on ctx
// cancellation. See run()'s originalLockInode capture for the why.
//
// "Inode changed" includes "file is gone": LockFileInode returns an
// error wrapping os.ErrNotExist when daemon.lock has been unlinked,
// which we treat the same as a mismatch. Other stat errors are logged
// at warn but otherwise ignored — they're transient (e.g. fs hiccup)
// and triggering shutdown on them would cause spurious daemon exits.
func watchLockInode(ctx context.Context, log *slog.Logger, stateDir string, original uint64, trigger chan<- struct{}) {
	ticker := time.NewTicker(lockInodeWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := discovery.LockFileInode(stateDir)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					log.Warn("daemon.lock disappeared", "stateDir", stateDir)
					close(trigger)
					return
				}
				log.Warn("statting daemon.lock", "error", err)
				continue
			}
			if current != original {
				log.Warn("daemon.lock inode changed",
					"original", original, "current", current, "stateDir", stateDir)
				close(trigger)
				return
			}
		}
	}
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

// isLoopbackBind reports whether bindAddr ("host:port" form) resolves
// to a loopback IP. Used by the --dev-seed-approvals guard so the
// developer-only seed surface can never accompany a public bind.
//
// Conservative on parse failure (returns false): unparseable bind
// addresses don't get the benefit of the doubt — better to reject the
// seed than silently expose it on an unintended interface.
func isLoopbackBind(bindAddr string) bool {
	host, _, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return false
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// selectVault decides how the daemon should access the local file vault
// at startup. The decision is governed by the four-state matrix from
// #492 items 5+6:
//
//	StateMissing             → hard error (run `aileron vault init` first)
//	StateReady, !isTTY        → start vault-locked; CLI/webapp drives /v1/vault/unlock
//	StateReady, isTTY         → unlock interactively at startup
//	parse error / corruption  → hard error (security signal — refuse to start)
//
// In every non-error case the returned [app.Config] sets
// `LocalVaultPath`, which is what wires the daemon's [vault.LockableVault]
// in [app.NewHandlerWithConfig] — without it, /v1/vault/unlock has no
// inner vault to swap into. Pre-#492 the auto-spawn path returned an
// empty Config, daemons silently fell through to an in-memory dev vault,
// and bindings disappeared on every restart while /v1/vault/unlock was
// effectively dead code in production.
//
// Cloud-shaped daemons (AILERON_DATABASE_URL set) bypass this entire
// branch — they store credentials in PostgreSQL via vault.PostgresVault,
// which [app.NewHandlerWithConfig] wires later. The local file vault
// is local-daemon-only, so a missing file there is benign.
//
// `prompter` is forwarded to [launch.EnsureVault] for the interactive
// path; nil means use the package default which reads from `/dev/tty`.
func selectVault(log *slog.Logger, vaultPath string, isTTY bool, prompter launch.PassphrasePrompter, w io.Writer) (app.Config, error) {
	if os.Getenv("AILERON_DATABASE_URL") != "" {
		log.Info("cloud-shaped daemon: skipping local vault (postgres-backed)")
		return app.Config{}, nil
	}
	state, err := vault.CheckState(vaultPath)
	if err != nil {
		// Tamper / corruption: fail-loud. Continuing with a memory vault
		// would mask the security signal and hide whatever bindings the
		// user thought they had.
		return app.Config{}, fmt.Errorf("checking vault %q: %w", vaultPath, err)
	}
	if state == vault.StateMissing {
		// Per #492 item 6a, the dev-vault fallback is gone. The CLI
		// state machine creates the file before spawning, and operators
		// running `aileron daemon start` directly are expected to
		// `aileron vault init` first. Refuse to start so we never
		// silently lose data.
		return app.Config{}, fmt.Errorf("no vault file at %q; run `aileron vault init` first", vaultPath)
	}
	if !isTTY {
		// Auto-spawn case: daemon starts vault-locked. The CLI POSTs
		// to /v1/vault/unlock with the passphrase right after spawn,
		// or the webapp drives the same endpoint via its passphrase
		// modal.
		log.Info("starting daemon with locked local vault", "path", vaultPath)
		return app.Config{LocalVaultPath: vaultPath}, nil
	}
	log.Info("unlocking persistent vault", "path", vaultPath)
	v, vErr := launch.EnsureVault(vaultPath, prompter, w, 3)
	if vErr != nil {
		return app.Config{}, vErr
	}
	// Set both Vault and LocalVaultPath: the LockableVault wraps the
	// pre-unlocked inner so the daemon's vault surface stays uniform
	// regardless of whether the unlock happened before or after spawn.
	return app.Config{Vault: v, LocalVaultPath: vaultPath}, nil
}
