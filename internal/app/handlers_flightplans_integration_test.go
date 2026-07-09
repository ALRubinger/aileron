//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/vault"
)

// TestLaunchFlightPlan_FullStackReachesRuntime drives the launch endpoint end to
// end through the full NewHandlerWithConfig stack over real HTTP. It proves the
// pieces only the full wiring exercises: the route is registered on the daemon
// router, the FlightPlanStoreDir config resolves a store the handler reads, and
// the endpoint reaches internal/flightplan/runtime.Run with the image seams left
// nil (the documented deterministic-only scope). The worked-example plan pins an
// environment image via `environment.tools`, so the runtime's own nil-guard
// ("no image runner is configured") is the deterministic, environment-independent
// signal that the launch reached runtime.Run with the in-scope seam set. That is
// the exact CLI parity: a CLI launch of an image-pinning plan with no
// ImageRunner wired fails the same closed way.
func TestLaunchFlightPlan_FullStackReachesRuntime(t *testing.T) {
	dir := t.TempDir()
	s := store.New(dir)
	fv := freezeExampleForIntegration(t)
	if err := s.WriteFrozen("weekly-metrics-digest", fv); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler, err := NewHandlerWithConfig(log, Config{
		Vault:              vault.NewMemVault(),
		FlightPlanStoreDir: dir,
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/flightplans/weekly-metrics-digest/launch",
		"application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST launch: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// An image-pinning plan with no ImageRunner wired fails closed inside
	// runtime.Run and the handler maps it to a runtime-boundary FailureEnvelope
	// (500). The status proves the launch reached the runtime; a 404 would mean
	// the route or store-dir wiring was wrong.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, body)
	}
	var env struct {
		Error struct {
			Class   string `json:"class"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, body)
	}
	if env.Error.Class == "" {
		t.Errorf("body is not an ADR-0010 FailureEnvelope: %s", body)
	}
	// The runtime's image nil-guard is the reached-runtime proof.
	if !bytes.Contains([]byte(env.Error.Message), []byte("image runner")) {
		t.Errorf("error message = %q, want the runtime image nil-guard", env.Error.Message)
	}
}

// TestLaunchFlightPlan_FullStackNoFrozen404 proves the no-frozen-version 404
// over the real HTTP stack: an installed-but-not-frozen (absent) plan name has
// no frozen version, so the endpoint returns 404 rather than reaching the
// runtime.
func TestLaunchFlightPlan_FullStackNoFrozen404(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler, err := NewHandlerWithConfig(log, Config{
		Vault:              vault.NewMemVault(),
		FlightPlanStoreDir: dir,
	})
	if err != nil {
		t.Fatalf("NewHandlerWithConfig: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/flightplans/ghost/launch",
		"application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST launch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, body)
	}
}

// freezeExampleForIntegration freezes the committed worked example (which pins
// an environment image via environment.tools) so the integration test can drive
// the image-pin nil-guard path. It composes a fake per-arch digest set so freeze
// does not need a container daemon.
func freezeExampleForIntegration(t *testing.T) store.FrozenVersion {
	t.Helper()
	// Locate the committed worked example relative to the repo root (walk up
	// from the package dir until docs/schema is found).
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	var examplePath string
	for {
		cand := filepath.Join(dir, "docs", "schema", "flight-plan-manifest.example.skill.md")
		if _, statErr := os.Stat(cand); statErr == nil {
			examplePath = cand
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate docs/schema worked example from %s", dir)
		}
		dir = parent
	}
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read worked example: %v", err)
	}

	res, err := freeze.Run(context.Background(), raw, freeze.Options{
		Version:        "1.0.0",
		SigningKeyPath: writeSigningKey(t),
		Resolver: freeze.DigestResolverFunc(func(context.Context, string) (string, error) {
			return "sha256:" + strings.Repeat("b", 64), nil
		}),
		Composer: freeze.FeatureComposerFunc(func(context.Context, string, []string) ([]freeze.PlatformDigest, error) {
			return []freeze.PlatformDigest{
				{OS: "linux", Arch: "amd64", Digest: "sha256:" + strings.Repeat("a", 64)},
				{OS: "linux", Arch: "arm64", Digest: "sha256:" + strings.Repeat("a", 64)},
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("freeze.Run worked example: %v", err)
	}
	return store.FrozenVersion{
		ID:        "test",
		SkillMD:   res.FrozenManifest,
		Lockfile:  res.Lockfile,
		Signature: res.Signature,
		PublicKey: res.PublicKey,
	}
}
