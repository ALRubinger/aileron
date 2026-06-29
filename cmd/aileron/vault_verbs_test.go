package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vault put/delete/list verbs are thin daemon-backed HTTP clients
// scoped to the agents/<name>/oauth namespace (ADR-0025, #981). These
// tests pin the contract:
//
//   - put reads --from-file bytes verbatim (no newline munging) and
//     PUTs them base64-encoded to /v1/vault/agents/<name>/credentials.
//   - put/delete reject any non-agents/<name>/oauth path before any HTTP.
//   - delete confirms interactively unless --yes; n/empty cancels with
//     no HTTP call; 404 surfaces a clear message and non-zero exit.
//   - list mirrors `secret list` output (names by default, NDJSON with
//     --json), rejects non-agents/ prefixes, and reports empty.

// fakeVaultServer stands up an httptest server scoped to AILERON_API_URL
// so vaultDoRequest resolves to the fake.
func fakeVaultServer(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/v1", fn))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")
	t.Setenv("AILERON_TOKEN", "test-token")
	return srv
}

func TestRunVaultPut_RoundTripsFileBytesVerbatim(t *testing.T) {
	// Credential files are exact-byte artifacts: a trailing newline in
	// the file must reach the daemon unchanged.
	fileBytes := []byte("{\"claudeAiOauth\":{\"accessToken\":\"tok\"}}\n")
	var received []byte
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/vault/agents/claude/credentials" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body agentCredentialsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received = body.Value
		w.WriteHeader(http.StatusNoContent)
	})

	dir := t.TempDir()
	credFile := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(credFile, fileBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "agents/claude/oauth", "--from-file", credFile},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !bytes.Equal(received, fileBytes) {
		t.Errorf("server received %q, want %q (verbatim)", received, fileBytes)
	}
	if !strings.Contains(stdout.String(), "Stored agents/claude/oauth") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunVaultPut_RejectsNonAgentPathBeforeHTTP(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	dir := t.TempDir()
	credFile := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(credFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "some/other/path", "--from-file", credFile},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for non-agent path")
	}
	if called {
		t.Errorf("HTTP request issued for a rejected path")
	}
	if !strings.Contains(stderr.String(), "agents/<name>/<purpose>") {
		t.Errorf("stderr = %q; want agents-only constraint message", stderr.String())
	}
}

func TestRunVaultDelete_YesSkipsPromptAndDeletes(t *testing.T) {
	gotDelete := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/codex/credentials" {
			gotDelete = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/codex/oauth", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !gotDelete {
		t.Errorf("DELETE not issued")
	}
	if !strings.Contains(stdout.String(), "Deleted agents/codex/oauth") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// #1317 flips the canonical form: `vault list` now prints the
// fully-qualified agents/<name>/oauth, and delete accepts ONLY that form.
// Regression for the flip — the bare agent name (what #1302/#1310
// accepted) is now rejected before any HTTP, while the fully-qualified
// line `list` emits round-trips straight into delete.
func TestRunVaultDelete_AcceptsFullyQualifiedFromList(t *testing.T) {
	const bare = "codex"                       // the pre-#1317 form, now rejected
	const listed = "agents/" + bare + "/oauth" // exactly what `vault list` prints

	// The bare name is rejected client-side, no DELETE issued.
	bareCalled := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		bareCalled = true
		w.WriteHeader(http.StatusNoContent)
	})
	var bstdout, bstderr bytes.Buffer
	if code := runVault([]string{"delete", bare, "--yes"},
		strings.NewReader(""), &bstdout, &bstderr); code == 0 {
		t.Errorf("bare name %q: exit = 0, want non-zero (bare form no longer accepted)", bare)
	}
	if bareCalled {
		t.Errorf("bare name %q: HTTP issued for rejected form", bare)
	}

	// The fully-qualified line from `list` round-trips into delete.
	gotDelete := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/"+bare+"/credentials" {
			gotDelete = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", listed, "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !gotDelete {
		t.Errorf("DELETE not issued for fully-qualified %q", listed)
	}
	if !strings.Contains(stdout.String(), "Deleted agents/"+bare+"/oauth") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// The list-output == delete-input contract (#1317): every line `vault
