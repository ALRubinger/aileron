package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// promptApproval contract:
//   - AILERON_APPROVAL_URL or AILERON_SESSION_ID unset → deny.
//   - Daemon returns 200 + {decision: allow_once|deny|allow_project|allow_user}
//     → mapped to the matching responseXxx constant.
//   - Daemon unreachable, non-200, or malformed body → deny (fail closed).

func TestPromptApproval_NoEnvFailsClosed(t *testing.T) {
	t.Setenv("AILERON_APPROVAL_URL", "")
	t.Setenv("AILERON_SESSION_ID", "")
	if got := promptApproval("ls", "ask"); got != responseDeny {
		t.Errorf("got %v, want responseDeny when env is unset", got)
	}
}

func TestPromptApproval_OnlyURL_FailsClosed(t *testing.T) {
	t.Setenv("AILERON_APPROVAL_URL", "http://x:1")
	t.Setenv("AILERON_SESSION_ID", "")
	if got := promptApproval("ls", "ask"); got != responseDeny {
		t.Errorf("missing session id should fail closed; got %v", got)
	}
}

func TestPromptApproval_DecisionMapping(t *testing.T) {
	cases := []struct {
		decision string
		want     approvalResponse
	}{
		{"allow_once", responseAllowOnce},
		{"deny", responseDeny},
		{"allow_project", responseAllowProject},
		{"allow_user", responseAllowUser},
		{"unknown_value", responseDeny},
	}
	for _, tc := range cases {
		t.Run(tc.decision, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/approvals/shell") {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"decision": tc.decision})
			}))
			defer srv.Close()

			t.Setenv("AILERON_APPROVAL_URL", srv.URL)
			t.Setenv("AILERON_SESSION_ID", "sess-x")

			if got := promptApproval("git push", "ask"); got != tc.want {
				t.Errorf("decision=%q: got %v, want %v", tc.decision, got, tc.want)
			}
		})
	}
}

func TestPromptApproval_RequestBodyShape(t *testing.T) {
	got := make(chan map[string]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body
		_ = json.NewEncoder(w).Encode(map[string]string{"decision": "deny"})
	}))
	defer srv.Close()

	t.Setenv("AILERON_APPROVAL_URL", srv.URL)
	t.Setenv("AILERON_SESSION_ID", "sess-42")

	_ = promptApproval("git push", "policy says ask")

	body := <-got
	if body["command"] != "git push" {
		t.Errorf("command in body = %q, want git push", body["command"])
	}
	if body["reason"] != "policy says ask" {
		t.Errorf("reason in body = %q, want policy says ask", body["reason"])
	}
	if body["cwd"] == "" {
		t.Errorf("cwd should be populated, got empty")
	}
}

func TestPromptApproval_UnreachableServerFailsClosed(t *testing.T) {
	t.Setenv("AILERON_APPROVAL_URL", "http://127.0.0.1:1") // privileged port; nothing listens
	t.Setenv("AILERON_SESSION_ID", "sess-x")
	if got := promptApproval("ls", "ask"); got != responseDeny {
		t.Errorf("unreachable server should deny; got %v", got)
	}
}

func TestPromptApproval_Non200FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("AILERON_APPROVAL_URL", srv.URL)
	t.Setenv("AILERON_SESSION_ID", "sess-x")
	if got := promptApproval("ls", "ask"); got != responseDeny {
		t.Errorf("500 should deny; got %v", got)
	}
}

func TestPromptApproval_GarbageBodyFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{not json")
	}))
	defer srv.Close()
	t.Setenv("AILERON_APPROVAL_URL", srv.URL)
	t.Setenv("AILERON_SESSION_ID", "sess-x")
	if got := promptApproval("ls", "ask"); got != responseDeny {
		t.Errorf("garbage body should deny; got %v", got)
	}
}
