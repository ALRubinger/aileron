package launch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ALRubinger/aileron/internal/vault"
)

// authspec_runtime.go contract (U3):
//
//   - Empty AuthSpec is a no-op: zero envAdditions, zero mounts,
//     capture is a no-op, cleanup is a no-op.
//   - File binding round-trip: vault entry → host file at binding
//     ContainerPath inside a transient dir bind-mounted at the
//     binding's container parent dir.
//   - StaticFile entries always land regardless of vault state.
//   - 404 vault on Required=false binding: bind-mount source is
//     created but empty for the binding's file; no error.
//   - 404 vault on Required=true binding: returns error and the
//     prepareAuthSpec call fails before launchSandbox would have
//     started.
//   - Capture after a clean exit reads the (possibly rotated) host-
//     side file and PUTs it back to the daemon.
//   - Capture surfaces vault PUT failures to stderr (R13 recovery
//     instructions) but stays non-fatal.
//   - PreLaunchRefresh runs before Render; rotated bundles are
//     persisted via the daemon's PutAgentCredentials before Render
//     uses them.
//   - Transient host dir is removed by Cleanup.

// fakeDaemon implements the authSpecDaemon interface for tests. The
// fake records calls so assertions can pin "did we PUT?" / "did we
// GET the right name?" — the real daemon HTTP server is exercised
// in daemon_client_agent_credentials_test.go.
//
// The store and per-name error/sequence maps are keyed by the FULL
// destination (name, purpose), not by name alone. Keying by name only
// would let a purpose misroute go GREEN: two bindings on the same agent
// (oauth + apikey) would collapse onto one store key, so a PUT that
// targeted the wrong purpose would still appear to land. With the
// composite key a misroute reads/writes a different slot and the
// assertion fails, which is the whole point of the distinct-destination
// test. The seed/deleteEntry/getErrors/putErrors helpers default the
// purpose to oauth so existing single-purpose tests keep compiling.
type fakeDaemon struct {
	mu        sync.Mutex
	store     map[daemonKey]vault.Secret
	getErrors map[daemonKey]error
	putErrors map[daemonKey]error
	// getSeq, when populated for a destination, supplies a per-call
	// error sequence consumed in order. A nil entry means "no error, use
	// the store" for that call. This lets a scenario make the render GET
	// succeed and the capture GET cancel/fail without affecting the
	// other. getSeq takes precedence over getErrors for that
	// destination.
	getSeq map[daemonKey][]error
	puts   []putRecord
	gets   []getRecord
}

// daemonKey is the composite (name, purpose) destination key the fake
// daemon routes by, mirroring the daemon's real per-(name, purpose)
// vault slots.
type daemonKey struct {
	Name    string
	Purpose string
}

type putRecord struct {
	Name    string
	Purpose string
	Secret  vault.Secret
}

type getRecord struct {
	Name    string
	Purpose string
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{
		store:     map[daemonKey]vault.Secret{},
		getErrors: map[daemonKey]error{},
		putErrors: map[daemonKey]error{},
		getSeq:    map[daemonKey][]error{},
	}
}

// seedPurpose seeds the vault slot at the full (name, purpose)
// destination. Use this when a test exercises a non-oauth purpose.
func (f *fakeDaemon) seedPurpose(name, purpose string, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[daemonKey{Name: name, Purpose: purpose}] = vault.Secret{Value: value, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}
}

// seed is the thin oauth-defaulting shim over seedPurpose so the
// existing single-purpose tests keep compiling unchanged.
func (f *fakeDaemon) seed(name string, value []byte) {
	f.seedPurpose(name, defaultAgentCredentialPurpose, value)
}

// deleteEntryPurpose simulates an operator running `aileron vault
// delete` mid-session against a specific (name, purpose) destination.
func (f *fakeDaemon) deleteEntryPurpose(name, purpose string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, daemonKey{Name: name, Purpose: purpose})
}

// deleteEntry is the oauth-defaulting shim over deleteEntryPurpose: the
// entry vanishes from the store so a subsequent GET returns
// ErrAgentCredentialsNotFound.
func (f *fakeDaemon) deleteEntry(name string) {
	f.deleteEntryPurpose(name, defaultAgentCredentialPurpose)
}

// setGetError installs a GET error for the (name, purpose) destination.
func (f *fakeDaemon) setGetError(name, purpose string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErrors[daemonKey{Name: name, Purpose: purpose}] = err
}

// setGetErrorOAuth installs a GET error for the oauth slot — the
// oauth-defaulting shim existing single-purpose tests use.
func (f *fakeDaemon) setGetErrorOAuth(name string, err error) {
	f.setGetError(name, defaultAgentCredentialPurpose, err)
}

// setPutError installs a PUT error for the (name, purpose) destination.
func (f *fakeDaemon) setPutError(name, purpose string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putErrors[daemonKey{Name: name, Purpose: purpose}] = err
}

// setPutErrorOAuth installs a PUT error for the oauth slot.
func (f *fakeDaemon) setPutErrorOAuth(name string, err error) {
	f.setPutError(name, defaultAgentCredentialPurpose, err)
}

// setGetSeqOAuth installs a per-call GET error sequence for the oauth
// slot, consumed in order.
func (f *fakeDaemon) setGetSeqOAuth(name string, seq []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getSeq[daemonKey{Name: name, Purpose: defaultAgentCredentialPurpose}] = seq
}

func (f *fakeDaemon) GetAgentCredentials(_ context.Context, name, purpose string) (vault.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := daemonKey{Name: name, Purpose: purpose}
	f.gets = append(f.gets, getRecord{Name: name, Purpose: purpose})
	if seq, ok := f.getSeq[key]; ok && len(seq) > 0 {
		err := seq[0]
		f.getSeq[key] = seq[1:]
		if err != nil {
			return vault.Secret{}, err
		}
		s, ok := f.store[key]
		if !ok {
			return vault.Secret{}, ErrAgentCredentialsNotFound
		}
		return s, nil
	}
	if err, ok := f.getErrors[key]; ok {
		return vault.Secret{}, err
	}
	s, ok := f.store[key]
	if !ok {
		return vault.Secret{}, ErrAgentCredentialsNotFound
	}
	return s, nil
}