// list` emits for an agent must resolve, via the delete validator, back
// to the same agent the daemon keys the DELETE on. list now prints the
// fully-qualified agents/<name>/oauth, so feed exactly that. The bare
// name (the pre-#1317 form) must no longer validate.
func TestVaultListNamesRoundTripIntoDelete(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "my-agent"} {
		// The fully-qualified line list prints is the sole accepted form.
		listed := "agents/" + agent + "/oauth"
		name, purpose, err := agentPathNameAndPurpose(listed)
		if err != nil {
			t.Errorf("agentPathNameAndPurpose(%q) errored: %v; a line from `vault list` must be deletable", listed, err)
			continue
		}
		if name != agent {
			t.Errorf("agentPathNameAndPurpose(%q) name = %q, want %q", listed, name, agent)
		}
		if purpose != "oauth" {
			t.Errorf("agentPathNameAndPurpose(%q) purpose = %q, want oauth", listed, purpose)
		}
		// The bare name is no longer a valid delete input.
		if gotName, gotPurpose, err := agentPathNameAndPurpose(agent); err == nil {
			t.Errorf("agentPathNameAndPurpose(%q) = (%q, %q, nil), want error (bare form no longer accepted)", agent, gotName, gotPurpose)
		}
	}
}

// vault delete now dispatches every namespace vault list prints, so the
// only client-side rejections are paths in no deletable namespace: an
// unrecognized `other` shape. (user/<service> and binding paths now
// dispatch — see the dedicated dispatch tests.) These must still be
// rejected before any HTTP call so a typo never silently hits the daemon.
func TestRunVaultDelete_RejectsUnrecognizedNamespace(t *testing.T) {
	// internal/secret: two segments, not user/ — classifies as `other`.
	// _underscore/a/b: IsBindingPath rejects a leading-underscore kind.
	for _, arg := range []string{"internal/secret", "foobar", "_underscore/a/b"} {
		called := false
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"delete", arg, "--yes"},
			strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("arg %q: exit = 0, want non-zero", arg)
		}
		if called {
			t.Errorf("arg %q: HTTP issued for rejected name", arg)
		}
	}
}

// Guard the namespace lock for the full-path form: a value that carries
// the agents/ prefix and /oauth suffix but extracts to an empty or
// slash-bearing <name> (e.g. `agents//oauth`, `agents/a/b/oauth`) must
// still be rejected, so no nested or empty key can slip through the path
// form (ADR-0025).
func TestAgentPathNameAndPurpose_RejectsMalformedFullPath(t *testing.T) {
	for _, arg := range []string{"agents//oauth", "agents/a/b/oauth", "agents/claude/", "agents/claude"} {
		if name, purpose, err := agentPathNameAndPurpose(arg); err == nil {
			t.Errorf("agentPathNameAndPurpose(%q) = (%q, %q, nil), want error", arg, name, purpose)
		}
	}
}

// The purpose segment is constrained to the daemon's allow-list
// (^[a-z0-9][a-z0-9_-]*$). A path whose purpose violates it must be
// rejected client-side, before any HTTP call.
func TestAgentPathNameAndPurpose_RejectsInvalidPurpose(t *testing.T) {
	for _, arg := range []string{"agents/claude/OAUTH", "agents/claude/-bad", "agents/claude/has space", "agents/claude/api.key"} {
		if name, purpose, err := agentPathNameAndPurpose(arg); err == nil {
			t.Errorf("agentPathNameAndPurpose(%q) = (%q, %q, nil), want error (invalid purpose)", arg, name, purpose)
		}
	}
}

// The apikey purpose (the case #1361 made addressable) parses to its
// name and purpose so it can be put/deleted just like oauth.
func TestAgentPathNameAndPurpose_AcceptsApikey(t *testing.T) {
	name, purpose, err := agentPathNameAndPurpose("agents/claude/apikey")
	if err != nil {
		t.Fatalf("agentPathNameAndPurpose(agents/claude/apikey) errored: %v", err)
	}
	if name != "claude" || purpose != "apikey" {
		t.Errorf("got (%q, %q), want (claude, apikey)", name, purpose)
	}
}

