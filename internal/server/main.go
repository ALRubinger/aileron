// Package main is the entry point for the Aileron control plane server.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/app"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/vault"
	"golang.org/x/term"
)

func main() {
	level := slog.LevelInfo
	if l := os.Getenv("AILERON_LOG_LEVEL"); l != "" {
		switch l {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	if err := run(log); err != nil {
		log.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	addr := os.Getenv("AILERON_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	cfg, err := selectVault(log, launch.DefaultVaultPath(), term.IsTerminal(int(os.Stdin.Fd())), nil, os.Stderr)
	if err != nil {
		return err
	}

	handler, err := app.NewHandlerWithConfig(log, cfg)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("listen error", "error", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
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
// process exited — surfacing later as "action declares no [[bindings]]
// entry for this connector" when the launch gateway tried to resolve
// the same binding against the persistent file vault.
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