func (f *fakeDaemon) PutAgentCredentials(_ context.Context, name, purpose string, secret vault.Secret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := daemonKey{Name: name, Purpose: purpose}
	if err, ok := f.putErrors[key]; ok {
		return err
	}
	f.puts = append(f.puts, putRecord{Name: name, Purpose: purpose, Secret: secret})
	f.store[key] = secret
	return nil
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestPrepareAuthSpec_EmptySpecIsNoOp(t *testing.T) {
	prep, err := prepareAuthSpec(context.Background(), "claude", AuthSpec{},
		newFakeDaemon(), newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	if len(prep.EnvAdditions) != 0 {
		t.Errorf("EnvAdditions = %v, want empty", prep.EnvAdditions)
	}
	if len(prep.Mounts) != 0 {
		t.Errorf("Mounts = %v, want empty", prep.Mounts)
	}
	if prep.HasBindings {
		t.Errorf("HasBindings = true, want false for empty spec")
	}
	// Both must be safe to call on the empty path.
	prep.CaptureFn(context.Background())
	prep.Cleanup()
}

func TestPrepareAuthSpec_FileBindingRendersFromVault(t *testing.T) {
	envelope := []byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"rt"}}`)
	daemon := newFakeDaemon()
	daemon.seed("claude", envelope)

	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Mode:          0o600,
			Required:      true,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture: func(b []byte) (vault.Secret, error) {
				return vault.Secret{Value: b, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}, nil
			},
		}},
	}

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1", len(prep.Mounts))
	}
	mount := prep.Mounts[0]
	if mount.Target != "/home/agent/.claude" {
		t.Errorf("mount target = %q, want /home/agent/.claude", mount.Target)
	}
	if mount.ReadOnly {
		t.Errorf("mount is read-only; Claude rotates tokens via rename so the dir must be writable")
	}

	rendered := filepath.Join(mount.Source, ".credentials.json")
	got, err := os.ReadFile(rendered)
	if err != nil {
		t.Fatalf("read rendered file: %v", err)
	}
	if !bytes.Equal(got, envelope) {
		t.Errorf("rendered bytes = %q, want %q", got, envelope)
	}

	info, err := os.Stat(rendered)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}

	// Transient root must be 0700 so OAuth tokens aren't readable
	// through a shared host's default umask.
	rootInfo, err := os.Stat(filepath.Dir(mount.Source))
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Errorf("transient root mode = %v, want 0700", rootInfo.Mode().Perm())
	}

	// The binding's purpose (oauth, from agents/claude/oauth) must be
	// threaded through to the daemon GET rather than discarded. This is
	// the foundation SUB2/SUB3 extend to a second purpose.
	if len(daemon.gets) == 0 {
		t.Fatal("expected at least one GET")
	}
	if daemon.gets[0].Name != "claude" || daemon.gets[0].Purpose != "oauth" {
		t.Errorf("GET = (%q, %q), want (claude, oauth)",
			daemon.gets[0].Name, daemon.gets[0].Purpose)
	}
}

func TestPrepareAuthSpec_StaticFileLandsRegardlessOfVault(t *testing.T) {
	daemon := newFakeDaemon() // empty vault
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Mode:          0o600,
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
		StaticFiles: []StaticFile{{
			ContainerPath: "/home/agent/.claude.json",
			Mode:          0o644,
			Content:       []byte(`{"hasCompletedOnboarding":true,"installMethod":"native"}`),
		}},
	}

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	// /home/agent/.claude.json gets a file-level mount (not a dir
	// mount) so the workspace at /home/agent/ isn't masked.
	var staticMount *struct{ Source, Target string }
	for _, m := range prep.Mounts {
		if m.Target == "/home/agent/.claude.json" {
			staticMount = &struct{ Source, Target string }{m.Source, m.Target}
		}
	}
	if staticMount == nil {
		t.Fatalf("no file mount for /home/agent/.claude.json; got %+v", prep.Mounts)
	}
	got, err := os.ReadFile(staticMount.Source)
	if err != nil {
		t.Fatalf("read static: %v", err)
	}
	if string(got) != `{"hasCompletedOnboarding":true,"installMethod":"native"}` {
		t.Errorf("static content = %q", got)
	}
}

func TestPrepareAuthSpec_RequiredVaultMissingFailsLaunch(t *testing.T) {
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Mode:          0o600,
			Required:      true,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "claude", spec, newFakeDaemon(),
		newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected error for required+empty vault")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %v, want mention of required", err)
	}
}

func TestPrepareAuthSpec_EmptyVaultOptionalBindingDoesNotRender(t *testing.T) {
	// On an empty vault with Required=false, no file is written;
	// the bind-mount source still exists (as the transient dir) so
	// the in-container agent can write its initial login output
	// into the mounted location, and Capture picks it up.
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, newFakeDaemon(),
		newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (empty group dir mounted for capture target)", len(prep.Mounts))
	}
	// The transient dir should be empty: no credentials file yet.
	entries, err := os.ReadDir(prep.Mounts[0].Source)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("transient dir not empty: %v", entries)
	}
}

func TestPrepareAuthSpec_CaptureOnCleanExitPersistsRotatedFile(t *testing.T) {
	// Simulate Claude rotating its OAuth bytes in-container.
	envelope := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"r"}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"r"}}`)

	daemon := newFakeDaemon()
	daemon.seed("claude", envelope)
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Mode:          0o600,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture: func(b []byte) (vault.Secret, error) {
				return vault.Secret{Value: b, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}, nil
			},
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	// Simulate in-container rotation.
	hostPath := filepath.Join(prep.Mounts[0].Source, ".credentials.json")
	if err := os.WriteFile(hostPath, rotated, 0o600); err != nil {
		t.Fatalf("simulate rotation: %v", err)
	}

	prep.CaptureFn(context.Background())

	if len(daemon.puts) != 1 {
		t.Fatalf("daemon.puts = %d, want 1", len(daemon.puts))
	}
	if daemon.puts[0].Name != "claude" {
		t.Errorf("PUT name = %q, want claude", daemon.puts[0].Name)
	}
	if !bytes.Equal(daemon.puts[0].Secret.Value, rotated) {
		t.Errorf("PUT value = %q, want %q", daemon.puts[0].Secret.Value, rotated)
	}
}