func TestRunVaultDelete_InteractiveCancelMakesNoCall(t *testing.T) {
	for _, answer := range []string{"n\n", "\n"} {
		called := false
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"delete", "agents/claude/oauth"},
			strings.NewReader(answer), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("answer %q: exit = %d, want 0", answer, code)
		}
		if called {
			t.Errorf("answer %q: DELETE issued despite cancel", answer)
		}
		if !strings.Contains(stdout.String(), "cancelled") {
			t.Errorf("answer %q: stdout = %q, want cancelled", answer, stdout.String())
		}
	}
}

func TestRunVaultDelete_InteractiveYesDeletes(t *testing.T) {
	gotDelete := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			gotDelete = true
		}
		w.WriteHeader(http.StatusNoContent)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/claude/oauth"},
		strings.NewReader("y\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !gotDelete {
		t.Errorf("DELETE not issued on 'y'")
	}
}

func TestRunVaultDelete_NotFoundMessage(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"vault_not_found","message":"nope"}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/claude/oauth", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for 404")
	}
	if !strings.Contains(stderr.String(), "no credential entry for agents/claude/oauth") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunVaultList_DefaultAndJSON(t *testing.T) {
	listBody := `{"agents":[{"name":"claude","metadata":{"type":"oauth_refresh_token"}},{"name":"codex"}]}`
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vault/agents" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, listBody)
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "agent"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Fields(stdout.String())
	if len(lines) != 2 || lines[0] != "agents/claude/oauth" || lines[1] != "agents/codex/oauth" {
		t.Errorf("default output = %q, want fully-qualified agents/<name>/oauth one per line", stdout.String())
	}

	var jout, jerr bytes.Buffer
	code = runVault([]string{"list", "--scope", "agent", "--json"}, strings.NewReader(""), &jout, &jerr)
	if code != 0 {
		t.Fatalf("json exit = %d, want 0; stderr=%s", code, jerr.String())
	}
	jsonLines := strings.Split(strings.TrimSpace(jout.String()), "\n")
	if len(jsonLines) != 2 {
		t.Fatalf("json output = %q, want 2 NDJSON lines", jout.String())
	}
	var first agentSummary
	if err := json.Unmarshal([]byte(jsonLines[0]), &first); err != nil {
		t.Fatalf("NDJSON line 0 not valid JSON: %v", err)
	}
	if first.Name != "claude" {
		t.Errorf("first NDJSON name = %q, want claude", first.Name)
	}
}

// #1361: the daemon is purpose-aware and emits one AgentCredentialSummary
// per (name, purpose). An apikey-only agent must surface in `vault list`
// with its real purpose, not be collapsed to /oauth or dropped.
func TestRunVaultList_ApikeyOnlyAgentSurfaces(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agents":[{"name":"claude","purpose":"apikey","metadata":{"type":"api_key"}}]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "agent"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "agents/claude/apikey" {
		t.Errorf("stdout = %q, want agents/claude/apikey (apikey-only agent must not be mislabeled oauth)", got)
	}
}

