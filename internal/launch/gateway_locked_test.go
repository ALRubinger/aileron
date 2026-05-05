package launch

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/vault"
)

// End-to-end shape (#429): gateway started in webapp-prompt mode
// reports `locked` over HTTP, refuses vault-needing endpoints with
// 423, accepts a passphrase via /v1/vault/unlock, and transitions
// to `unlocked` for subsequent requests.

func TestStartGateway_LockedThenUnlocked_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "open sesame"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	g, err := StartGateway(context.Background(), gatewayConfig{LocalVaultPath: path})
	if err != nil {
		t.Fatalf("StartGateway: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = g.Close(ctx)
	})

	client := &http.Client{Timeout: 2 * time.Second}

	// Status before unlock: locked.
	got := getVaultStatus(t, client, g.URL)
	if !got.locked || got.state != "locked" {
		t.Errorf("pre-unlock status = %+v, want locked", got)
	}

	// Bindings setup is gated by vaultLocked; expect 423.
	resp, err := client.Post(g.URL+"/v1/bindings/setup", "application/json",
		strings.NewReader(`{"connector_fqn":"x","bindings":[]}`))
	if err != nil {
		t.Fatalf("POST /v1/bindings/setup: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusLocked {
		t.Errorf("pre-unlock /v1/bindings/setup status = %d, want 423", resp.StatusCode)
	}

	// Unlock via HTTP.
	resp, err = client.Post(g.URL+"/v1/vault/unlock", "application/json",
		strings.NewReader(`{"passphrase":"open sesame"}`))
	if err != nil {
		t.Fatalf("POST /v1/vault/unlock: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("unlock status = %d, want 200", resp.StatusCode)
	}

	// Status after unlock: unlocked.
	got = getVaultStatus(t, client, g.URL)
	if got.locked || got.state != "unlocked" {
		t.Errorf("post-unlock status = %+v, want unlocked", got)
	}

	// Wrong passphrase on a fresh request: 401.
	resp, err = client.Post(g.URL+"/v1/vault/unlock", "application/json",
		strings.NewReader(`{"passphrase":"wrong"}`))
	if err != nil {
		t.Fatalf("POST /v1/vault/unlock (wrong, post-unlock): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		// Already unlocked — handler returns 409, not 401.
		t.Errorf("post-unlock unlock status = %d, want 409 (already unlocked)", resp.StatusCode)
	}
}

func TestStartGateway_LockedRejectsWrongPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	if _, err := vault.Init(path, "open sesame"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	g, err := StartGateway(context.Background(), gatewayConfig{LocalVaultPath: path})
	if err != nil {
		t.Fatalf("StartGateway: %v", err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(g.URL+"/v1/vault/unlock", "application/json",
		strings.NewReader(`{"passphrase":"wrong"}`))
	if err != nil {
		t.Fatalf("POST /v1/vault/unlock: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}

	// Vault stays locked after wrong passphrase.
	got := getVaultStatus(t, client, g.URL)
	if !got.locked {
		t.Errorf("vault unlocked after wrong passphrase: %+v", got)
	}
}

type vaultStatusBody struct {
	locked bool
	state  string
}

func getVaultStatus(t *testing.T, c *http.Client, base string) vaultStatusBody {
	t.Helper()
	resp, err := c.Get(base + "/v1/vault/status")
	if err != nil {
		t.Fatalf("GET /v1/vault/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Locked bool   `json:"locked"`
		State  string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return vaultStatusBody{locked: body.Locked, state: body.State}
}