func TestPrepareAuthSpec_CaptureSchemaFailureSkipsPut(t *testing.T) {
	// A Capture func that returns an error simulates schema-drift
	// (partial write, agent version mismatch). The launcher must
	// log and skip the PUT — overwriting the vault with garbage is
	// strictly worse than retaining the prior entry.
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte(`{"claudeAiOauth":{"accessToken":"old"}}`))

	stderr := &bytes.Buffer{}
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{}, errors.New("schema drift") },
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), stderr, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	// Write garbage into the mount so capture has something to read.
	hostPath := filepath.Join(prep.Mounts[0].Source, ".credentials.json")
	if err := os.WriteFile(hostPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	beforePuts := len(daemon.puts)
	prep.CaptureFn(context.Background())

	if len(daemon.puts) != beforePuts {
		t.Errorf("schema failure should skip PUT; got %d new puts", len(daemon.puts)-beforePuts)
	}
	if !strings.Contains(stderr.String(), "capture for claude failed") {
		t.Errorf("expected stderr warning; got %q", stderr.String())
	}
}

// Regression (#1384, sub-req A): a CaptureValidate hook that fails (the
// captured credential did not authenticate against the vendor) must cause
// the launcher to SKIP the PUT, retaining the prior vault entry rather
// than overwriting it with an unusable token. The hook runs after Capture
// and before the Fresher/presence branch.
func TestPrepareAuthSpec_CaptureValidateFailureSkipsPut(t *testing.T) {
	prior := []byte(`{"claudeAiOauth":{"accessToken":"prior"}}`)
	captured := []byte(`{"claudeAiOauth":{"accessToken":"bad-new"}}`)

	daemon := newFakeDaemon()
	daemon.seed("claude", prior)

	stderr := &bytes.Buffer{}
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture: func(b []byte) (vault.Secret, error) {
				return vault.Secret{Value: b, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}, nil
			},
			CaptureValidate: func(_ context.Context, _ *http.Client, _ vault.Secret) error {
				return errors.New("token failed live validation")
			},
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), stderr, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	hostPath := filepath.Join(prep.Mounts[0].Source, ".credentials.json")
	if err := os.WriteFile(hostPath, captured, 0o600); err != nil {
		t.Fatal(err)
	}

	beforePuts := len(daemon.puts)
	prep.CaptureFn(context.Background())

	if len(daemon.puts) != beforePuts {
		t.Errorf("CaptureValidate failure should skip PUT; got %d new puts", len(daemon.puts)-beforePuts)
	}
	// Prior vault entry must be retained unchanged.
	got, getErr := daemon.GetAgentCredentials(context.Background(), "claude", "oauth")
	if getErr != nil {
		t.Fatalf("GetAgentCredentials after skipped PUT: %v", getErr)
	}
	if !bytes.Equal(got.Value, prior) {
		t.Errorf("prior entry = %q, want it retained as %q", got.Value, prior)
	}
	if !strings.Contains(stderr.String(), "capture for claude failed") {
		t.Errorf("expected stderr warning; got %q", stderr.String())
	}
}

// Regression (#1384, sub-req A): a CaptureValidate hook that PASSES must
// let the PUT proceed — a valid captured token is persisted as today.
func TestPrepareAuthSpec_CaptureValidateSuccessPersists(t *testing.T) {
	prior := []byte(`{"claudeAiOauth":{"accessToken":"prior","expiresAt":1}}`)
	captured := []byte(`{"claudeAiOauth":{"accessToken":"fresh","expiresAt":2}}`)

	daemon := newFakeDaemon()
	daemon.seed("claude", prior)

	validated := false
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture: func(b []byte) (vault.Secret, error) {
				return vault.Secret{Value: b, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}, nil
			},
			CaptureValidate: func(_ context.Context, _ *http.Client, _ vault.Secret) error {
				validated = true
				return nil
			},
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	hostPath := filepath.Join(prep.Mounts[0].Source, ".credentials.json")
	if err := os.WriteFile(hostPath, captured, 0o600); err != nil {
		t.Fatal(err)
	}

	prep.CaptureFn(context.Background())

	if !validated {
		t.Error("CaptureValidate hook was never invoked")
	}
	if len(daemon.puts) != 1 {
		t.Fatalf("daemon.puts = %d, want 1 (valid token persists)", len(daemon.puts))
	}
	if !bytes.Equal(daemon.puts[0].Secret.Value, captured) {
		t.Errorf("PUT value = %q, want %q", daemon.puts[0].Secret.Value, captured)
	}
}