// #1361: an agent that holds both oauth and apikey yields two distinct
// list lines (no duplicate, no collapse) and two NDJSON entries that each
// carry their purpose.
func TestRunVaultList_BothPurposesYieldTwoDistinctEntries(t *testing.T) {
	const body = `{"agents":[{"name":"claude","purpose":"oauth"},{"name":"claude","purpose":"apikey"}]}`
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "agent"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Fields(stdout.String())
	if len(lines) != 2 || lines[0] != "agents/claude/oauth" || lines[1] != "agents/claude/apikey" {
		t.Errorf("text output = %q, want two distinct lines agents/claude/oauth and agents/claude/apikey", stdout.String())
	}

	var jout, jerr bytes.Buffer
	code = runVault([]string{"list", "--scope", "agent", "--json"}, strings.NewReader(""), &jout, &jerr)
	if code != 0 {
		t.Fatalf("json exit = %d, want 0; stderr=%s", code, jerr.String())
	}
	jsonLines := strings.Split(strings.TrimSpace(jout.String()), "\n")
	if len(jsonLines) != 2 {
		t.Fatalf("json output = %q, want 2 NDJSON lines", jout.String())
	}
	gotPurposes := map[string]bool{}
	for _, line := range jsonLines {
		var s agentSummary
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("NDJSON line not valid: %v (%q)", err, line)
		}
		if s.Name != "claude" {
			t.Errorf("NDJSON name = %q, want claude", s.Name)
		}
		if s.Purpose == nil {
			t.Fatalf("NDJSON entry %q dropped purpose; --json must carry it", line)
		}
		gotPurposes[*s.Purpose] = true
	}
	if !gotPurposes["oauth"] || !gotPurposes["apikey"] {
		t.Errorf("NDJSON purposes = %v, want both oauth and apikey", gotPurposes)
	}
}

// #1361: `vault delete agents/<name>/apikey` must issue the DELETE with
// ?purpose=apikey so the daemon removes the apikey envelope, not oauth.
func TestRunVaultDelete_ThreadsApikeyPurpose(t *testing.T) {
	var gotPurpose string
	gotDelete := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/claude/credentials" {
			gotDelete = true
			gotPurpose = r.URL.Query().Get("purpose")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/claude/apikey", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !gotDelete {
		t.Fatalf("DELETE not issued")
	}
	if gotPurpose != "apikey" {
		t.Errorf("DELETE ?purpose = %q, want apikey", gotPurpose)
	}
	if !strings.Contains(stdout.String(), "Deleted agents/claude/apikey") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// #1361: the oauth path keeps threading ?purpose=oauth — backward-compat
// for the historical form (the daemon defaults to oauth, the CLI is
// explicit).
func TestRunVaultDelete_ThreadsOauthPurpose(t *testing.T) {
	var gotPurpose string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/claude/credentials" {
			gotPurpose = r.URL.Query().Get("purpose")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/claude/oauth", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotPurpose != "oauth" {
		t.Errorf("DELETE ?purpose = %q, want oauth", gotPurpose)
	}
}

// #1361: put threads the purpose into ?purpose= as well, so storing
// agents/<name>/apikey writes the apikey envelope.
func TestRunVaultPut_ThreadsApikeyPurpose(t *testing.T) {
	var gotPurpose string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/vault/agents/claude/credentials" {
			gotPurpose = r.URL.Query().Get("purpose")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
	})
	dir := t.TempDir()
	credFile := filepath.Join(dir, "creds")
	if err := os.WriteFile(credFile, []byte("sk-xxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "agents/claude/apikey", "--from-file", credFile},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotPurpose != "apikey" {
		t.Errorf("PUT ?purpose = %q, want apikey", gotPurpose)
	}
	if !strings.Contains(stdout.String(), "Stored agents/claude/apikey") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// #1361 end-to-end: the apikey line `vault list` prints feeds straight
// back into `vault delete`, issuing the DELETE the daemon keys on the
// same (name, purpose).
func TestRunVaultList_ApikeyLineRoundTripsIntoDelete(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agents":[{"name":"claude","purpose":"apikey"}]}`)
	})
	var lout, lerr bytes.Buffer
	if code := runVault([]string{"list", "--scope", "agent"}, strings.NewReader(""), &lout, &lerr); code != 0 {
		t.Fatalf("list exit = %d, want 0; stderr=%s", code, lerr.String())
	}
	listed := strings.TrimSpace(lout.String())
	if listed != "agents/claude/apikey" {
		t.Fatalf("listed = %q, want agents/claude/apikey", listed)
	}

	var gotPurpose string
	gotDelete := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/vault/agents/claude/credentials" {
			gotDelete = true
			gotPurpose = r.URL.Query().Get("purpose")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
	})
	var dout, derr bytes.Buffer
	if code := runVault([]string{"delete", listed, "--yes"}, strings.NewReader(""), &dout, &derr); code != 0 {
		t.Fatalf("delete exit = %d, want 0; stderr=%s", code, derr.String())
	}
	if !gotDelete || gotPurpose != "apikey" {
		t.Errorf("round-trip delete: gotDelete=%v purpose=%q, want true/apikey", gotDelete, gotPurpose)
	}
}

