package launch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/oauth"
	"github.com/ALRubinger/aileron/internal/vault"
)

// authspec host-acquire contract (#1268):
//
//   - Empty vault + declared HostAcquire + host-login enabled →
//     acquire on host, PUT the returned Secret through the daemon, and
//     render+mount the credential exactly as a vault-resident one
//     (RenderedAnyCredential == true so the in-container status line is
//     suppressed).
//   - Acquire error → non-fatal fallback: no PUT, legacy in-container
//     capture target (PresentAtRender == false).
//   - Acquire returns an empty Secret → fallback, no PUT.
//   - host-login disabled → acquirer never invoked even when declared.
//   - daemon PUT failure after a successful acquire → launch aborts and
//     cleanup runs.
//   - Required binding → required-but-empty error path is unchanged;
//     the acquirer is never consulted (the required check runs first).
//   - The acquired Secret round-trips the binding's own Render (a
//     Claude-shaped envelope), and a malformed acquired Secret surfaces
//     the Render error rather than silently seeding garbage.

// spyAcquirer is a HostAcquire hook that records its invocation count
// and returns a configurable Secret/error. It also captures the deps it
// was handed so tests can assert the launcher wired a Browser through.
type spyAcquirer struct {
	calls    int
	secret   vault.Secret
	err      error
	gotDeps  HostAcquireDeps
	gotBrows oauth.Opener
}

func (s *spyAcquirer) acquire(_ context.Context, deps HostAcquireDeps) (vault.Secret, error) {
	s.calls++
	s.gotDeps = deps
	s.gotBrows = deps.Browser
	return s.secret, s.err
}

// futureExpiresAtMS is a far-future expiry in MILLISECONDS, matching the real
// claude .credentials.json claudeAiOauth.expiresAt format (ms, not seconds).
const futureExpiresAtMS int64 = 1893456000000 // 2030-01-01Z

func TestPrepareAuthSpec_HostAcquireSeedsVaultAndRenders(t *testing.T) {
	envelope := claudeEnvelope("acquired-tok", futureExpiresAtMS)
	daemon := newFakeDaemon() // empty vault

	spy := &spyAcquirer{secret: vault.Secret{Value: envelope, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}}
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Mode:          0o600,
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture: func(b []byte) (vault.Secret, error) {
				return vault.Secret{Value: b, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}, nil
			},
			HostAcquire: spy.acquire,
		}},
	}

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if spy.calls != 1 {
		t.Fatalf("acquirer calls = %d, want 1", spy.calls)
	}
	if spy.gotBrows == nil {
		t.Errorf("acquirer deps Browser is nil; launcher should default to oauth.SystemBrowser{}")
	}
	if _, ok := spy.gotBrows.(oauth.SystemBrowser); !ok {
		t.Errorf("acquirer deps Browser = %T, want oauth.SystemBrowser", spy.gotBrows)
	}
	if spy.gotDeps.HTTPClient == nil {
		t.Errorf("acquirer deps HTTPClient is nil; launcher should supply a timed client")
	}

	if len(daemon.puts) != 1 {
		t.Fatalf("PUTs = %d, want exactly 1 (the host-acquired seed)", len(daemon.puts))
	}
	if daemon.puts[0].Name != "claude" {
		t.Errorf("PUT name = %q, want claude", daemon.puts[0].Name)
	}
	if string(daemon.puts[0].Secret.Value) != string(envelope) {
		t.Errorf("PUT value = %q, want %q", daemon.puts[0].Secret.Value, envelope)
	}

	if !prep.RenderedAnyCredential {
		t.Errorf("RenderedAnyCredential = false; a successful host-acquire must suppress the in-container status line")
	}

	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1", len(prep.Mounts))
	}
	rendered := filepath.Join(prep.Mounts[0].Source, ".credentials.json")
	got, err := os.ReadFile(rendered)
	if err != nil {
		t.Fatalf("read rendered file: %v", err)
	}
	if string(got) != string(envelope) {
		t.Errorf("rendered bytes = %q, want %q", got, envelope)
	}
}

func TestPrepareAuthSpec_HostAcquireErrorFallsBackToInContainer(t *testing.T) {
	daemon := newFakeDaemon() // empty vault
	spy := &spyAcquirer{err: errors.New("user cancelled consent")}
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			HostAcquire:   spy.acquire,
		}},
	}

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v (acquire failure must be non-fatal)", err)
	}
	defer prep.Cleanup()

	if spy.calls != 1 {
		t.Fatalf("acquirer calls = %d, want 1", spy.calls)
	}
	if len(daemon.puts) != 0 {
		t.Errorf("PUTs = %d, want 0 (acquire failed → no seed)", len(daemon.puts))
	}
	if prep.RenderedAnyCredential {
		t.Errorf("RenderedAnyCredential = true; an acquire failure must leave it false so the status line prints")
	}
	// Legacy capture target must exist (PresentAtRender == false) and no
	// file should be written for the binding.
	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (empty group dir for the capture target)", len(prep.Mounts))
	}
	entries, err := os.ReadDir(prep.Mounts[0].Source)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("transient dir not empty after fallback: %v", entries)
	}
}

func TestPrepareAuthSpec_HostAcquireEmptySecretFallsBack(t *testing.T) {
	daemon := newFakeDaemon()
	spy := &spyAcquirer{secret: vault.Secret{}} // empty Value
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			HostAcquire:   spy.acquire,
		}},
	}

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if spy.calls != 1 {
		t.Fatalf("acquirer calls = %d, want 1", spy.calls)
	}
	if len(daemon.puts) != 0 {
		t.Errorf("PUTs = %d, want 0 (empty Secret → fallback, no seed)", len(daemon.puts))
	}
	if prep.RenderedAnyCredential {
		t.Errorf("RenderedAnyCredential = true; an empty acquired Secret must fall back")
	}
}

