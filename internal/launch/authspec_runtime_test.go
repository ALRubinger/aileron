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
	puts      []putRecord
	gets      []string
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
	}
}

func (f *fakeDaemon) seed(name string, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[name] = vault.Secret{Value: value, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}
}

func (f *fakeDaemon) GetAgentCredentials(_ context.Context, name string) (vault.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, name)
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
		newFakeDaemon(), newTestLogger(), nil)
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

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil)
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

	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil)
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
		newTestLogger(), nil)
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
		newTestLogger(), nil)
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
			Capture:       func(b []byte) (vault.Secret, error) { return vault.Secret{Value: b, Metadata: vault.Metadata{Type: "oauth_refresh_token"}}, nil },
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil)
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
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), stderr)
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
	prep, err := prepareAuthSpec(context.Background(), "codex", spec, daemon, newTestLogger(), nil)
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
	_, err := prepareAuthSpec(context.Background(), "codex", spec, daemon, newTestLogger(), nil)
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
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil)
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
			VaultPath: "agents/example/api-key",
			Required:  true,
			Render: func(s vault.Secret) (map[string]string, error) {
				return map[string]string{"EXAMPLE_API_KEY": string(s.Value)}, nil
			},
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "example", spec, daemon, newTestLogger(), nil)
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
	prep, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), stderr)
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
			VaultPath: "agents/example/api-key",
			Required:  true,
			Render: func(s vault.Secret) (map[string]string, error) {
				return map[string]string{"X": string(s.Value)}, nil
			},
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "example", spec, newFakeDaemon(),
		newTestLogger(), nil)
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
			VaultPath: "agents/example/api-key",
			Required:  false,
			Render: func(s vault.Secret) (map[string]string, error) {
				return map[string]string{"X": string(s.Value)}, nil
			},
		}},
	}
	prep, err := prepareAuthSpec(context.Background(), "example", spec, newFakeDaemon(),
		newTestLogger(), nil)
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
			VaultPath: "agents/example/api-key",
			Required:  false,
			Render:    func(s vault.Secret) (map[string]string, error) { return nil, nil },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "example", spec, daemon, newTestLogger(), nil)
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
			VaultPath: "agents/example/api-key",
			Required:  true,
			Render:    func(s vault.Secret) (map[string]string, error) { return nil, errors.New("bad shape") },
		}},
	}
	_, err := prepareAuthSpec(context.Background(), "example", spec, daemon, newTestLogger(), nil)
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
	_, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil)
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
	_, err := prepareAuthSpec(context.Background(), "claude", spec, daemon, newTestLogger(), nil)
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
		newTestLogger(), nil)
	if err == nil {
		t.Fatal("expected validation error for nil Render")
	}
	if !errors.Is(err, ErrAuthSpecRenderNil) {
		t.Errorf("err = %v, want ErrAuthSpecRenderNil", err)
	}
}

func TestVaultBindingTail(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"agents/claude/oauth", "claude"},
		{"agents/codex/oauth", "codex"},
		{"/agents/claude/oauth", "claude"},
		{"agents/example/api-key", "example"},
		{"orphan", "orphan"},
	}
	for _, tc := range tests {
		if got := vaultBindingTail(tc.in); got != tc.want {
			t.Errorf("vaultBindingTail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