// #1361 backward-compat: an older daemon that omits `purpose` (entries
// predating per-purpose listing) must still render as agents/<name>/oauth.
func TestRunVaultList_NilPurposeRendersOauth(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agents":[{"name":"codex"}]}`)
	})
	var stdout, stderr bytes.Buffer
	if code := runVault([]string{"list", "--scope", "agent"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "agents/codex/oauth" {
		t.Errorf("stdout = %q, want agents/codex/oauth (nil purpose -> oauth)", got)
	}
}

func TestRunVaultList_EmptyMessage(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agents":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "agent"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No agent credentials stored.") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunVaultList_EmptyJSONIsEmptyArray(t *testing.T) {
	// Mirror `aileron secret list --json`: empty must be parseable JSON.
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agents":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "agent", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Errorf("stdout = %q, want []", stdout.String())
	}
}

func TestRunVaultDelete_OverlapPathRejectedNoPanic(t *testing.T) {
	// Regression: "agents/oauth" passes both HasPrefix and HasSuffix but
	// the prefix and suffix overlap. The validator must reject it
	// cleanly, not slice out of range.
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/oauth", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for overlap path")
	}
	if called {
		t.Errorf("HTTP issued for rejected overlap path")
	}
}

func TestRunVaultList_RejectsNonAgentsPrefix(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"agents":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--prefix", "internal/"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for non-agents prefix")
	}
	if called {
		t.Errorf("HTTP request issued for a rejected prefix")
	}
	if !strings.Contains(stderr.String(), "agents/") {
		t.Errorf("stderr = %q; want agents/-only message", stderr.String())
	}
}

func TestRunVaultPut_MissingFromFile(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request without --from-file")
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "agents/claude/oauth"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero when --from-file missing")
	}
	if !strings.Contains(stderr.String(), "--from-file") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunVaultPut_EmptyFile(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request for empty file")
	})
	dir := t.TempDir()
	f := filepath.Join(dir, "empty")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"put", "agents/claude/oauth", "--from-file", f},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for empty file")
	}
}

func TestRunVaultPut_LockedAndNoVault(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusLocked, "locked"},
		{http.StatusServiceUnavailable, "no vault"},
	} {
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"put", "agents/claude/oauth", "--from-file", f},
			strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("status %d: exit = 0, want non-zero", tc.status)
		}
	}
}

func TestRunVaultDelete_LockedAndNoVault(t *testing.T) {
	for _, status := range []int{http.StatusLocked, http.StatusServiceUnavailable} {
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"delete", "agents/claude/oauth", "--yes"},
			strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("status %d: exit = 0, want non-zero", status)
		}
	}
}

func TestRunVaultList_NoVault(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for 503")
	}
	if !strings.Contains(stderr.String(), "vault") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// internal/secret is an unrecognized `other` namespace: it is not an
// agent, user, or binding path, so delete still rejects it client-side
// with no HTTP call. The message must name the real constraint (the
// supported paths), not the old misleading "must be agents/<name>/<purpose>".
func TestRunVaultDelete_RejectsNonAgentPath(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "internal/secret", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for non-deletable path")
	}
	if called {
		t.Errorf("HTTP issued for rejected path")
	}
	msg := stderr.String()
	if strings.Contains(msg, "must be agents/<name>/<purpose>") {
		t.Errorf("stderr still uses the misleading agents-only message: %q", msg)
	}
	// The coherent message names the supported namespaces.
	for _, want := range []string{"agents/<name>/<purpose>", "user/<service>", "binding"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr = %q, want it to mention %q", msg, want)
		}
	}
}

