package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func ptrString(s string) *string { return &s }

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
	if !strings.Contains(out, "ORIGIN") {
		t.Errorf("missing ORIGIN column header: %s", out)
	}
	if !strings.Contains(out, "HUB") {
		t.Errorf("missing HUB badge for hub:// sources: %s", out)
	}
	if !strings.Contains(out, "Restart your MCP server") {
		t.Errorf("expected disabled-count restart hint when any action is off; got: %s", out)
	}
}

func TestOriginBadge_ClassifiesSources(t *testing.T) {
	cases := map[string]string{
		"hub://aileron/ship-update@1.0.0": "HUB",
		"hub://aileron/list@2.0.0":        "HUB",
		"":                                "—",
		"some-arbitrary-string":           "—",
		"https://example.com/foo":         "—",
	}
	for source, want := range cases {
		if got := originBadge(source); got != want {
			t.Errorf("originBadge(%q) = %q, want %q", source, got, want)
		}
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

// TestRunActionList_SurfacesLoadErrorsOnStderr pins the visibility
// fix for the dash-bug class of failures. Pre-fix, the daemon's
// load_errors field was decoded into `_` and never rendered, so a
// wrap-emitted action.md with a dashed input name (validation_error
// from internal/action/validate.go's `^[a-z][a-z0-9_]*$` regex)
// vanished from the table view with no indication anything was
// wrong. The fix prints a structured summary to stderr that names
// the failure class, the file, and the validator message so users
// can act on it without curl detours.
func TestRunActionList_SurfacesLoadErrorsOnStderr(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(actionListWireResponse{
			Items: []actionListItem{
				{Name: "good-action", Version: "1.0.0", Source: "hub://x/y@1"},
			},
			LoadErrors: []actionLoadErrorItem{
				{
					Class:   "validation_error",
					File:    "/Users/x/.aileron/actions/linear-foo.md",
					Message: "inputs[1].name \"data-source\" must match ^[a-z][a-z0-9_]*$",
				},
				{
					Class:   "parse_error",
					File:    "/Users/x/.aileron/actions/broken.md",
					Line:    7,
					Message: "expected `+++` frontmatter delimiter",
				},
			},
		})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	if code := runActionList(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "good-action") {
		t.Errorf("stdout missing valid row: %s", stdout.String())
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "2 action file(s) failed to load") {
		t.Errorf("stderr missing failure count: %s", errOut)
	}
	if !strings.Contains(errOut, "validation_error") || !strings.Contains(errOut, "data-source") {
		t.Errorf("stderr missing first failure detail: %s", errOut)
	}
	if !strings.Contains(errOut, "parse_error") || !strings.Contains(errOut, "broken.md:7") {
		t.Errorf("stderr missing line-numbered failure: %s", errOut)
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

// --- run ---

func TestRunActionRun_ArgFlagsSendArgsAndRenderResult(t *testing.T) {
	var gotPath, gotMethod, gotContentType string
	var gotBody []byte
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(actionRunResponse{
			AuditID: "audit-123",
			Result:  ptrString(`{"ok":true}`),
		})
	})
	setBindingBase(t, base)

	auditPath := filepath.Join(t.TempDir(), "audit-id")
	var stdout, stderr bytes.Buffer
	code := runActionRun([]string{"--arg", "team=ENG", "--arg", "title=Smoke test", "--audit-id-out", auditPath, "linear-issues-create"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q", gotMethod)
	}
	if gotPath != "/v1/actions/linear-issues-create/run" {
		t.Errorf("path = %q", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}
	var parsed actionRunRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if parsed.Args["team"] != "ENG" || parsed.Args["title"] != "Smoke test" {
		t.Errorf("args = %#v", parsed.Args)
	}
	if !strings.Contains(stdout.String(), "wrapped output:") || !strings.Contains(stdout.String(), `{"ok":true}`) {
		t.Errorf("stdout missing rendered result: %s", stdout.String())
	}
	written, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit id: %v", err)
	}
	if string(written) != "audit-123\n" {
		t.Errorf("audit id file = %q", string(written))
	}
}

func TestRunActionRun_RawArgsJSONPassthrough(t *testing.T) {
	var gotBody []byte
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(actionRunResponse{AuditID: "audit-raw"})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runActionRun([]string{"--args", `{"labels":["bug","triage"],"priority":2}`, "linear-issues-create"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	var parsed actionRunRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	labels, ok := parsed.Args["labels"].([]any)
	if !ok || len(labels) != 2 || labels[0] != "bug" || labels[1] != "triage" {
		t.Errorf("labels = %#v", parsed.Args["labels"])
	}
	if parsed.Args["priority"] != float64(2) {
		t.Errorf("priority = %#v", parsed.Args["priority"])
	}
}

func TestRunActionRun_JSONModePrintsRawEnvelope(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"audit_id":"audit-json","result":"ok"}`)
	})
	setBindingBase(t, base)

	auditPath := filepath.Join(t.TempDir(), "audit-id")
	var stdout, stderr bytes.Buffer
	code := runActionRun([]string{"--json", "--audit-id-out", auditPath, "ship-update"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != `{"audit_id":"audit-json","result":"ok"}` {
		t.Errorf("stdout = %q", stdout.String())
	}
	written, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit id: %v", err)
	}
	if string(written) != "audit-json\n" {
		t.Errorf("audit id file = %q", string(written))
	}
}

func TestRunActionRun_PendingApprovalReturnsTempFail(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(actionRunPendingResponse{
			Status:     "pending_approval",
			ApprovalID: "act-123",
			ReviewURL:  "http://127.0.0.1:50419/approvals?focus=act-123",
		})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runActionRun([]string{"send-draft"}, &stdout, &stderr)
	if code != 75 {
		t.Fatalf("exit = %d, want 75; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "approval pending") ||
		!strings.Contains(stderr.String(), "http://127.0.0.1:50419/approvals?focus=act-123") ||
		!strings.Contains(stderr.String(), "aileron approval approve act-123") {
		t.Errorf("stderr missing approval guidance: %s", stderr.String())
	}
}

func TestRunActionRun_SurfacesStructuredErrorClass(t *testing.T) {
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `{"error":{"class":"binding_required","message":"vault is locked","boundary":"runtime","retriable":true}}`)
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runActionRun([]string{"ship-update"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(stderr.String(), "binding_required: vault is locked") {
		t.Errorf("stderr missing structured class: %s", stderr.String())
	}
}

func TestRunActionRun_RejectsInvalidArgs(t *testing.T) {
	cases := [][]string{
		{"--arg", "missing-equals", "ship-update"},
		{"--arg", "x=y", "--args", `{"x":"z"}`, "ship-update"},
		{"--args", `[1,2]`, "ship-update"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runActionRun(args, &stdout, &stderr); code == 0 {
				t.Fatalf("expected non-zero exit for args %v", args)
			}
		})
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

func TestRunAction_DispatchesRun(t *testing.T) {
	var gotPath string
	base := newActionsFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(actionRunResponse{AuditID: "audit-run"})
	})
	setBindingBase(t, base)

	var stdout, stderr bytes.Buffer
	code := runAction([]string{"run", "ship-update"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if gotPath != "/v1/actions/ship-update/run" {
		t.Errorf("run dispatch path = %q", gotPath)
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