func TestPrepareAuthSpec_PreLaunchRefreshPersistsBeforeRender(t *testing.T) {
	// PreLaunchRefresh receives the original secret and the deps;
	// when it returns a rotated secret it must already have been
	// PUT through deps.PutAgentCredentials before Render uses it
	// (AE6 invariant: rotated bundle is in vault before container
	// start).
	daemon := newFakeDaemon()
	original := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"old","refresh_token":"r"},"last_refresh":"2026-06-01T00:00:00Z"}`)
	rotated := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"new","refresh_token":"r2"},"last_refresh":"2026-06-10T00:00:00Z"}`)
	daemon.seed("codex", original)

	persistedDuringRefresh := false
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/codex/oauth",
			ContainerPath: "/home/agent/.codex/auth.json",
			Mode:          0o600,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			PreLaunchRefresh: func(s vault.Secret, deps RefreshDeps) (vault.Secret, error) {
				newSecret := vault.Secret{Value: rotated, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}
				if err := deps.PutAgentCredentials(newSecret); err != nil {
					return vault.Secret{}, err
				}
				persistedDuringRefresh = true
				return newSecret, nil
			},
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "codex", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if !persistedDuringRefresh {
		t.Fatal("PreLaunchRefresh should have persisted via deps.PutAgentCredentials")
	}
	// The rendered file should reflect the rotated bundle.
	hostPath := filepath.Join(prep.Mounts[0].Source, "auth.json")
	got, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("read rendered: %v", err)
	}
	if !bytes.Equal(got, rotated) {
		t.Errorf("rendered = %q, want rotated %q", got, rotated)
	}
}

func TestPrepareAuthSpec_PreLaunchRefreshErrorAbortsLaunch(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("codex", []byte(`{}`))
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/codex/oauth",
			ContainerPath: "/home/agent/.codex/auth.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			PreLaunchRefresh: func(s vault.Secret, deps RefreshDeps) (vault.Secret, error) {
				return vault.Secret{}, errors.New("refresh failed")
			},
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "codex", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected error when PreLaunchRefresh fails")
	}
}

func TestPrepareAuthSpec_CleanupRemovesTransientDir(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte("envelope"))
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	dir := filepath.Dir(prep.Mounts[0].Source)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("transient dir missing before cleanup: %v", err)
	}
	prep.Cleanup()
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("transient dir still exists after cleanup: %v", err)
	}
	// Calling cleanup twice is a no-op.
	prep.Cleanup()
}

