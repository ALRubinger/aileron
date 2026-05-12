package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setBindingBase(t *testing.T, base string) {
	t.Helper()
	orig := bindingAPIBaseURL
	bindingAPIBaseURL = func() (string, error) { return base, nil }
	t.Cleanup(func() { bindingAPIBaseURL = orig })
}

func newActionsFakeDaemon(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// --- list ---

func TestRunActionList_RendersTableWithEnabledColumn(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/actions" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		enabled := true
		disabled := false
		_ = json.NewEncoder(w).Encode(actionListWireResponse{
			Items: []actionListItem{
				{Name: "ship-update", Version: "1.0.0", Source: "hub://aileron/ship-update@1.0.0", Enabled: &enabled},
				{Name: "list-emails", Version: "2.1.0", Source: "hub://aileron/list-emails@2.1.0", Enabled: &disabled},
			},
		})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runActionList(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ship-update") || !strings.Contains(out, "1.0.0") {
		t.Errorf("missing enabled row: %s", out)
	}
	if !strings.Contains(out, "list-emails") || !strings.Contains(out, "false") {
		t.Errorf("missing disabled row: %s", out)
	}
	if !strings.Contains(out, "Restart your MCP server") {
		t.Errorf("expected disabled-count restart hint when any action is off; got: %s", out)
	}
}

func TestRunActionList_EmptyShowsHint(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(actionListWireResponse{Items: nil})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	if code := runActionList(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No actions installed") {
		t.Errorf("missing empty-state hint: %s", stdout.String())
	}
}

func TestRunActionList_JSONMode(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		enabled := false
		_ = json.NewEncoder(w).Encode(actionListWireResponse{
			Items: []actionListItem{
				{Name: "x", Version: "0.1.0", Source: "hub://x@0.1.0", Enabled: &enabled},
			},
		})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	if code := runActionList([]string{"--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	// NDJSON: one JSON object per line.
	line := strings.TrimSpace(stdout.String())
	var got actionListItem
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("not NDJSON: %v, body=%s", err, line)
	}
	if got.Name != "x" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Enabled == nil || *got.Enabled {
		t.Errorf("Enabled = %v, want false", got.Enabled)
	}
}

func TestRunActionList_DaemonErrorPropagates(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	if code := runActionList(nil, &stdout, &stderr); code == 0 {
		t.Errorf("expected non-zero exit on 500")
	}
	if !strings.Contains(stderr.String(), "500") {
		t.Errorf("stderr missing status: %s", stderr.String())
	}
}

// --- enable / disable ---

func TestRunActionToggle_EnableSendsPatch(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		enabled := true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "ship-update", "version": "1.0.0", "source": "x",
			"enabled": &enabled,
		})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runActionToggle([]string{"ship-update"}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("Method = %q", gotMethod)
	}
	if gotPath != "/v1/actions/ship-update" {
		t.Errorf("Path = %q", gotPath)
	}
	var parsed struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !parsed.Enabled {
		t.Errorf("Enabled in PATCH body = false, want true")
	}
	if !strings.Contains(stdout.String(), "Enabled") || !strings.Contains(stdout.String(), "ship-update") {
		t.Errorf("stdout missing confirmation: %s", stdout.String())
	}
}

func TestRunActionToggle_DisableSendsFalse(t *testing.T) {
	var gotBody []byte
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "x"})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runActionToggle([]string{"ship-update"}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	var parsed struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if parsed.Enabled {
		t.Errorf("Enabled in PATCH body = true, want false")
	}
	if !strings.Contains(stdout.String(), "Disabled") {
		t.Errorf("stdout missing 'Disabled': %s", stdout.String())
	}
}

func TestRunActionToggle_NotFound(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runActionToggle([]string{"no-such"}, false, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit for 404")
	}
	if !strings.Contains(stderr.String(), "not installed") {
		t.Errorf("stderr missing 'not installed' hint: %s", stderr.String())
	}
}

func TestRunActionToggle_RequiresName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runActionToggle(nil, true, &stdout, &stderr); code == 0 {
		t.Errorf("expected non-zero exit when no name supplied")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr missing usage hint: %s", stderr.String())
	}
}

