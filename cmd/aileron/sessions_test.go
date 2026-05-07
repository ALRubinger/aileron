package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
)

// /v1/sessions CLI contract:
//
//   - `aileron sessions` and `aileron sessions list` both list sessions
//     (mirroring `aileron audit`'s no-subcommand-means-list shape).
//   - List renders a fixed-width table by default; `--json` emits one
//     JSON object per session. Empty sets render "No sessions." in
//     human mode and `[]` in JSON mode.
//   - Status column maps (EndedAt, ExitCode) to "running" / "ended" /
//     "orphaned" per the OpenAPI Session schema.
//   - `aileron sessions get <id>` emits indented JSON; 404 surfaces as
//     "session ... not found" on stderr with exit code 1.
//   - Filter flags (--active, --agent, --since, --limit) flow through
//     to the wire query parameters.
//   - Error from the fetcher (typically a spawn / daemon failure) is
//     surfaced verbatim with exit code 1.

// withSessionsFetchers swaps the package-level fetcher seams for the
// duration of a test, restoring on cleanup. Mirrors the pattern used
// by audit / runtimeStatus tests.
func withSessionsFetchers(t *testing.T,
	list func(sessionsListQuery) (*api.SessionListResponse, error),
	get func(string) (*api.Session, int, error),
) {
	t.Helper()
	if list != nil {
		origList := sessionsListFetcher
		sessionsListFetcher = list
		t.Cleanup(func() { sessionsListFetcher = origList })
	}
	if get != nil {
		origGet := sessionsGetFetcher
		sessionsGetFetcher = get
		t.Cleanup(func() { sessionsGetFetcher = origGet })
	}
}

// --- list rendering ---

func TestRunSessions_NoSubcommandListsByDefault(t *testing.T) {
	withSessionsFetchers(t,
		func(sessionsListQuery) (*api.SessionListResponse, error) {
			return &api.SessionListResponse{Items: []api.Session{}}, nil
		},
		nil,
	)
	var stdout, stderr bytes.Buffer
	code := runSessions(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No sessions") {
		t.Errorf("bare `aileron sessions` should list (got %q)", stdout.String())
	}
}

func TestRunSessionsList_EmptyRendersHumanMessageAndHint(t *testing.T) {
	withSessionsFetchers(t,
		func(sessionsListQuery) (*api.SessionListResponse, error) {
			return &api.SessionListResponse{}, nil
		},
		nil,
	)
	var stdout, stderr bytes.Buffer
	code := runSessionsList(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "No sessions.") {
		t.Errorf("missing empty-state message; got %q", out)
	}
	if !strings.Contains(out, "aileron launch") {
		t.Errorf("empty-state should hint at `aileron launch`; got %q", out)
	}
}

func TestRunSessionsList_EmptyJSONRendersBracketBracket(t *testing.T) {
	withSessionsFetchers(t,
		func(sessionsListQuery) (*api.SessionListResponse, error) {
			return &api.SessionListResponse{}, nil
		},
		nil,
	)
	var stdout bytes.Buffer
	code := runSessionsList([]string{"--json"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatal("expected exit 0")
	}
	if got := strings.TrimSpace(stdout.String()); got != "[]" {
		t.Errorf("empty --json should be []; got %q", got)
	}
}

func TestRunSessionsList_RendersTableRowPerSession(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	exit := 0
	end := now.Add(45 * time.Second)
	withSessionsFetchers(t,
		func(sessionsListQuery) (*api.SessionListResponse, error) {
			return &api.SessionListResponse{Items: []api.Session{
				{Id: "01ABC", Agent: "claude", StartedAt: now, EndedAt: &end, ExitCode: &exit, WorkingDir: "/proj"},
				{Id: "01DEF", Agent: "pi", StartedAt: now}, // running
			}}, nil
		},
		nil,
	)
	var stdout bytes.Buffer
	code := runSessionsList(nil, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatal("expected exit 0")
	}
	out := stdout.String()
	for _, want := range []string{"01ABC", "01DEF", "claude", "pi", "ended", "running", "exit=0", "/proj"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got:\n%s", want, out)
		}
	}
}

func TestRunSessionsList_StatusForOrphan(t *testing.T) {
	// EndedAt set + ExitCode nil = orphaned (daemon-restart signal).
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	end := now.Add(45 * time.Second)
	withSessionsFetchers(t,
		func(sessionsListQuery) (*api.SessionListResponse, error) {
			return &api.SessionListResponse{Items: []api.Session{
				{Id: "01ORPH", Agent: "claude", StartedAt: now, EndedAt: &end},
			}}, nil
		},
		nil,
	)
	var stdout bytes.Buffer
	code := runSessionsList(nil, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatal("expected exit 0")
	}
	if !strings.Contains(stdout.String(), "orphaned") {
		t.Errorf("orphan should render with status=orphaned; got %q", stdout.String())
	}
}

