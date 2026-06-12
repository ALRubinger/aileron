package launch

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
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
type fakeDaemon struct {
	mu        sync.Mutex
	store     map[string]vault.Secret
	getErrors map[string]error
	putErrors map[string]error
	// getSeq, when populated for a name, supplies a per-call error
	// sequence consumed in order. A nil entry means "no error, use the
	// store" for that call. This lets a scenario make the render GET
	// succeed and the capture GET cancel/fail without affecting the
	// other. getSeq takes precedence over getErrors for that name.
	getSeq map[string][]error
	puts   []putRecord
	gets   []string
}

type putRecord struct {
	Name   string
	Secret vault.Secret
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{
		store:     map[string]vault.Secret{},
		getErrors: map[string]error{},
		putErrors: map[string]error{},
		getSeq:    map[string][]error{},
	}
}

func (f *fakeDaemon) seed(name string, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[name] = vault.Secret{Value: value, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}
}

// deleteEntry simulates an operator running `aileron vault delete`
// mid-session: the entry vanishes from the store so a subsequent GET
// returns ErrAgentCredentialsNotFound.
func (f *fakeDaemon) deleteEntry(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, name)
}

func (f *fakeDaemon) GetAgentCredentials(_ context.Context, name string) (vault.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, name)
	if seq, ok := f.getSeq[name]; ok && len(seq) > 0 {
		err := seq[0]
		f.getSeq[name] = seq[1:]
		if err != nil {
			return vault.Secret{}, err
		}
		s, ok := f.store[name]
		if !ok {
			return vault.Secret{}, ErrAgentCredentialsNotFound
		}
		return s, nil
	}
	if err, ok := f.getErrors[name]; ok {
		return vault.Secret{}, err
	}
	s, ok := f.store[name]
	if !ok {
		return vault.Secret{}, ErrAgentCredentialsNotFound
	}
	return s, nil
}

func (f *fakeDaemon) PutAgentCredentials(_ context.Context, name string, secret vault.Secret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.putErrors[name]; ok {
		return err
	}
	f.puts = append(f.puts, putRecord{Name: name, Secret: secret})
	f.store[name] = secret
	return nil
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestPrepareAuthSpec_EmptySpecIsNoOp(t *testing.T) {
	prep, err := prepareAuthSpec(context.Background(), "claude", AuthSpec{},
		newFakeDaemon(), newTestLogger(), nil, nil, nil)
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

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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
		newTestLogger(), nil, nil, nil)
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
		newTestLogger(), nil, nil, nil)
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
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), stderr, nil, nil)
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
	prep, err := prepareAuthSpec(context.Background(), "codex", spec, daemon, newTestLogger(), nil, nil, nil)
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
	_, err := prepareAuthSpec(context.Background(), "codex", spec, daemon, newTestLogger(), nil, nil, nil)
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
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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
	prep, err := prepareAuthSpec(context.Background(), "example", spec, daemon, newTestLogger(), nil, nil, nil)
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
	daemon.putErrors["claude"] = errors.New("daemon offline")

	stderr := &bytes.Buffer{}
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), stderr, nil, nil)
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
		newTestLogger(), nil, nil, nil)
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
		newTestLogger(), nil, nil, nil)
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
	daemon.getErrors["example"] = errors.New("network down")
	spec := AuthSpec{
		EnvBindings: []EnvBinding{{
			VaultPath: "agents/example/oauth",
			Required:  false,
			Render:    func(s vault.Secret) (map[string]string, error) { return nil, nil },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "example", spec, daemon, newTestLogger(), nil, nil, nil)
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
	_, err := prepareAuthSpec(context.Background(), "example", spec, daemon, newTestLogger(), nil, nil, nil)
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
	_, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
	if err == nil {
		t.Fatal("expected error when Render fails")
	}
}

func TestPrepareAuthSpec_FileBindingGetErrorPropagates(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.getErrors["claude"] = errors.New("daemon offline")
	spec := AuthSpec{
		FileBindings: []FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/home/agent/.claude/.credentials.json",
			Render:        func(s vault.Secret) ([]byte, error) { return s.Value, nil },
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b}, nil },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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
		newTestLogger(), nil, nil, nil)
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
	prep, err := prepareAuthSpec(context.Background(), "codex", spec, daemon, newTestLogger(), nil, nil, nil)
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
		newTestLogger(), nil, nil, nil)
	if !errors.Is(err, ErrAuthSpecBadVaultPath) {
		t.Fatalf("err = %v, want ErrAuthSpecBadVaultPath", err)
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
		newTestLogger(), nil, nil, nil)
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
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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
	prep, err := prepareAuthSpec(context.Background(), "codex", spec, daemon, newTestLogger(), nil, nil, nil)
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
		daemon, newTestLogger(), nil, hook, nil)
	if err != nil {
		t.Fatalf("prepareAuthSpec: %v", err)
	}
	defer prep.Cleanup()

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
		daemon, newTestLogger(), nil, nil, nil)
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
		daemon, newTestLogger(), stderr, hook, nil)
	if err != nil {
		t.Fatalf("prepareAuthSpec must not fail on chown hook error, got %v", err)
	}
	defer prep.Cleanup()

	if len(prep.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (prep proceeds despite hook error)", len(prep.Mounts))
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
		daemon, newTestLogger(), nil, nil, reclaim)
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
		daemon, newTestLogger(), stderr, nil, reclaim)
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

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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
}

func TestPrepareAuthSpec_FresherTruePuts(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte("current"))
	spec := freshnessSpec(func(_, _ vault.Secret) (bool, error) { return true, nil })

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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

// On a SIGINT/SIGTERM salvage the captureCtx may already be cancelled.
// A cancelled freshness GET must skip the PUT with a distinct
// cancellation path rather than clobber or blind-PUT.
func TestPrepareAuthSpec_CaptureGetCancelledSkipsPut(t *testing.T) {
	daemon := newFakeDaemon()
	daemon.seed("claude", []byte("current"))
	// Render GET succeeds (store hit); capture GET returns context.Canceled.
	daemon.getSeq["claude"] = []error{nil, context.Canceled}
	spec := freshnessSpec(func(_, _ vault.Secret) (bool, error) { return true, nil })

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil, nil, nil)
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

func TestVaultBindingTail(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"agents/claude/oauth", "claude"},
		{"agents/codex/oauth", "codex"},
		{"/agents/claude/oauth", "claude"},
		{"agents/example/oauth", "example"},
		{"orphan", "orphan"},
	}
	for _, tc := range tests {
		if got := vaultBindingTail(tc.in); got != tc.want {
			t.Errorf("vaultBindingTail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
