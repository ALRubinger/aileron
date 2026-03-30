// Package main is the entry point for the Aileron control plane server.
//
// The server wires together the core components — policy engine, approval
// orchestrator, connector registry, vault, notifiers, and audit store —
// and exposes the control plane API over HTTP.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/core/connector"
	googlecalendar "github.com/ALRubinger/aileron/core/connector/calendar/google"
	"github.com/ALRubinger/aileron/core/connector/git/github"
	"github.com/ALRubinger/aileron/core/connector/payments/stripe"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx := context.Background()
	cfg := configFromEnv()

	// --- Connector registry ---
	registry := connector.NewRegistry()
	registry.Register(ctx, stripe.New())
	registry.Register(ctx, googlecalendar.New())
	registry.Register(ctx, github.New())

	// --- HTTP server ---
	mux := http.NewServeMux()
	registerRoutes(mux, log, registry)

	// Middleware chain: CORS → request ID → logging → routes.
	var handler http.Handler = mux
	handler = loggingMiddleware(log, handler)
	handler = requestIDMiddleware(handler)
	handler = corsMiddleware(handler)

	srv := &http.Server{
		Addr:         cfg.addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("server listening", "addr", cfg.addr)
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

func registerRoutes(mux *http.ServeMux, log *slog.Logger, _ *connector.Registry) {
	// Health check — no auth required.
	mux.HandleFunc("GET /v1/health", handleHealth(log))

	// TODO: register intent, approval, policy, connector, execution,
	// funding source, credential, trace, and analytics handlers.
}

func handleHealth(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.InfoContext(r.Context(), "health check")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","service":"aileron","version":"0.1.0"}`)
	}
}

// config holds runtime configuration sourced from environment variables.
type config struct {
	addr        string
	databaseURL string
}

func configFromEnv() config {
	addr := os.Getenv("AILERON_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	return config{
		addr:        addr,
		databaseURL: os.Getenv("DATABASE_URL"),
	}
}
