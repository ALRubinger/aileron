// Command aileron-enclave runs the enclave HTTP server inside a TEE.
//
// In production (Google Confidential Space), this binary runs inside a
// confidential VM where memory is encrypted by AMD SEV-SNP. The host
// control plane communicates with it over HTTPS.
//
// For local development, the same binary runs as a regular process with
// AILERON_TEE_PROVIDER=local, using a dev attestation token.
package main

import (
	"context"
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
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	port := os.Getenv("AILERON_ENCLAVE_PORT")
	if port == "" {
		port = "8443"
	}

	provider := os.Getenv("AILERON_TEE_PROVIDER")
	if provider == "" {
		provider = "local"
	}

	// Build connector registry.
	ctx := context.Background()
	registry := connector.NewRegistry()
	registry.Register(ctx, stripe.New())
	registry.Register(ctx, googlecalendar.New())
	registry.Register(ctx, github.New())

	srv := newEnclaveServer(log, registry, provider)
	mux := http.NewServeMux()
	srv.registerRoutes(mux)

	httpServer := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Graceful shutdown.
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Info("enclave server starting", "port", port, "provider", provider)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-done
	log.Info("shutting down enclave server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpServer.Shutdown(shutdownCtx)
}