func TestRunSessionsList_JSONOneObjectPerLine(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	exit := 0
	end := now.Add(time.Second)
	withSessionsFetchers(t,
		func(sessionsListQuery) (*api.SessionListResponse, error) {
			return &api.SessionListResponse{Items: []api.Session{
				{Id: "01ABC", Agent: "claude", StartedAt: now, EndedAt: &end, ExitCode: &exit},
				{Id: "01DEF", Agent: "pi", StartedAt: now},
			}}, nil
		},
		nil,
	)
	var stdout bytes.Buffer
	code := runSessionsList([]string{"--json"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatal("expected exit 0")
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON lines, got %d:\n%s", len(lines), stdout.String())
	}
	for i, line := range lines {
		var s api.Session
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Errorf("line %d not valid JSON: %v (%s)", i, err, line)
		}
	}
}

// --- filter flags flow through to the query ---

func TestRunSessionsList_FlagsFlowToQuery(t *testing.T) {
	var got sessionsListQuery
	withSessionsFetchers(t,
		func(q sessionsListQuery) (*api.SessionListResponse, error) {
			got = q
			return &api.SessionListResponse{}, nil
		},
		nil,
	)
	code := runSessionsList(
		[]string{"--active", "--agent", "claude", "--since", "2026-05-01T00:00:00Z", "--limit", "5"},
		&bytes.Buffer{}, &bytes.Buffer{},
	)
	if code != 0 {
		t.Fatal("expected exit 0")
	}
	if !got.activeOnly {
		t.Error("--active should set activeOnly")
	}
	if got.agent != "claude" {
		t.Errorf("agent = %q, want claude", got.agent)
	}
	if got.since != "2026-05-01T00:00:00Z" {
		t.Errorf("since = %q", got.since)
	}
	if got.limit != 5 {
		t.Errorf("limit = %d, want 5", got.limit)
	}
}

// --- error propagation ---

func TestRunSessionsList_FetcherErrorReturnsNonZero(t *testing.T) {
	withSessionsFetchers(t,
		func(sessionsListQuery) (*api.SessionListResponse, error) {
			return nil, fmt.Errorf("daemon unreachable")
		},
		nil,
	)
	var stdout, stderr bytes.Buffer
	code := runSessionsList(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit on fetcher error")
	}
	if !strings.Contains(stderr.String(), "daemon unreachable") {
		t.Errorf("expected error in stderr; got %q", stderr.String())
	}
}

// --- get ---

