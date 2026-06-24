package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
)

// skipIfClaudeKeychainOnly skips a Claude file-fixture test on macOS,
// where Claude credentials live in the Keychain rather than a file the
// HOME-redirect fixture can seed. Reading the real Keychain item in a
// test is forbidden, so the authoritative coverage for the Claude
// file-mode path is the ubuntu CI shard (cmd/aileron's full suite runs
// there). Codex-based tests cover the daemon-side behavior on every OS
// because Codex prefers its auth.json file on macOS too.
func skipIfClaudeKeychainOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("Claude is Keychain-only on macOS; file-fixture path is covered on the ubuntu CI shard")
	}
}

// authTestRegistry builds a registry holding the agents host-import
// cares about plus an unsupported one, mirroring main()'s wiring.
func authTestRegistry() *launch.Registry {
	r := launch.NewRegistry()
	r.Register(agents.Claude{})
	r.Register(agents.Codex{})
	r.Register(agents.Goose{})
	return r
}

// seedHostCredential writes a fixture credential file under a redirected
// HOME so hostimport's file-mode extractor reads it. Returns nothing;
// the test asserts on the daemon PUT body. These cmd/aileron tests run
// on the ubuntu shards where file mode is the extraction path.
func seedHostCredential(t *testing.T, agent string, content []byte) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var rel string
	switch agent {
	case "claude":
		rel = filepath.Join(".claude", ".credentials.json")
	case "codex":
		rel = filepath.Join(".codex", "auth.json")
	default:
		t.Fatalf("unsupported agent in fixture: %s", agent)
	}
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunAuth_ImportClaudeHappyPath(t *testing.T) {
	skipIfClaudeKeychainOnly(t)
	envelope := []byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"r","scopes":["x"]}}`)
	seedHostCredential(t, "claude", envelope)

	var received []byte
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/vault/agents/claude/credentials" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body agentCredentialsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		received = body.Value
		w.WriteHeader(http.StatusNoContent)
	})

	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"claude", "--import-from-host"}, authTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !bytes.Equal(received, envelope) {
		t.Errorf("PUT body = %q, want %q (byte-verbatim)", received, envelope)
	}
	if !strings.Contains(stdout.String(), "Imported host credentials to agents/claude/oauth") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunAuth_ImportCodexHappyPath(t *testing.T) {
	envelope := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r"}}`)
	seedHostCredential(t, "codex", envelope)

	var received []byte
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vault/agents/codex/credentials" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body agentCredentialsBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		received = body.Value
		w.WriteHeader(http.StatusNoContent)
	})

	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"codex", "--import-from-host"}, authTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !bytes.Equal(received, envelope) {
		t.Errorf("PUT body = %q, want %q", received, envelope)
	}
}

func TestRunAuth_UnsupportedAgent(t *testing.T) {
	// No HTTP server: an unsupported agent must fail before any extract
	// or daemon call.
	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"goose", "--import-from-host"}, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not supported for") {
		t.Errorf("stderr = %q, want unsupported message", stderr.String())
	}
}

func TestRunAuth_MissingImportFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"claude"}, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--import-from-host is required") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestRunAuth_FlagBeforeAgentRejected pins the contract that the agent
// name must be the first positional and flags follow it (auth.go:35-37).
// Invoking `aileron auth --import-from-host claude` makes the flag string
// the agent name, leaving --import-from-host unset, so the command fails
// with the "is required" error rather than silently importing claude.
func TestRunAuth_FlagBeforeAgentRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"--import-from-host", "claude"}, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--import-from-host is required") {
		t.Errorf("stderr = %q, want the flag-required error (flags must follow the agent name)", stderr.String())
	}
}

