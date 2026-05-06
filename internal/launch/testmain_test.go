package launch_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain spins up a fake daemon and points AILERON_API_URL at it
// for every test in this package. spawn.Resolve honors the env var
// and returns the URL immediately without trying to fork-exec a real
// daemon binary — so tests don't need `server` on PATH.
//
// The fake handles the three endpoints Launch hits:
//
//   - POST /v1/sessions          mints a stable test session id
//   - POST /v1/sessions/{id}/end accepts and returns the record
//   - GET  /v1/vault/local/status reports unlocked
//
// Tests that need a different fake (e.g., to assert the request body
// or simulate a locked vault) can set AILERON_API_URL themselves to
// override the package-wide one.
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
	os.Exit(m.Run())
}