func TestRunSessionsGet_RendersIndentedJSON(t *testing.T) {
	exit := 0
	end := time.Now().UTC()
	withSessionsFetchers(t, nil,
		func(id string) (*api.Session, int, error) {
			if id != "01ABC" {
				t.Errorf("id = %q, want 01ABC", id)
			}
			return &api.Session{
				Id: "01ABC", Agent: "claude", StartedAt: end, EndedAt: &end, ExitCode: &exit,
			}, http.StatusOK, nil
		},
	)
	var stdout, stderr bytes.Buffer
	code := runSessionsGet([]string{"01ABC"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	// Indented JSON has two-space indentation on every property line.
	if !strings.Contains(out, `  "id": "01ABC"`) {
		t.Errorf("expected indented JSON; got %q", out)
	}
}

func TestRunSessionsGet_NotFoundReturnsNonZero(t *testing.T) {
	withSessionsFetchers(t, nil,
		func(string) (*api.Session, int, error) {
			return nil, http.StatusNotFound, nil
		},
	)
	var stdout, stderr bytes.Buffer
	code := runSessionsGet([]string{"missing"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit for 404")
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("expected 'not found' on stderr; got %q", stderr.String())
	}
}

func TestRunSessionsGet_MissingIDReturnsUsage(t *testing.T) {
	withSessionsFetchers(t, nil,
		func(string) (*api.Session, int, error) {
			t.Fatal("fetcher should not be called when id is missing")
			return nil, 0, nil
		},
	)
	var stderr bytes.Buffer
	code := runSessionsGet(nil, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit when id is missing")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("expected usage on stderr; got %q", stderr.String())
	}
}

// --- dispatch ---

func TestRunSessions_UnknownSubcommandReturnsUsage(t *testing.T) {
	var stderr bytes.Buffer
	code := runSessions([]string{"frobnicate"}, &bytes.Buffer{}, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit for unknown subcommand")
	}
	if !strings.Contains(stderr.String(), "unknown sessions command") {
		t.Errorf("expected unknown-command message; got %q", stderr.String())
	}
}

// --- sessionStatus / sessionSummary helpers ---

func TestSessionStatus(t *testing.T) {
	now := time.Now()
	end := now.Add(time.Second)
	exit := 0

	cases := []struct {
		name string
		s    api.Session
		want string
	}{
		{"running", api.Session{StartedAt: now}, "running"},
		{"ended", api.Session{StartedAt: now, EndedAt: &end, ExitCode: &exit}, "ended"},
		{"orphaned", api.Session{StartedAt: now, EndedAt: &end}, "orphaned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionStatus(tc.s); got != tc.want {
				t.Errorf("sessionStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionSummary_EndedHasExitAndDuration(t *testing.T) {
	start := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Minute)
	exit := 42
	got := sessionSummary(api.Session{StartedAt: start, EndedAt: &end, ExitCode: &exit, WorkingDir: "/proj"})
	for _, want := range []string{"exit=42", "2m0s", "/proj"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in summary %q", want, got)
		}
	}
}

// --- HTTP fetchers against a fake daemon ---

// fetchSessionsList / fetchSessionsGet are the only places in the
// sessions surface that talk to the wire. The fetcher seam is
// stubbed in the runner tests above; these tests exercise the
// real fetchers against an httptest server so the URL shape, query
// string, and decode path get end-to-end coverage.

func TestFetchSessionsList_BuildsQueryAndDecodes(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	exit := 0
	end := now.Add(time.Second)
	gotPath := make(chan string, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path + "?" + r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(api.SessionListResponse{Items: []api.Session{
			{Id: "01ABC", Agent: "claude", StartedAt: now, EndedAt: &end, ExitCode: &exit},
		}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	resp, err := fetchSessionsList(sessionsListQuery{
		activeOnly: true,
		agent:      "claude",
		since:      "2026-05-01T00:00:00Z",
		limit:      10,
	})
	if err != nil {
		t.Fatalf("fetchSessionsList: %v", err)
	}

	pathQuery := <-gotPath
	if !strings.HasPrefix(pathQuery, "/v1/sessions?") {
		t.Errorf("expected /v1/sessions path; got %q", pathQuery)
	}
	for _, want := range []string{"active_only=true", "agent=claude", "since=2026-05-01", "limit=10"} {
		if !strings.Contains(pathQuery, want) {
			t.Errorf("expected %q in query; got %q", want, pathQuery)
		}
	}
	if len(resp.Items) != 1 || resp.Items[0].Id != "01ABC" {
		t.Errorf("decoded body wrong: %+v", resp)
	}
}

func TestFetchSessionsList_EmptyQueryWhenNoFilters(t *testing.T) {
	gotQuery := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery <- r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(api.SessionListResponse{})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	if _, err := fetchSessionsList(sessionsListQuery{}); err != nil {
		t.Fatal(err)
	}
	if got := <-gotQuery; got != "" {
		t.Errorf("expected no query string, got %q", got)
	}
}

func TestFetchSessionsList_Non200SurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	_, err := fetchSessionsList(sessionsListQuery{})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected status code in error; got %v", err)
	}
}

func TestFetchSessionsGet_Decodes(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sessions/01ABC") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.Session{Id: "01ABC", Agent: "claude", StartedAt: now})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	s, status, err := fetchSessionsGet("01ABC")
	if err != nil {
		t.Fatalf("fetchSessionsGet: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if s.Id != "01ABC" {
		t.Errorf("id = %q, want 01ABC", s.Id)
	}
}

func TestFetchSessionsGet_404SurfacesViaStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	s, status, err := fetchSessionsGet("missing")
	if err != nil {
		t.Fatalf("404 should not be a transport error: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if s != nil {
		t.Errorf("session = %+v, want nil on 404", s)
	}
}

func TestFetchSessionsGet_Non200Non404IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	_, _, err := fetchSessionsGet("01ABC")
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestFetchSessionsList_PropagatesBaseURLError(t *testing.T) {
	// When AILERON_API_URL isn't set, bindingAPIBaseURL goes through
	// the spawn path. Stub that path to return a sentinel error and
	// verify it propagates.
	t.Setenv("AILERON_API_URL", "")
	sentinel := fmt.Errorf("spawn boom")
	orig := bindingAPIBaseURL
	bindingAPIBaseURL = func() (string, error) { return "", sentinel }
	t.Cleanup(func() { bindingAPIBaseURL = orig })

	if _, err := fetchSessionsList(sessionsListQuery{}); err == nil {
		t.Fatal("expected error from bindingAPIBaseURL")
	}
	if _, _, err := fetchSessionsGet("x"); err == nil {
		t.Fatal("expected error from bindingAPIBaseURL")
	}
}
