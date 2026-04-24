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

	"github.com/ALRubinger/aileron/internal/connector"
	googlecalendar "github.com/ALRubinger/aileron/internal/connector/calendar/google"
	"github.com/ALRubinger/aileron/internal/connector/git/github"
	"github.com/ALRubinger/aileron/internal/connector/payments/stripe"
	"github.com/ALRubinger/aileron/internal/source"
	calendarsource "github.com/ALRubinger/aileron/internal/source/calendar"
	githubsource "github.com/ALRubinger/aileron/internal/source/github"
	gmailsource "github.com/ALRubinger/aileron/internal/source/gmail"
	slacksource "github.com/ALRubinger/aileron/internal/source/slack"
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

	dataDir := os.Getenv("AILERON_ENCLAVE_DATA_DIR")
	if dataDir == "" {
		if provider == "confidential-space" {
			dataDir = "/data/enclave"
		} else {
			home, _ := os.UserHomeDir()
			dataDir = home + "/.aileron/enclave"
		}
	}

	// Build execution connector registry (write actions).
	ctx := context.Background()
	registry := connector.NewRegistry()
	registry.Register(ctx, stripe.New())
	registry.Register(ctx, googlecalendar.New("", ""))
	registry.Register(ctx, github.New())

	// Build source connector registry (read-only tools).
	// Source connectors execute inside the enclave so credentials never
	// leave the TEE. Google connectors need OAuth config for token refresh.
	googleClientID := os.Getenv("GOOGLE_CONNECTOR_CLIENT_ID")
	googleClientSecret := os.Getenv("GOOGLE_CONNECTOR_CLIENT_SECRET")

	sourceReg := source.NewRegistry()
	sourceReg.Register(gmailsource.New(googleClientID, googleClientSecret))
	sourceReg.Register(calendarsource.New(googleClientID, googleClientSecret))
	sourceReg.Register(slacksource.New())
	sourceReg.Register(githubsource.New())

	srv, err := newEnclaveServer(log, registry, sourceReg, provider, dataDir)
	if err != nil {
		log.Error("failed to initialize enclave server", "error", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	srv.registerRoutes(mux)

	httpServer := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Background escrow cleanup.
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			srv.escrow.EvictExpired()
		}
	}()

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
