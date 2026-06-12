//go:build !darwin

package hostimport

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestExtract_FileModeReturnsBytesVerbatim confirms the file-mode
// extractor (Linux/Windows) reads the credential file byte-for-byte,
// including a trailing newline, since agent credential files are
// exact-byte artifacts. Runs on ubuntu shards and on windows-latest CI
// (the build tag selects this file on both).
func TestExtract_FileModeReturnsBytesVerbatim(t *testing.T) {
	want := []byte("{\"claudeAiOauth\":{\"accessToken\":\"tok\"}}\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Extract(AgentClaude, Options{FilePathOverride: path})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Extract = %q, want %q (verbatim)", got, want)
	}
}

// TestExtract_FileModeMissingFile maps an absent credential file to
// ErrNotAuthenticated so the CLI surfaces the in-container login
// recovery path.
func TestExtract_FileModeMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	_, err := Extract(AgentCodex, Options{FilePathOverride: missing})
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("Extract(missing): err = %v, want ErrNotAuthenticated", err)
	}
}

// TestExtract_FileModeEmptyFile maps a zero-byte credential file to
// ErrNotAuthenticated: an empty credential is as useless as a missing
// one.
func TestExtract_FileModeEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Extract(AgentClaude, Options{FilePathOverride: path})
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("Extract(empty): err = %v, want ErrNotAuthenticated", err)
	}
}

// TestExtract_FileModeDefaultPath confirms that with no override the
// extractor resolves the agent's default path under the user's home
// directory. We redirect HOME (and USERPROFILE on Windows) to a temp
// dir and seed the canonical layout so the test never reads the
// developer's real install.
func TestExtract_FileModeDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows home resolution

	want := []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r"}}`)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), want, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Extract(AgentCodex, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Extract = %q, want %q", got, want)
	}
}