// Control-plane namespaces (connected-accounts/, llm-config/) appear in
// `vault list --include-control-plane` but are managed by the control
// plane; delete rejects them client-side with no HTTP call and names the
// constraint.
func TestRunVaultDelete_RejectsControlPlanePath(t *testing.T) {
	for _, path := range []string{"connected-accounts/acme/github", "llm-config/anthropic/default"} {
		called := false
		fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		})
		var stdout, stderr bytes.Buffer
		code := runVault([]string{"delete", path, "--yes"},
			strings.NewReader(""), &stdout, &stderr)
		if code == 0 {
			t.Errorf("%s: exit = 0, want non-zero", path)
		}
		if called {
			t.Errorf("%s: HTTP issued for rejected control-plane path", path)
		}
		if !strings.Contains(stderr.String(), "control plane") {
			t.Errorf("%s: stderr = %q, want it to name the control plane", path, stderr.String())
		}
	}
}

// vault delete on a user/<service> path dispatches to the daemon's
// per-service user delete endpoint, DELETE /vault/user/<service>/credentials
// — symmetric with what `vault list` prints for a user entry.
func TestRunVaultDelete_UserPathDispatch(t *testing.T) {
	var gotMethod, gotPath string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "user/github", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/vault/user/github/credentials" {
		t.Errorf("request = %s %s, want DELETE /vault/user/github/credentials", gotMethod, gotPath)
	}
	if !strings.Contains(stdout.String(), "Deleted user/github") {
		t.Errorf("stdout = %q, want it to confirm deletion", stdout.String())
	}
}

// A 404 from the user delete endpoint surfaces a user-scoped not-found
// message and a non-zero exit.
func TestRunVaultDelete_UserPathNotFound(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "user/github", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for 404")
	}
	if !strings.Contains(stderr.String(), "user/github") {
		t.Errorf("stderr = %q, want it to name user/github", stderr.String())
	}
}

// vault delete on a binding path dispatches to DELETE /bindings/<name>,
// identical to `aileron binding revoke`, so connector/binding credentials
// `vault list` surfaces are deletable through the same verb.
func TestRunVaultDelete_BindingPathDispatch(t *testing.T) {
	const bindingPath = "aws_sigv4/aileron-connector-athena/aileron"
	var gotMethod, gotPath string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", bindingPath, "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/bindings/"+bindingPath {
		t.Errorf("request = %s %s, want DELETE /bindings/%s", gotMethod, gotPath, bindingPath)
	}
	if !strings.Contains(stdout.String(), "Deleted "+bindingPath) {
		t.Errorf("stdout = %q, want it to confirm deletion", stdout.String())
	}
}

// A 404 from the binding delete endpoint surfaces a binding-scoped
// not-found message and a non-zero exit.
func TestRunVaultDelete_BindingPathNotFound(t *testing.T) {
	const bindingPath = "aws_sigv4/aileron-connector-athena/aileron"
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", bindingPath, "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for 404")
	}
	if !strings.Contains(stderr.String(), "binding not found") {
		t.Errorf("stderr = %q, want a binding-not-found message", stderr.String())
	}
}

// The agent path remains supported and unchanged: agents/<name>/<purpose>
// dispatches to the agent credential endpoint with the purpose query param.
func TestRunVaultDelete_AgentPathDispatch(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"delete", "agents/claude/oauth", "--yes"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/vault/agents/claude/credentials" || gotQuery != "purpose=oauth" {
		t.Errorf("request = %s %s?%s, want DELETE /vault/agents/claude/credentials?purpose=oauth",
			gotMethod, gotPath, gotQuery)
	}
	if !strings.Contains(stdout.String(), "Deleted agents/claude/oauth") {
		t.Errorf("stdout = %q, want it to confirm deletion", stdout.String())
	}
}

func TestRunVault_DispatchUnknownAndEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runVault([]string{"explode"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Errorf("unknown subcommand exit = 0, want non-zero")
	}
	stderr.Reset()
	if code := runVault(nil, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Errorf("empty args exit = 0, want non-zero")
	}
}