func TestRunActionToggle_BlankNameRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runActionToggle([]string{"   "}, true, &stdout, &stderr); code == 0 {
		t.Errorf("expected non-zero exit for whitespace-only name")
	}
}

func TestRunActionList_BaseURLError(t *testing.T) {
	orig := bindingAPIBaseURL
	bindingAPIBaseURL = func() (string, error) { return "", fmt.Errorf("spawn boom") }
	t.Cleanup(func() { bindingAPIBaseURL = orig })

	var stdout, stderr bytes.Buffer
	if code := runActionList(nil, &stdout, &stderr); code == 0 {
		t.Errorf("expected non-zero exit when baseURL fails")
	}
	if !strings.Contains(stderr.String(), "spawn boom") {
		t.Errorf("stderr missing base-URL error: %s", stderr.String())
	}
}

func TestRunActionList_TransportError(t *testing.T) {
	// Point at an address that will refuse the connection so the HTTP
	// transport fails before the daemon has a chance to respond.
	setBindingBase(t, "http://127.0.0.1:1")
	var stdout, stderr bytes.Buffer
	if code := runActionList(nil, &stdout, &stderr); code == 0 {
		t.Errorf("expected non-zero exit on transport failure")
	}
}

func TestRunActionList_MalformedJSONResponse(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not-json"))
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	if code := runActionList(nil, &stdout, &stderr); code == 0 {
		t.Errorf("expected non-zero exit when daemon returns garbage")
	}
	if !strings.Contains(stderr.String(), "decoding response") {
		t.Errorf("stderr missing decode-error hint: %s", stderr.String())
	}
}

func TestRunActionList_EmptyJSONMode(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(actionListWireResponse{Items: nil})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	if code := runActionList([]string{"--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Errorf("expected [] for empty JSON, got: %q", stdout.String())
	}
}

func TestRunActionToggle_BaseURLError(t *testing.T) {
	orig := bindingAPIBaseURL
	bindingAPIBaseURL = func() (string, error) { return "", fmt.Errorf("spawn boom") }
	t.Cleanup(func() { bindingAPIBaseURL = orig })

	var stdout, stderr bytes.Buffer
	if code := runActionToggle([]string{"x"}, true, &stdout, &stderr); code == 0 {
		t.Errorf("expected non-zero exit when baseURL fails")
	}
}

func TestRunActionToggle_TransportError(t *testing.T) {
	setBindingBase(t, "http://127.0.0.1:1")
	var stdout, stderr bytes.Buffer
	if code := runActionToggle([]string{"x"}, false, &stdout, &stderr); code == 0 {
		t.Errorf("expected non-zero exit on transport failure")
	}
}

// --- dispatcher (cmd/aileron/main.go runAction switch) ---

func TestRunAction_DispatchesList(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(actionListWireResponse{Items: nil})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runAction([]string{"list"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No actions installed") {
		t.Errorf("list dispatch did not call runActionList: %s", stdout.String())
	}
}

func TestRunAction_DispatchesEnable(t *testing.T) {
	var gotBody []byte
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "x"})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runAction([]string{"enable", "ship-update"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	var parsed struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !parsed.Enabled {
		t.Errorf("enable dispatch sent enabled=false")
	}
}

func TestRunAction_DispatchesDisable(t *testing.T) {
	var gotBody []byte
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "x"})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runAction([]string{"disable", "ship-update"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	var parsed struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if parsed.Enabled {
		t.Errorf("disable dispatch sent enabled=true")
	}
}

func TestRunAction_NoArgsShowsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runAction(nil, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Errorf("expected non-zero exit when no subcommand supplied")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr missing usage: %s", stderr.String())
	}
}

func TestRunAction_UnknownSubcommand_State(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runAction([]string{"frobnicate"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Errorf("expected non-zero exit for unknown subcommand")
	}
	if !strings.Contains(stderr.String(), "unknown action command") {
		t.Errorf("stderr missing unknown-command hint: %s", stderr.String())
	}
}