func TestPrepareAuthSpec_EnvBindingMerges(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("example", []byte("sekret"))
	spec := AuthSpec{
		EnvBindings: []EnvBinding{{
			VaultPath: "agents/example/oauth",
			Required:  true,
			Render: func(s vault.Secret) (map[string]string, error) {
				return map[string]string{"EXAMPLE_API_KEY": string(s.Value)}, nil
			},
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "example", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()
	if prep.EnvAdditions["EXAMPLE_API_KEY"] != "sekret" {
		t.Errorf("EXAMPLE_API_KEY = %q, want sekret", prep.EnvAdditions["EXAMPLE_API_KEY"])
	}
}

func TestPrepareAuthSpec_CapturePutFailureSurfacesWarning(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte(`{"x":1}`))
	daemon.setPutErrorOAuth("claude", errors.New("daemon offline"))

	stderr := &bytes.Buffer{}
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), stderr, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	// Modify file so capture reads non-empty bytes.
	hostPath := filepath.Join(prep.Mounts[0].Source, ".credentials.json")
	if err := os.WriteFile(hostPath, []byte(`{"x":2}`), 0o600); err != nil {
		t.Fatal(err)
	}

	prep.CaptureFn(context.Background())

	if !strings.Contains(stderr.String(), "capture for claude failed") {
		t.Errorf("expected stderr capture warning; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "rerun the launch") {
		t.Errorf("expected stderr to include recovery hint; got %q", stderr.String())
	}
}

func TestPrepareAuthSpec_EnvBindingRequiredMissingErrors(t *testing.T) {
	spec := AuthSpec{
		EnvBindings: []EnvBinding{{
			VaultPath: "agents/example/oauth",
			Required:  true,
			Render: func(s vault.Secret) (map[string]string, error) {
				return map[string]string{"X": string(s.Value)}, nil
			},
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "example", spec, newFakeDaemon(),
		newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected error on required env binding with empty vault")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %v, want mention of required", err)
	}
}

func TestPrepareAuthSpec_EnvBindingOptionalMissingContinues(t *testing.T) {
	spec := AuthSpec{
		EnvBindings: []EnvBinding{{
			VaultPath: "agents/example/oauth",
			Required:  false,
			Render: func(s vault.Secret) (map[string]string, error) {
				return map[string]string{"X": string(s.Value)}, nil
			},
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "example", spec, newFakeDaemon(),
		newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()
	if len(prep.EnvAdditions) != 0 {
		t.Errorf("EnvAdditions = %v, want empty (no vault entry, optional)", prep.EnvAdditions)
	}
}

func TestPrepareAuthSpec_EnvBindingGetErrorPropagates(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.setGetErrorOAuth("example", errors.New("network down"))
	spec := AuthSpec{
		EnvBindings: []EnvBinding{{
			VaultPath: "agents/example/oauth",
			Required:  false,
			Render:    func(s vault.Secret) (map[string]string, error) { return nil, nil },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "example", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected error when GET fails with non-NotFound")
	}
	if !strings.Contains(err.Error(), "network down") {
		t.Errorf("error = %v, want network down", err)
	}
}

func TestPrepareAuthSpec_EnvBindingRenderErrorPropagates(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("example", []byte("v"))
	spec := AuthSpec{
		EnvBindings: []EnvBinding{{
			VaultPath: "agents/example/oauth",
			Required:  true,
			Render:    func(s vault.Secret) (map[string]string, error) { return nil, errors.New("bad shape") },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "example", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected error when Render fails")
	}
}

func TestPrepareAuthSpec_FileBindingRenderErrorPropagates(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte("v"))
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return nil, errors.New("malformed envelope") },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected error when Render fails")
	}
}

func TestPrepareAuthSpec_FileBindingGetErrorPropagates(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.setGetErrorOAuth("claude", errors.New("daemon offline"))
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected error when GET fails with non-NotFound")
	}
}

func TestPrepareAuthSpec_ValidationErrorReturnsBeforeFS(t *testing.T) {
	// A binding with a nil Render must fail validation BEFORE any
	// transient dir is created — keeps invalid specs from leaving
	// temp directories on disk.
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			// Render is nil — validation should reject.
			Capture: func(b []byte) (vault.Secret, error) { return vault.Secret{}, nil },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "claude", spec, newFakeDaemon(),
		newTestLogger(), nil, nil, nil, true)
	if err == nil {
		t.Fatal("expected validation error for nil Render")
	}
	if !errors.Is(err, ErrAuthSpecRenderNil) {
		t.Errorf("err = %v, want ErrAuthSpecRenderNil", err)
	}
}

func TestPrepareAuthSpec_EmptyVaultFirstLaunchProducesWritableParentMount(t *testing.T) {
	// Regression test for the Codex first-launch bootstrap bug: an
	// empty vault with Required=false must still produce a writable
	// parent-dir mount so the in-container login can write the
	// credential file and Capture can read it on clean exit. Before
	// the fix, MountAsFile=true skipped the parent-dir mount AND the
	// emptyVault continue skipped the per-file mount, leaving the
	// in-container login writing into the container overlay FS.
	daemon := newFakeDaemon() // empty vault
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/codex/oauth",
			ContainerPath: "/home/agent/.codex/auth.json",
			Mode:          0o600,
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "codex", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (parent dir mount for first-launch bootstrap)", len(prep.Mounts))
	}
	mount := prep.Mounts[0]
	if mount.Target != "/home/agent/.codex" {
		t.Errorf("mount target = %q, want /home/agent/.codex (parent dir, not file)", mount.Target)
	}
	if mount.ReadOnly {
		t.Errorf("mount is read-only; in-container login needs write access to seed auth.json")
	}
	// Simulate the in-container login writing auth.json.
	loginBytes := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r"}}`)
	hostPath := filepath.Join(mount.Source, "auth.json")
	if err := os.WriteFile(hostPath, loginBytes, 0o600); err != nil {
		t.Fatalf("simulate login write: %v", err)
	}
	prep.CaptureFn(context.Background())
	if len(daemon.puts) != 1 {
		t.Fatalf("daemon.puts = %d, want 1 (capture should PUT the seeded login)", len(daemon.puts))
	}
	if !bytes.Equal(daemon.puts[0].Secret.Value, loginBytes) {
		t.Errorf("seeded value = %q, want %q", daemon.puts[0].Secret.Value, loginBytes)
	}
}

func TestPrepareAuthSpec_RejectsNonConformingVaultPath(t *testing.T) {
	// Bindings whose VaultPath does not follow agents/<name>/oauth
	// would silently misroute through the daemon API today (the
	// daemon endpoint scopes by name and stores at /oauth only).
	// Validation must reject them up front rather than letting them
	// write to the wrong vault path.
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/example/api-key",
			ContainerPath: "/x",
			Render:        func(vault.Secret) ([]byte, error) { return nil, nil },
			Capture:       func([]byte) (vault.Secret, error) { return vault.Secret{}, nil },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "example", spec, newFakeDaemon(),
		newTestLogger(), nil, nil, nil, true)
	if !errors.Is(err, ErrAuthSpecBadVaultPath) {
		t.Fatalf("err = %v, want ErrAuthSpecBadVaultPath", err)
	}
}

// TestPrepareAuthSpec_TwoBindingsDistinctPurposesRouteSeparately is the
// residual #1344 regression: two bindings on the SAME agent name but
// distinct purposes (apikey + oauth) must route to distinct vault
// destinations. The EnvBinding at agents/foo/apikey is host-acquire
// seeded; the FileBinding at agents/foo/oauth is vault-resident. The
// apikey PUT must land at (foo, apikey) and the oauth read at
// (foo, oauth) — a routing bug that collapses both onto name `foo`
// would overwrite the oauth slot with the apikey value (or vice-versa)
// and fail these assertions. The re-keyed fakeDaemon is what makes the
// misroute observable rather than silently green.
func TestPrepareAuthSpec_TwoBindingsDistinctPurposesRouteSeparately(t *testing.T) {
	daemon := newFakeDaemon()
	// Seed only the oauth slot (vault-resident FileBinding). The apikey
	// slot is empty so the EnvBinding takes the host-acquire path.
	oauthEnvelope := []byte(`{"claudeAiOauth":{"accessToken":"oauth-tok"}}`)
	daemon.seedPurpose("foo", "oauth", oauthEnvelope)

	spy := &spyAcquirer{secret: vault.Secret{Value: []byte("sk-apikey")}}
	spec := AuthSpec{
		EnvBindings: []EnvBinding{{
			VaultPath: "agents/foo/apikey",
			Render: func(s vault.Secret) (map[string]string, error) {
				return map[string]string{"FOO_KEY": string(s.Value)}, nil
			},
			HostAcquire: spy.acquire,
		}},
		FileBindings: []FileBinding{{
			VaultPath:     "agents/foo/oauth",
			ContainerPath: "/home/agent/.foo/.credentials.json",
			Mode:          0o600,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}

	prep, err := prepareAuthSpec(context.Background(), "foo", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	// The apikey EnvBinding host-acquired and PUT exactly one entry, at
	// (foo, apikey) — NOT (foo, oauth).
	if len(daemon.puts) != 1 {
		t.Fatalf("PUTs = %d, want 1 (the apikey host-acquire seed)", len(daemon.puts))
	}
	if daemon.puts[0].Name != "foo" || daemon.puts[0].Purpose != "apikey" {
		t.Errorf("PUT destination = (%q,%q), want (foo,apikey)", daemon.puts[0].Name, daemon.puts[0].Purpose)
	}

	// The oauth read landed at (foo, oauth): assert a GET recorded that
	// destination.
	var sawOAuthGet bool
	for _, g := range daemon.gets {
		if g.Name == "foo" && g.Purpose == "oauth" {
			sawOAuthGet = true
		}
	}
	if !sawOAuthGet {
		t.Errorf("no GET at (foo,oauth) recorded; gets=%v", daemon.gets)
	}

	// The apikey seed did NOT overwrite the oauth slot: the oauth slot
	// still holds the original envelope, and the apikey slot holds the
	// acquired key.
	gotOAuth, err := daemon.GetAgentCredentials(context.Background(), "foo", "oauth")
	if err != nil {
		t.Fatalf("GET (foo,oauth): %v", err)
	}
	if string(gotOAuth.Value) != string(oauthEnvelope) {
		t.Errorf("oauth slot = %q, want the original envelope %q (apikey PUT must not clobber it)", gotOAuth.Value, oauthEnvelope)
	}
	gotApikey, err := daemon.GetAgentCredentials(context.Background(), "foo", "apikey")
	if err != nil {
		t.Fatalf("GET (foo,apikey): %v", err)
	}
	if string(gotApikey.Value) != "sk-apikey" {
		t.Errorf("apikey slot = %q, want sk-apikey", gotApikey.Value)
	}

	// The rendered env var carries the apikey, and the oauth credential
	// rendered into a mounted file.
	if got := prep.EnvAdditions["FOO_KEY"]; got != "sk-apikey" {
		t.Errorf("EnvAdditions[FOO_KEY] = %q, want sk-apikey", got)
	}
	if !prep.RenderedAnyCredential {
		t.Errorf("RenderedAnyCredential = false; both bindings rendered")
	}
}

func TestPrepareAuthSpec_R30BootstrapLineFiresOnEmptyVault(t *testing.T) {
	// RenderedAnyCredential must be false when no FileBinding or
	// EnvBinding rendered anything, even if a StaticFile produced a
	// mount. The launcher uses this signal for the R30 status line.
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
		StaticFiles: []StaticFile{{
			ContainerPath: "/home/agent/.claude.json",
			Mode:          0o644,
			Content:       []byte(`{}`),
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, newFakeDaemon(),
		newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()
	if !prep.HasBindings {
		t.Errorf("HasBindings = false, want true")
	}
	if prep.RenderedAnyCredential {
		t.Errorf("RenderedAnyCredential = true on empty vault; the R30 status line would not fire")
	}
}

func TestPrepareAuthSpec_RenderedAnyCredentialIsTrueOnHit(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte(`{"claudeAiOauth":{"accessToken":"a"}}`))
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()
	if !prep.RenderedAnyCredential {
		t.Errorf("RenderedAnyCredential = false on vault hit")
	}
}

func TestPrepareAuthSpec_MountAsFileSkipsParentDirMount(t *testing.T) {
	// Codex's auth.json needs a file mount, not a parent-dir mount,
	// so the read-only config.toml mount that ConfigureMCP installs
	// at the same parent directory can coexist. MountAsFile = true
	// must produce exactly one volume targeting the binding's
	// ContainerPath, and zero volumes targeting the parent dir.
	daemon := newFakeDaemon()
	daemon.seed("codex", []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r"}}`))
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/codex/oauth",
			ContainerPath: "/home/agent/.codex/auth.json",
			Mode:          0o600,
			MountAsFile:   true,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "codex", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 file mount", len(prep.Mounts))
	}
	if prep.Mounts[0].Target != "/home/agent/.codex/auth.json" {
		t.Errorf("mount target = %q, want /home/agent/.codex/auth.json (file mount)", prep.Mounts[0].Target)
	}
}

// claudeFileBindingSpec is a minimal AuthSpec that renders one file
// binding into the transient dir, used by the chown-hook wiring tests.
func claudeFileBindingSpec() AuthSpec {
	return AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Mode:          0o600,
			Required:      true,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
}

func TestPrepareAuthSpec_ChownHookInvokedWithHostRootAfterFilesExist(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`))

	var gotDir string
	var sawFile bool
	hook := func(dir string) error {
		gotDir = dir
		// The rendered credential file must already be on disk when the
		// chown runs so the recursive chown covers it. The transient
		// root mirrors the container parent path beneath it.
		if _, err := os.Stat(filepath.Join(dir, "home", "agent", ".claude", ".credentials.json")); err == nil {
			sawFile = true
		}
		return nil
	}

	prep, err := prepareAuthSpec(context.Background(), "claude", claudeFileBindingSpec(),
		daemon, newTestLogger(), nil, hook, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	// The chown is deferred (issue #1488): prep wraps the hook in
	// prep.ChownFn and the launcher invokes it AFTER placing the MCP
	// config, not at prep time. So the hook must not have run yet.
	if gotDir != "" {
		t.Fatal("chown hook ran during prepareAuthSpec; it must be deferred to prep.ChownFn so the launcher can place the MCP config first")
	}
	if prep.ChownFn == nil {
		t.Fatal("prep.ChownFn must be non-nil when a chown hook is supplied")
	}
	if err := prep.ChownFn(); err != nil {
		t.Fatalf("prep.ChownFn: %v", err)
	}

	if gotDir == "" {
		t.Fatal("chown hook was never invoked")
	}
	if len(prep.Mounts) == 0 {
		t.Fatal("expected at least one mount")
	}
	// The hook must receive the transient root: the group dir mounted
	// into the container lives under it, so a recursive chown of the
	// root covers both the mounted parent and its rendered files.
	mountSource := prep.Mounts[0].Source
	rel, err := filepath.Rel(gotDir, mountSource)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("hook dir %q is not an ancestor of mount source %q; the chown must cover the mounted dir", gotDir, mountSource)
	}
	if !sawFile {
		t.Error("chown hook ran before the credential file was written; it must run after so the chown covers rendered files")
	}
}

func TestPrepareAuthSpec_NilChownHookLeavesFilesIntact(t *testing.T) {
	daemon := newFakeDaemon()
	envelope := []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`)
	daemon.seed("claude", envelope)

	// A nil hook is the non-Linux path: prep must still succeed and the
	// rendered file must be present and unchanged.
	prep, err := prepareAuthSpec(context.Background(), "claude", claudeFileBindingSpec(),
		daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1", len(prep.Mounts))
	}
	got, err := os.ReadFile(filepath.Join(prep.Mounts[0].Source, ".credentials.json"))
	if err != nil {
		t.Fatalf("read rendered file: %v", err)
	}
	if !bytes.Equal(got, envelope) {
		t.Errorf("rendered bytes = %q, want %q", got, envelope)
	}
}

func TestPrepareAuthSpec_ChownHookErrorIsNonFatal(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`))

	stderr := &bytes.Buffer{}
	hook := func(string) error { return errors.New("resolve agent uid: boom") }

	prep, err := prepareAuthSpec(context.Background(), "claude", claudeFileBindingSpec(),
		daemon, newTestLogger(), stderr, hook, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec must not fail on chown hook error, got %v", err)
	}
	defer prep.Cleanup()

	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (prep proceeds despite hook error)", len(prep.Mounts))
	}
	// The chown is deferred into prep.ChownFn; invoking it surfaces the
	// non-fatal warning. A hook error must not propagate out of ChownFn
	// (the launcher proceeds with the launch).
	if err := prep.ChownFn(); err != nil {
		t.Fatalf("ChownFn must not propagate a hook error, got %v", err)
	}
	// The permanent diagnostic must name the UID mismatch and the
	// chown-to-agent-UID remedy so a regression is attributable.
	warned := stderr.String()
	if !strings.Contains(warned, "chown") || !strings.Contains(warned, "UID") {
		t.Errorf("warning %q must name the chown fix and UID mismatch", warned)
	}
}

func TestPrepareAuthSpec_ReclaimHookRunsBeforeCaptureRead(t *testing.T) {
	envelope := []byte(`{"claudeAiOauth":{"accessToken":"old"}}`)
	rotated := []byte(`{"claudeAiOauth":{"accessToken":"new"}}`)
	daemon := newFakeDaemon()
	daemon.seed("claude", envelope)

	var reclaimedDir string
	reclaim := func(dir string) error { reclaimedDir = dir; return nil }

	prep, err := prepareAuthSpec(context.Background(), "claude", claudeFileBindingSpec(),
		daemon, newTestLogger(), nil, nil, reclaim, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	// Simulate the agent rotating the credential in-container.
	hostPath := filepath.Join(prep.Mounts[0].Source, ".credentials.json")
	if err := os.WriteFile(hostPath, rotated, 0o600); err != nil {
		t.Fatalf("simulate rotation: %v", err)
	}

	prep.CaptureFn(context.Background())

	if reclaimedDir == "" {
		t.Fatal("reclaim hook was never invoked; capture must reclaim ownership before reading the rotated file")
	}
	// The reclaim must cover the mounted dir: its source lives under the
	// transient root the hook received, so a recursive chown back to the
	// host UID makes the rotated file readable for the PUT.
	rel, err := filepath.Rel(reclaimedDir, prep.Mounts[0].Source)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("reclaim dir %q is not an ancestor of mount source %q", reclaimedDir, prep.Mounts[0].Source)
	}
	if len(daemon.puts) != 1 || !bytes.Equal(daemon.puts[0].Secret.Value, rotated) {
		t.Fatalf("rotated bytes must be captured after reclaim; puts=%d", len(daemon.puts))
	}
}

func TestPrepareAuthSpec_ReclaimHookErrorIsNonFatal(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`))

	stderr := &bytes.Buffer{}
	reclaim := func(string) error { return errors.New("chown via runtime: boom") }

	prep, err := prepareAuthSpec(context.Background(), "claude", claudeFileBindingSpec(),
		daemon, newTestLogger(), stderr, nil, reclaim, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	// A reclaim failure must not abort capture: the read is still
	// attempted (it succeeds here because the test owns the file), and the
	// warning names the reclaim step so a regression is attributable.
	prep.CaptureFn(context.Background())

	warned := stderr.String()
	if !strings.Contains(warned, "reclaim") {
		t.Errorf("warning %q must name the reclaim step", warned)
	}
	if len(daemon.puts) != 1 {
		t.Fatalf("capture must still attempt the PUT after a non-fatal reclaim error; puts=%d", len(daemon.puts))
	}
}

// freshnessSpec builds a single-FileBinding AuthSpec with byte-identity
// Render/Capture and the supplied Fresher, for exercising CaptureFn's
// presence-aware PUT decision.
func freshnessSpec(fresher func(captured, current vault.Secret) (bool, error)) AuthSpec {
	return AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Mode:          0o600,
			Required:      false,
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
			Fresher:       fresher,
		}},
	}
}

func captureHostPath(prep authSpecPrep) string {
	return filepath.Join(prep.Mounts[0].Source, ".credentials.json")
}

// P0 regression (see #1010): a first login seeds an empty vault, so the
// capture-time GET returns NotFound just like a deliberate delete. The
// entry was ABSENT at render, so this must PUT (seed) — even with a
// non-nil Fresher that would otherwise gate the write. Skipping it would
// silently lose every first-login credential.
func TestPrepareAuthSpec_FirstLoginSeedWithFresherStillPuts(t *testing.T) {
	daemon := newFakeDaemon() // empty vault → absent at render
	// A Fresher that always reports "not fresher" — proves the first-login
	// branch never consults it (there is no prior entry to compare).
	spec := freshnessSpec(func(_, _ vault.Secret) (bool, error) { return false, nil })

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if err := os.WriteFile(captureHostPath(prep), []byte("seeded-on-first-login"), 0o600); err != nil {
		t.Fatalf("simulate first-login write: %v", err)
	}
	prep.CaptureFn(context.Background())

	if len(daemon.puts) != 1 {
		t.Fatalf("first-login seed must PUT even with a non-nil Fresher; puts=%d", len(daemon.puts))
	}
	if got := string(daemon.puts[0].Secret.Value); got != "seeded-on-first-login" {
		t.Errorf("seeded value = %q, want seeded-on-first-login", got)
	}
}

func TestPrepareAuthSpec_DeliberateDeleteMidSessionSkipsPut(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte("old")) // present at render
	spec := freshnessSpec(func(_, _ vault.Secret) (bool, error) { return true, nil })

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

	if err := os.WriteFile(captureHostPath(prep), []byte("rotated"), 0o600); err != nil {
		t.Fatalf("simulate rotation: %v", err)
	}
	daemon.deleteEntry("claude") // operator deletes mid-session

	prep.CaptureFn(context.Background())

	if len(daemon.puts) != 0 {
		t.Fatalf("a mid-session delete (present@render, absent@capture) must be honored; puts=%d", len(daemon.puts))
	}
}

func TestPrepareAuthSpec_StaleCaptureNotFresherSkipsPut(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte("current")) // present at render and capture
	spec := freshnessSpec(func(_, _ vault.Secret) (bool, error) { return false, nil })

	var stderr bytes.Buffer
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), &stderr, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()
	if err := os.WriteFile(captureHostPath(prep), []byte("stale-rotation"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	prep.CaptureFn(context.Background())

	if len(daemon.puts) != 0 {
		t.Fatalf("a not-fresher capture must not clobber the vault; puts=%d", len(daemon.puts))
	}
	// The not-newer outcome is the freshness gate working as designed, not
	// a failure. It must NOT surface a "capture failed" line on stderr —
	// the common steady-state on every clean exit after the first login
	// would otherwise alarm the user with a benign no-op.
	if strings.Contains(stderr.String(), "failed") {
		t.Errorf("not-newer capture must not print a failure to stderr; got %q", stderr.String())
	}
}

func TestPrepareAuthSpec_FresherTruePuts(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte("current"))
	spec := freshnessSpec(func(_, _ vault.Secret) (bool, error) { return true, nil })

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()
	if err := os.WriteFile(captureHostPath(prep), []byte("fresh-rotation"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	prep.CaptureFn(context.Background())

	if len(daemon.puts) != 1 || string(daemon.puts[0].Secret.Value) != "fresh-rotation" {
		t.Fatalf("a strictly-fresher capture must PUT; puts=%d", len(daemon.puts))
	}
}

func TestPrepareAuthSpec_FresherErrorSkipsPut(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte("current"))
	spec := freshnessSpec(func(_, _ vault.Secret) (bool, error) { return false, errors.New("malformed envelope") })

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()
	if err := os.WriteFile(captureHostPath(prep), []byte("rotation"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	prep.CaptureFn(context.Background())

	if len(daemon.puts) != 0 {
		t.Fatalf("a Fresher error must retain the prior entry (skip PUT); puts=%d", len(daemon.puts))
	}
}

// #1017 regression: when the captured envelope parses but the current
// vault entry is corrupt, the Fresher wraps ErrCurrentEnvelopeMalformed.
// A corrupt entry is unusable (Render rejects it next launch), so the
// valid fresh capture must overwrite it rather than be skipped — the
// opposite of the generic Fresher-error path above.
func TestPrepareAuthSpec_CurrentMalformedOverwritesWithCapture(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte("corrupt-not-json")) // present at render and capture
	stderr := &bytes.Buffer{}
	spec := freshnessSpec(func(_, _ vault.Secret) (bool, error) {
		return false, fmt.Errorf("%w: parse current: bad", ErrCurrentEnvelopeMalformed)
	})

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), stderr, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()
	if err := os.WriteFile(captureHostPath(prep), []byte("valid-fresh-capture"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	prep.CaptureFn(context.Background())

	if len(daemon.puts) != 1 || string(daemon.puts[0].Secret.Value) != "valid-fresh-capture" {
		t.Fatalf("a malformed current entry must be overwritten by the valid capture; puts=%d", len(daemon.puts))
	}
	if !strings.Contains(stderr.String(), "current vault entry is malformed") {
		t.Errorf("expected an overwrite warning on stderr; got %q", stderr.String())
	}
}

// On a SIGINT/SIGTERM salvage the captureCtx may already be cancelled.
// A cancelled freshness GET must skip the PUT with a distinct
// cancellation path rather than clobber or blind-PUT.
func TestPrepareAuthSpec_CaptureGetCancelledSkipsPut(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte("current"))
	// Render GET succeeds (store hit); capture GET returns context.Canceled.
	daemon.setGetSeqOAuth("claude", []error{nil, context.Canceled})
	spec := freshnessSpec(func(_, _ vault.Secret) (bool, error) { return true, nil })

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil, true)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()
	if err := os.WriteFile(captureHostPath(prep), []byte("rotation"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	prep.CaptureFn(context.Background())

	if len(daemon.puts) != 0 {
		t.Fatalf("a cancelled capture GET must skip the freshness-gated PUT; puts=%d", len(daemon.puts))
	}
}

func TestVaultBindingNameAndPurpose(t *testing.T) {
	tests := []struct {
		in, wantName, wantPurpose string
	}{
		{"agents/claude/oauth", "claude", "oauth"},
		{"agents/codex/oauth", "codex", "oauth"},
		{"/agents/claude/oauth", "claude", "oauth"},
		{"agents/example/oauth", "example", "oauth"},
		// A non-oauth purpose is threaded through, not discarded — the
		// SUB2/SUB3 foundation this PR establishes.
		{"agents/claude/apikey", "claude", "apikey"},
		{"agents/codex/apikey", "codex", "apikey"},
		// Malformed/short paths fall back to the canonical oauth purpose
		// so callers always receive a usable purpose segment.
		{"agents/claude", "claude", "oauth"},
		{"orphan", "orphan", "oauth"},
	}
	for _, tc := range tests {
		name, purpose := vaultBindingNameAndPurpose(tc.in)
		if name != tc.wantName || purpose != tc.wantPurpose {
			t.Errorf("vaultBindingNameAndPurpose(%q) = (%q, %q), want (%q, %q)",
				tc.in, name, purpose, tc.wantName, tc.wantPurpose)
		}
	}
}