// Guards the base64 wire encoding contract: the daemon expects
// `value` as a base64 string (spec `format: byte`), and encoding/json
// produces exactly that for a []byte field.
func TestAgentCredentialsBody_ValueIsBase64(t *testing.T) {
	raw, err := json.Marshal(agentCredentialsBody{Value: []byte("abc")})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Value != base64.StdEncoding.EncodeToString([]byte("abc")) {
		t.Errorf("value = %q, want base64 of abc", wire.Value)
	}
}

// vault list --scope user|all contract (#1180):
//
//   - --scope user routes to GET /vault/user and surfaces service names,
//     never the credential bytes.
//   - --scope all surfaces both agent names and user services.
//   - --scope user --json emits NDJSON keyed by service, and [] when empty.
//   - An invalid --scope is rejected before any HTTP call.
//   - --prefix agents/ together with a user-bearing scope is a usage error.
//   - 503 from /vault/user maps to the daemon-not-configured message.

func TestRunVaultList_ScopeUser(t *testing.T) {
	const secret = "gho_SECRETBYTES"
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/vault/user" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"services":[{"service":"github","metadata":{"type":"oauth_access_token"}}]}`)
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "user"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "user/github" {
		t.Errorf("stdout = %q, want user/github", stdout.String())
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stdout.String(), "value") {
		t.Errorf("stdout leaked credential material: %q", stdout.String())
	}
}

func TestRunVaultList_ScopeUserJSON(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"services":[{"service":"github"}]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "user", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	var u userSummary
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &u); err != nil {
		t.Fatalf("NDJSON not valid: %v (%q)", err, stdout.String())
	}
	if u.Service != "github" {
		t.Errorf("service = %q, want github", u.Service)
	}
}

func TestRunVaultList_ScopeUserEmpty(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"services":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "user"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No user credentials stored.") {
		t.Errorf("stdout = %q", stdout.String())
	}

	var jout, jerr bytes.Buffer
	code = runVault([]string{"list", "--scope", "user", "--json"}, strings.NewReader(""), &jout, &jerr)
	if code != 0 {
		t.Fatalf("json exit = %d, want 0", code)
	}
	if strings.TrimSpace(jout.String()) != "[]" {
		t.Errorf("json stdout = %q, want []", jout.String())
	}
}

