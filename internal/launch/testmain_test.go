package launch_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain spins up a fake daemon, points AILERON_API_URL at it, and
// installs a fake aileron-mcp on PATH for every test in this package.
//
// Fake daemon (AILERON_API_URL): spawn.Resolve honors the env var and
// returns the URL immediately without trying to fork-exec a real
// daemon binary. The fake handles the endpoints Launch hits:
//
//   - POST /v1/sessions          mints a stable test session id
//   - POST /v1/sessions/{id}/end accepts and returns the record
//   - GET  /v1/vault/local/status reports unlocked
//
// Fake aileron-mcp: per ADR-0015, Launch requires aileron-mcp to be
// resolvable (next to the running binary or on PATH). Production
// always satisfies this; the test binary doesn't, so we plant a
// no-op shell script in a tempdir and prepend that dir to PATH. The
// MCP server never runs in these tests — Launch only resolves the
// path to hand to the agent's ConfigureMCP method.
//
// Tests that need a different fake (e.g., to assert the request body,
// simulate a locked vault, or exercise the missing-aileron-mcp error
// path) can override the relevant env vars themselves.
func TestMain(m *testing.M) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          "01HK0000000000000000000FAK",
				"started_at":  time.Now().UTC(),
				"agent":       "test",
				"working_dir": "/test",
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(r.URL.Path, "/end"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          "01HK0000000000000000000FAK",
				"started_at":  time.Now().UTC(),
				"ended_at":    time.Now().UTC(),
				"exit_code":   0,
				"agent":       "test",
				"working_dir": "/test",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vault/local/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"locked": false, "state": "unlocked"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	_ = os.Setenv("AILERON_API_URL", srv.URL)

	mcpDir, err := os.MkdirTemp("", "aileron-mcp-fake-")
	if err != nil {
		panic("creating fake aileron-mcp dir: " + err.Error())
	}
	defer os.RemoveAll(mcpDir)
	mcpBin := filepath.Join(mcpDir, "aileron-mcp")
	if err := os.WriteFile(mcpBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		panic("writing fake aileron-mcp: " + err.Error())
	}
	_ = os.Setenv("PATH", mcpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	os.Exit(m.Run())
}
