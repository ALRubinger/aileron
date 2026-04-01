package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `version: "1"
workspace_id: prod
downstream_servers:
  - name: github
    command: ["npx", "-y", "@modelcontextprotocol/server-github"]
    env:
      GITHUB_TOKEN: "vault://secrets/github/token"
      LOG_LEVEL: "debug"
    policy_mapping:
      tool_prefix: git
  - name: filesystem
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/data"]
`

func TestLoad_ValidConfig(t *testing.T) {
	path := writeTempFile(t, "aileron-*.yaml", validYAML)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Version != "1" {
		t.Errorf("Version = %q, want %q", cfg.Version, "1")
	}
	if cfg.WorkspaceID != "prod" {
		t.Errorf("WorkspaceID = %q, want %q", cfg.WorkspaceID, "prod")
	}
	if len(cfg.DownstreamServers) != 2 {
		t.Fatalf("len(DownstreamServers) = %d, want 2", len(cfg.DownstreamServers))
	}

	gh := cfg.DownstreamServers[0]
	if gh.Name != "github" {
		t.Errorf("server[0].Name = %q, want %q", gh.Name, "github")
	}
	if len(gh.Command) != 3 {
		t.Fatalf("server[0].Command length = %d, want 3", len(gh.Command))
	}
	if gh.Command[0] != "npx" {
		t.Errorf("server[0].Command[0] = %q, want %q", gh.Command[0], "npx")
	}
	if gh.Env["GITHUB_TOKEN"] != "vault://secrets/github/token" {
		t.Errorf("server[0].Env[GITHUB_TOKEN] = %q, want vault reference", gh.Env["GITHUB_TOKEN"])
	}
	if gh.PolicyMapping == nil || gh.PolicyMapping.ToolPrefix != "git" {
		t.Errorf("server[0].PolicyMapping.ToolPrefix = %v, want %q", gh.PolicyMapping, "git")
	}

	fs := cfg.DownstreamServers[1]
	if fs.Name != "filesystem" {
		t.Errorf("server[1].Name = %q, want %q", fs.Name, "filesystem")
	}
	if fs.PolicyMapping != nil {
		t.Errorf("server[1].PolicyMapping = %v, want nil", fs.PolicyMapping)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/aileron.yaml")
	if err == nil {
		t.Fatal("Load() expected error for nonexistent path, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTempFile(t, "bad-*.yaml", ":\n  :\n    - :\n  ][invalid")

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for invalid YAML, got nil")
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	cfg := &Config{
		Version: "1",
		DownstreamServers: []DownstreamServer{
			{Name: "server_one", Command: []string{"cmd"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error: %v", err)
	}
}

func TestValidate_EmptyServerName(t *testing.T) {
	cfg := &Config{
		DownstreamServers: []DownstreamServer{
			{Name: "", Command: []string{"cmd"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for empty server name, got nil")
	}
	if !strings.Contains(err.Error(), "name must not be empty") {
		t.Errorf("error = %q, want mention of empty name", err.Error())
	}
}

func TestValidate_DuplicateServerNames(t *testing.T) {
	cfg := &Config{
		DownstreamServers: []DownstreamServer{
			{Name: "dup", Command: []string{"cmd"}},
			{Name: "dup", Command: []string{"cmd"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for duplicate names, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate name") {
		t.Errorf("error = %q, want mention of duplicate", err.Error())
	}
}

func TestValidate_InvalidServerName(t *testing.T) {
	tests := []struct {
		name string
	}{
		{"has space"},
		{"123start"},
		{"special!char"},
		{"dash-name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				DownstreamServers: []DownstreamServer{
					{Name: tt.name, Command: []string{"cmd"}},
				},
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() expected error for name %q, got nil", tt.name)
			}
			if !strings.Contains(err.Error(), "must match") {
				t.Errorf("error = %q, want mention of pattern match", err.Error())
			}
		})
	}
}

func TestValidate_EmptyCommand(t *testing.T) {
	cfg := &Config{
		DownstreamServers: []DownstreamServer{
			{Name: "nocommand", Command: nil},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for empty command, got nil")
	}
	if !strings.Contains(err.Error(), "command must not be empty") {
		t.Errorf("error = %q, want mention of empty command", err.Error())
	}
}

func TestValidate_InvalidVersion(t *testing.T) {
	cfg := &Config{
		Version: "2",
		DownstreamServers: []DownstreamServer{
			{Name: "srv", Command: []string{"cmd"}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for invalid version, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported config version") {
		t.Errorf("error = %q, want mention of unsupported version", err.Error())
	}
}

func TestLoadFromEnvOrDefault_NoConfig(t *testing.T) {
	// Run in an empty temp directory so no aileron.yaml exists.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error: %v", err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Chdir() error: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origDir)
	})

	// Ensure AILERON_CONFIG is unset.
	t.Setenv("AILERON_CONFIG", "")

	cfg, err := LoadFromEnvOrDefault()
	if err != nil {
		t.Fatalf("LoadFromEnvOrDefault() unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadFromEnvOrDefault() returned nil config, want non-nil empty config")
	}
	if len(cfg.DownstreamServers) != 0 {
		t.Errorf("DownstreamServers length = %d, want 0", len(cfg.DownstreamServers))
	}
}

func TestResolveVaultEnv_ResolvesVaultPrefixes(t *testing.T) {
	env := map[string]string{
		"SECRET_KEY": "vault://secrets/api/key",
		"PLAIN":      "plain-value",
	}

	vaultGet := func(path string) ([]byte, error) {
		if path == "secrets/api/key" {
			return []byte("resolved-secret"), nil
		}
		return nil, errors.New("not found")
	}

	resolved, err := ResolveVaultEnv(env, vaultGet)
	if err != nil {
		t.Fatalf("ResolveVaultEnv() unexpected error: %v", err)
	}

	if resolved["SECRET_KEY"] != "resolved-secret" {
		t.Errorf("SECRET_KEY = %q, want %q", resolved["SECRET_KEY"], "resolved-secret")
	}

	// Verify original map is not modified.
	if env["SECRET_KEY"] != "vault://secrets/api/key" {
		t.Error("original env map was modified")
	}
}

func TestResolveVaultEnv_PassesThroughNonVault(t *testing.T) {
	env := map[string]string{
		"PLAIN":    "some-value",
		"ALSO":     "another-value",
		"EMPTY":    "",
		"VAULTISH": "vault:/",
	}

	vaultGet := func(path string) ([]byte, error) {
		t.Fatalf("vaultGet should not be called, but was called with path %q", path)
		return nil, nil
	}

	resolved, err := ResolveVaultEnv(env, vaultGet)
	if err != nil {
		t.Fatalf("ResolveVaultEnv() unexpected error: %v", err)
	}

	for k, v := range env {
		if resolved[k] != v {
			t.Errorf("resolved[%q] = %q, want %q", k, resolved[k], v)
		}
	}
}

func TestResolveVaultEnv_VaultError(t *testing.T) {
	env := map[string]string{
		"SECRET": "vault://secrets/missing",
	}

	vaultGet := func(path string) ([]byte, error) {
		return nil, errors.New("secret not found")
	}

	_, err := ResolveVaultEnv(env, vaultGet)
	if err == nil {
		t.Fatal("ResolveVaultEnv() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "secret not found") {
		t.Errorf("error = %q, want mention of underlying vault error", err.Error())
	}
}

// writeTempFile writes content to a temporary file and returns its path.
// The file is automatically cleaned up when the test completes.
func writeTempFile(t *testing.T, pattern, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, pattern)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return path
}