func TestRunVaultList_ScopeAll(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vault/agents":
			_, _ = io.WriteString(w, `{"agents":[{"name":"claude"}]}`)
		case "/vault/user":
			_, _ = io.WriteString(w, `{"services":[{"service":"github"}]}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "all"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Fields(stdout.String())
	if len(lines) != 2 || lines[0] != "agents/claude/oauth" || lines[1] != "user/github" {
		t.Errorf("output = %q, want agents/claude/oauth then user/github", stdout.String())
	}
}

func TestRunVaultList_InvalidScopeRejectedNoCall(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for invalid scope")
	}
	if called {
		t.Errorf("HTTP issued for rejected scope")
	}
	if !strings.Contains(stderr.String(), "scope") {
		t.Errorf("stderr = %q; want scope error", stderr.String())
	}
}

func TestRunVaultList_PrefixAgentsConflictsWithUserScope(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--prefix", "agents/", "--scope", "all"},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for prefix/scope conflict")
	}
	if called {
		t.Errorf("HTTP issued despite conflicting flags")
	}
}

func TestRunVaultList_ScopeUserNoVault(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "user"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for 503")
	}
	if !strings.Contains(stderr.String(), "vault") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// #1402: `vault list` with no --scope hits the new /vault union endpoint
// and renders every namespace, each line prefixed with its scope label.
func TestRunVaultList_UnionDefaultGroupsByScope(t *testing.T) {
	const body = `{"entries":[` +
		`{"path":"agents/claude/oauth","scope":"agent"},` +
		`{"path":"user/github","scope":"user"},` +
		`{"path":"connectors/github/default","scope":"binding"},` +
		`{"path":"oauth2/google/default","scope":"binding"}]}`
	var gotPath string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Query().Get("include_control_plane") != "" {
			t.Errorf("default union must not set include_control_plane: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, body)
	})

	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotPath != "/vault" {
		t.Errorf("default list hit %q, want /vault (the union endpoint)", gotPath)
	}
	out := stdout.String()
	for _, want := range []string{
		"agents/claude/oauth",
		"user/github",
		"connectors/github/default",
		"oauth2/google/default",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("union output missing %q\n%s", want, out)
		}
	}
	// Every scope label must appear; the connector/binding entries are
	// exactly what the old agent+user default silently hid.
	for _, label := range []string{"agent:", "user:", "binding:"} {
		if !strings.Contains(out, label) {
			t.Errorf("union output missing scope label %q:\n%s", label, out)
		}
	}
}

// --json over the union streams one NDJSON object per entry carrying both
// path and scope.
func TestRunVaultList_UnionJSON(t *testing.T) {
	const body = `{"entries":[{"path":"agents/claude/oauth","scope":"agent"},{"path":"connectors/github/default","scope":"binding"}]}`
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("union --json = %q, want 2 NDJSON lines", stdout.String())
	}
	gotScopes := map[string]string{}
	for _, l := range lines {
		var e vaultEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("NDJSON line not valid: %v (%q)", err, l)
		}
		gotScopes[e.Path] = e.Scope
	}
	if gotScopes["agents/claude/oauth"] != "agent" || gotScopes["connectors/github/default"] != "binding" {
		t.Errorf("union --json scopes = %v, want agent + binding", gotScopes)
	}
}

// --include-control-plane threads the include_control_plane query param.
func TestRunVaultList_UnionIncludeControlPlane(t *testing.T) {
	var gotInclude string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotInclude = r.URL.Query().Get("include_control_plane")
		_, _ = io.WriteString(w, `{"entries":[{"path":"connected-accounts/usr_1/slack","scope":"connected-account"}]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--include-control-plane"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotInclude != "true" {
		t.Errorf("include_control_plane query = %q, want true", gotInclude)
	}
	if !strings.Contains(stdout.String(), "connected-account:") {
		t.Errorf("output missing control-plane entry:\n%s", stdout.String())
	}
}

// An empty union prints the shared "nothing stored" message; --json yields
// an empty array for script-parseability.
func TestRunVaultList_UnionEmpty(t *testing.T) {
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"entries":[]}`)
	})
	var stdout, stderr bytes.Buffer
	if code := runVault([]string{"list"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No credentials stored.") {
		t.Errorf("empty union text = %q", stdout.String())
	}

	var jout, jerr bytes.Buffer
	if code := runVault([]string{"list", "--json"}, strings.NewReader(""), &jout, &jerr); code != 0 {
		t.Fatalf("json exit = %d, want 0", code)
	}
	if strings.TrimSpace(jout.String()) != "[]" {
		t.Errorf("empty union --json = %q, want []", jout.String())
	}
}

// --include-control-plane only applies to the union; combining it with an
// explicit --scope is a usage error caught before any HTTP call.
func TestRunVaultList_IncludeControlPlaneWithScopeRejected(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"agents":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--scope", "agent", "--include-control-plane"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for --include-control-plane with --scope")
	}
	if called {
		t.Errorf("HTTP issued for rejected flag combination")
	}
}

// The legacy --prefix agents/ still routes to the typed agent endpoint, not
// the union.
func TestRunVaultList_PrefixAgentsStillTyped(t *testing.T) {
	var gotPath string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"agents":[{"name":"claude","purpose":"oauth"}]}`)
	})
	var stdout, stderr bytes.Buffer
	code := runVault([]string{"list", "--prefix", "agents/"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotPath != "/vault/agents" {
		t.Errorf("--prefix agents/ hit %q, want /vault/agents (typed, not union)", gotPath)
	}
	if got := strings.TrimSpace(stdout.String()); got != "agents/claude/oauth" {
		t.Errorf("output = %q, want agents/claude/oauth", got)
	}
}