func TestRunAuth_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAuth(nil, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunAuth_HostNotAuthenticated(t *testing.T) {
	// On macOS an absent Codex file would fall back to the real "Codex
	// Auth" Keychain item, which a test must not touch; Claude is
	// keychain-only there. Run the file-absence assertion on the ubuntu
	// shard via the skip guard.
	skipIfClaudeKeychainOnly(t)

	// Redirect HOME to an empty dir so no credential file exists.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"claude", "--import-from-host"}, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	msg := stderr.String()
	if !strings.Contains(msg, "no host credentials found") {
		t.Errorf("stderr = %q, want not-authenticated message", msg)
	}
	if !strings.Contains(msg, "--sandbox=docker") {
		t.Errorf("stderr = %q, want in-container recovery path", msg)
	}
}

// TestRunAuth_ExtractError covers the non-ErrNotAuthenticated extract
// failure branch: a credential file that exists but is unreadable
// surfaces the underlying read error rather than the login-recovery
// message. Skipped when running as root (chmod 000 does not deny root)
// and on Windows (POSIX perms do not apply).
func TestRunAuth_ExtractError(t *testing.T) {
	skipIfClaudeKeychainOnly(t) // darwin Claude path ignores the HOME fixture
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions do not gate reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not deny root")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":"t"}}`), 0o000); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"claude", "--import-from-host"}, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "error reading host credentials") {
		t.Errorf("stderr = %q, want extract-error message", stderr.String())
	}
}

func TestRunAuth_MalformedEnvelope(t *testing.T) {
	// A file present but failing the agent's Capture (Codex envelope
	// missing the required refresh_token). Codex reads its file on every
	// OS, so this exercises the Capture-rejection path cross-platform.
	seedHostCredential(t, "codex", []byte(`{"auth_mode":"chatgpt","tokens":{}}`))

	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"codex", "--import-from-host"}, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not a valid envelope") {
		t.Errorf("stderr = %q, want malformed-envelope message", stderr.String())
	}
}

func TestRunAuth_DaemonLocked(t *testing.T) {
	seedHostCredential(t, "codex", []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r"}}`))
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusLocked)
	})

	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"codex", "--import-from-host"}, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "locked") {
		t.Errorf("stderr = %q, want locked message", stderr.String())
	}
}

func TestRunAuth_DaemonUnexpectedStatus(t *testing.T) {
	// An unexpected daemon status maps to the default "server returned"
	// branch with the body echoed for diagnosis.
	seedHostCredential(t, "codex", []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r"}}`))
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})

	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"codex", "--import-from-host"}, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "server returned 500") || !strings.Contains(stderr.String(), "boom") {
		t.Errorf("stderr = %q, want server-returned-500 with body", stderr.String())
	}
}

// fakeAuthAgent is a stand-in registered under a supported name so the
// command's defensive AuthSpec guards (missing binding, nil Capture)
// can be exercised without depending on the real agent definitions
// staying misconfigured.
type fakeAuthAgent struct {
	name string
	spec launch.AuthSpec
}

func (f fakeAuthAgent) Name() string                { return f.name }
func (f fakeAuthAgent) BinaryNames() []string       { return []string{f.name} }
func (f fakeAuthAgent) Args(_ launch.Mode) []string { return nil }
func (f fakeAuthAgent) Env() map[string]string {
	return nil
}
func (f fakeAuthAgent) LLMEndpointEnv() string { return "" }
func (f fakeAuthAgent) ConfigureMCP(string, map[string]string, string, launch.Mode) ([]string, []launch.MCPMount, error) {
	return nil, nil, nil
}
func (f fakeAuthAgent) AuthSpec() launch.AuthSpec { return f.spec }
func (f fakeAuthAgent) SkillsPath() string        { return "" }

// TestRunAuth_BindingMissingCapture covers the defensive guard that
// rejects an agent whose oauth FileBinding declares no Capture
// validator, rather than nil-dereferencing it.
func TestRunAuth_BindingMissingCapture(t *testing.T) {
	r := launch.NewRegistry()
	r.Register(fakeAuthAgent{
		name: "claude",
		spec: launch.AuthSpec{FileBindings: []launch.FileBinding{{
			VaultPath:     "agents/claude/oauth",
			ContainerPath: "/x",
			Capture:       nil,
		}}},
	})
	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"claude", "--import-from-host"}, r, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no Capture validator") {
		t.Errorf("stderr = %q, want nil-Capture guard", stderr.String())
	}
}

// TestRunAuth_BindingPathMissing covers the guard that fires when a
// supported, registered agent has no binding at agents/<name>/oauth.
func TestRunAuth_BindingPathMissing(t *testing.T) {
	r := launch.NewRegistry()
	r.Register(fakeAuthAgent{name: "codex", spec: launch.AuthSpec{}})
	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"codex", "--import-from-host"}, r, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no credential binding at agents/codex/oauth") {
		t.Errorf("stderr = %q, want missing-binding guard", stderr.String())
	}
}

// TestRunAuth_UnknownAgentNotRegistered exercises the registry-miss
// branch: a supported name (codex) that is nonetheless absent from the
// registry surfaces a clear "unknown agent" error before any host read.
func TestRunAuth_UnknownAgentNotRegistered(t *testing.T) {
	empty := launch.NewRegistry() // codex not registered
	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"codex", "--import-from-host"}, empty, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown agent") {
		t.Errorf("stderr = %q, want unknown-agent error", stderr.String())
	}
}

func TestRunAuth_DaemonNoVault(t *testing.T) {
	seedHostCredential(t, "codex", []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r"}}`))
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"codex", "--import-from-host"}, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not configured with a vault") {
		t.Errorf("stderr = %q, want no-vault message", stderr.String())
	}
}