func TestPrepareAuthSpec_HostLoginDisabledSkipsAcquirer(t *testing.T) {
	daemon := newFakeDaemon()
	spy := &spyAcquirer{secret: vault.Secret{Value: claudeEnvelope("tok", futureExpiresAtMS)}}
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			HostAcquire:   spy.acquire,
		}},
	}

	// hostLoginEnabled == false: the acquirer must never be invoked.
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, false)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if spy.calls != 0 {
		t.Errorf("acquirer calls = %d, want 0 (host-login disabled)", spy.calls)
	}
	if len(daemon.puts) != 0 {
		t.Errorf("PUTs = %d, want 0", len(daemon.puts))
	}
	if prep.RenderedAnyCredential {
		t.Errorf("RenderedAnyCredential = true; disabled host-login must take the in-container path")
	}
}

func TestPrepareAuthSpec_HostAcquirePutFailureAbortsLaunch(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.putErrors["claude"] = errors.New("vault locked")
	spy := &spyAcquirer{secret: vault.Secret{Value: claudeEnvelope("tok", futureExpiresAtMS)}}

	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			HostAcquire:   spy.acquire,
		}},
	}

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected error when daemon PUT of the host-acquired secret fails")
	}
	if spy.calls != 1 {
		t.Errorf("acquirer calls = %d, want 1", spy.calls)
	}
	// The abort must have run cleanup (transient dir removed) and left no
	// usable mount list. Cleanup is idempotent, so calling it again is
	// safe regardless.
	prep.Cleanup()
	if len(prep.Mounts) != 0 {
		t.Errorf("Mounts = %d on aborted launch, want 0", len(prep.Mounts))
	}
}

func TestPrepareAuthSpec_RequiredBindingIgnoresAcquirer(t *testing.T) {
	daemon := newFakeDaemon() // empty vault
	spy := &spyAcquirer{secret: vault.Secret{Value: claudeEnvelope("tok", futureExpiresAtMS)}}
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Required:      true, // required-but-empty must error before acquire
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			HostAcquire:   spy.acquire,
		}},
	}

	_, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected required-but-empty error")
	}
	if spy.calls != 0 {
		t.Errorf("acquirer calls = %d, want 0 (required check precedes acquire)", spy.calls)
	}
}

// TestPrepareAuthSpec_HostAcquireRoundTripsClaudeEnvelope proves the
// acquired Secret is validated by a real render-shaped function: the
// binding's own Render parses the Claude envelope, and the bytes the
// daemon PUT match the file written into the mount.
func TestPrepareAuthSpec_HostAcquireRoundTripsClaudeEnvelope(t *testing.T) {
	daemon := newFakeDaemon()
	envelope := claudeEnvelope("round-trip-tok", futureExpiresAtMS)
	spy := &spyAcquirer{secret: vault.Secret{Value: envelope}}
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Mode:          0o600,
			Required:      false,
			Render:        claudeShapedRender,
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			HostAcquire:   spy.acquire,
		}},
	}

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if len(daemon.puts) != 1 {
		t.Fatalf("PUTs = %d, want 1", len(daemon.puts))
	}
	// The PUT'd bytes must themselves pass the binding's Render.
	if _, err := claudeShapedRender(daemon.puts[0].Secret); err != nil {
		t.Errorf("PUT'd secret does not round-trip Render: %v", err)
	}
	// The file on disk must equal the rendered bytes.
	rendered := filepath.Join(prep.Mounts[0].Source, ".credentials.json")
	got, err := os.ReadFile(rendered)
	if err != nil {
		t.Fatalf("read rendered file: %v", err)
	}
	want, _ := claudeShapedRender(spy.secret)
	if string(got) != string(want) {
		t.Errorf("file bytes = %q, want rendered %q", got, want)
	}
}

// TestPrepareAuthSpec_HostAcquireMalformedSecretAborts proves a
// malformed acquired Secret surfaces the Render error (launch aborts)
// rather than silently seeding garbage. The daemon PUT happens first
// (AE6), so the abort path is the Render call.
func TestPrepareAuthSpec_HostAcquireMalformedSecretAborts(t *testing.T) {
	daemon := newFakeDaemon()
	spy := &spyAcquirer{secret: vault.Secret{Value: []byte("not json")}}
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Required:      false,
			Render:        claudeShapedRender,
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			HostAcquire:   spy.acquire,
		}},
	}

	_, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected Render error for a malformed acquired secret")
	}
	if spy.calls != 1 {
		t.Errorf("acquirer calls = %d, want 1", spy.calls)
	}
}

// claudeShapedRender mirrors the Claude credential envelope's Render
// contract: it requires a non-empty accessToken under claudeAiOauth and
// returns the canonical bytes. It lives in the test because the launch
// package cannot import the agents package (agents imports launch).
func claudeShapedRender(s vault.Secret) ([]byte, error) {
	type claudeOauth struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	var env claudeOauth
	if err := json.Unmarshal(s.Value, &env); err != nil {
		return nil, err
	}
	if env.ClaudeAiOauth.AccessToken == "" {
		return nil, errors.New("claude envelope missing accessToken")
	}
	return s.Value, nil
}
