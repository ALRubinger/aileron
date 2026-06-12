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
