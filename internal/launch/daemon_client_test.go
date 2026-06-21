package launch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// daemon_client.go contract:
//   - RegisterSession returns the daemon-minted ULID and caveat token on 200.
//   - RegisterSession surfaces a typed status error on non-200, with
//     the daemon's `error.code` / `error.message` from the body.
//   - EndSession is a fire-and-forget round-trip on 200; non-200
//     returns a status error.
//   - LocalVaultLocked returns (locked, true) on a healthy probe and
//     (_, false) when the daemon is unreachable or returns non-200.
//   - trimTrailingSlash strips one or more trailing slashes.

func TestDaemonClient_RegisterSession_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sessions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "01HK00000000000000000XYZAB",
			"started_at":   "2026-05-06T00:00:00Z",
			"agent":        "claude",
			"working_dir":  "/work",
			"caveat_token": "caveat.jwt.value",
		})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	reg, err := c.RegisterSession(context.Background(), "claude", "/work")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if reg.ID != "01HK00000000000000000XYZAB" {
		t.Errorf("got id %q, want from server", reg.ID)
	}
	if reg.CaveatToken != "caveat.jwt.value" {
		t.Errorf("got caveat token %q, want from server", reg.CaveatToken)
	}
}

func TestDaemonClient_SendsBearerToken(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "01HK00000000000000000XYZAB"})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL, "tok_launch")
	if _, err := c.RegisterSession(context.Background(), "claude", "/work"); err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if got := <-seen; got != "Bearer tok_launch" {
		t.Fatalf("Authorization = %q, want bearer token", got)
	}
}

func TestDaemonClient_RegisterSession_NonOK_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "sessions_disabled",
				"message": "daemon is not configured with a sessions store",
			},
		})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, err := c.RegisterSession(context.Background(), "claude", "/work")
	if err == nil {
		t.Fatal("RegisterSession should fail on 503")
	}
	for _, want := range []string{"503", "sessions_disabled", "sessions store"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestDaemonClient_RegisterSession_EmptyID_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": ""})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, err := c.RegisterSession(context.Background(), "claude", "/work")
	if err == nil {
		t.Fatal("expected error when daemon returns empty id")
	}
}

func TestDaemonClient_RegisterSession_GarbageBody_ReturnsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, err := c.RegisterSession(context.Background(), "claude", "/work")
	if err == nil {
		t.Fatal("expected decode error")
	}
}

func TestDaemonClient_EndSession_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("got %s, want POST", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/end") {
			t.Errorf("path %q should end with /end", r.URL.Path)
		}
		var body struct {
			ExitCode *int `json:"exit_code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ExitCode == nil || *body.ExitCode != 0 {
			t.Errorf("got exit_code %v, want 0", body.ExitCode)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x"})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	exit := 0
	if err := c.EndSession(context.Background(), "01HK0000000000000000000001", &exit); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
}

func TestDaemonClient_EndSession_Orphaned_NilExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["exit_code"] != nil {
			t.Errorf("expected null exit_code, got %v", body["exit_code"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x"})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	if err := c.EndSession(context.Background(), "01HK0000000000000000000001", nil); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
}

func TestDaemonClient_EndSession_NonOK_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	exit := 0
	err := c.EndSession(context.Background(), "id", &exit)
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q should mention status 500", err)
	}
}

func TestDaemonClient_LocalVaultLocked_LockedTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"locked": true})
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	locked, ok := c.LocalVaultLocked(context.Background())
	if !ok {
		t.Fatal("ok should be true on 200")
	}
	if !locked {
		t.Error("locked should be true")
	}
}

func TestDaemonClient_LocalVaultLocked_UnreachableReturnsNotOK(t *testing.T) {
	c := newDaemonClient("http://127.0.0.1:1") // privileged port; nothing listens
	_, ok := c.LocalVaultLocked(context.Background())
	if ok {
		t.Fatal("ok should be false when daemon is unreachable")
	}
}

func TestDaemonClient_LocalVaultLocked_Non200ReturnsNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, ok := c.LocalVaultLocked(context.Background())
	if ok {
		t.Fatal("ok should be false on 404")
	}
}

func TestDaemonClient_LocalVaultLocked_GarbageBodyReturnsNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	c := newDaemonClient(srv.URL)
	_, ok := c.LocalVaultLocked(context.Background())
	if ok {
		t.Fatal("ok should be false when body is malformed")
	}
}

func TestTrimTrailingSlash(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"http://x":    "http://x",
		"http://x/":   "http://x",
		"http://x///": "http://x",
		"/":           "/", // single-char "/" is preserved
		"/v1":         "/v1",
		"/v1/":        "/v1",
	}
	for in, want := range cases {
		if got := trimTrailingSlash(in); got != want {
			t.Errorf("trimTrailingSlash(%q) = %q, want %q", in, got, want)
		}
	}
}
