package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
)

// sessionsListFetcher and sessionsGetFetcher are the HTTP clients for
// the daemon's `/v1/sessions` endpoints. Replaceable in tests so they
// don't depend on a running daemon (mirrors auditListFetcher /
// auditGetFetcher). Keep at package scope so tests can swap them.
var (
	sessionsListFetcher = fetchSessionsList
	sessionsGetFetcher  = fetchSessionsGet
)

const sessionsUsage = `usage:
  aileron sessions list [--active] [--agent NAME] [--since RFC3339] [--limit N] [--json]
  aileron sessions get  <session-id> [--json]`

// sessionsListQuery mirrors the four query parameters supported by
// `GET /v1/sessions`. Empty / zero values are not sent.
type sessionsListQuery struct {
	activeOnly bool
	agent      string
	since      string
	limit      int
}

// runSessions dispatches `aileron sessions <subcommand>`. With no
// subcommand it lists, mirroring `aileron audit`'s shape — the most
// common interactive use is "what did I run today?" which a bare
// `aileron sessions` should answer without ceremony.
func runSessions(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runSessionsList(nil, stdout, stderr)
	}
	switch args[0] {
	case "list":
		return runSessionsList(args[1:], stdout, stderr)
	case "get":
		return runSessionsGet(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown sessions command: %q\n", args[0])
		fmt.Fprintln(stderr, sessionsUsage)
		return 1
	}
}

// runSessionsList renders the user's launch sessions as a fixed-width
// table. The render layout mirrors `aileron audit list`: timestamp,
// id, status, summary. The status column carries the three-state
// vocabulary the OpenAPI spec defines:
//   - "running"     — ended_at is null
//   - "ended"       — ended_at + exit_code present
//   - "orphaned"    — ended_at present, exit_code null
//
// "orphaned" is the honest signal for a session whose daemon was
// killed mid-flight: the orphan-reaper stamps ended_at on next
// daemon start without fabricating an exit code (per ADR-0012).
func runSessionsList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sessions list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	activeOnly := flags.Bool("active", false, "Only sessions still running (ended_at is null)")
	agent := flags.String("agent", "", "Filter by agent name (e.g. claude, pi)")
	since := flags.String("since", "", "Lower-bound on started_at (RFC 3339, e.g. 2026-05-01T00:00:00Z)")
	limit := flags.Int("limit", 0, "Maximum sessions to return (default: server returns all)")
	asJSON := flags.Bool("json", false, "Emit one JSON-encoded session per line")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	resp, err := sessionsListFetcher(sessionsListQuery{
		activeOnly: *activeOnly,
		agent:      *agent,
		since:      *since,
		limit:      *limit,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(resp.Items) == 0 {
		if *asJSON {
			fmt.Fprintln(stdout, "[]")
			return 0
		}
		fmt.Fprintln(stdout, "No sessions.")
		fmt.Fprintln(stdout, "Run `aileron launch <agent>` to start one.")
		return 0
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		for _, s := range resp.Items {
			if err := enc.Encode(s); err != nil {
				fmt.Fprintf(stderr, "encode: %v\n", err)
				return 1
			}
		}
		return 0
	}
	for _, s := range resp.Items {
		ts := s.StartedAt.Local().Format("2006-01-02 15:04:05")
		fmt.Fprintf(stdout, "%s  %-26s  %-9s  %-8s  %s\n",
			ts, s.Id, sessionStatus(s), s.Agent, sessionSummary(s))
	}
	return 0
}

// runSessionsGet fetches a single session by id and emits it as
// indented JSON. Always JSON: the wire shape is small (8 fields)
// and a "show" command is expected to be machine-readable for
// scripts that need the full record.
func runSessionsGet(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sessions get", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 1
	}
	rest := flags.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: aileron sessions get <session-id>")
		return 1
	}
	s, status, err := sessionsGetFetcher(rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status == http.StatusNotFound {
		fmt.Fprintf(stderr, "session %q not found\n", rest[0])
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		fmt.Fprintf(stderr, "encode: %v\n", err)
		return 1
	}
	return 0
}

// sessionStatus collapses the (EndedAt, ExitCode) tuple to the wire
// vocabulary defined in the OpenAPI Session schema. The three states
// are mutually exclusive; ExitCode without EndedAt is impossible by
// construction.
func sessionStatus(s api.Session) string {
	if s.EndedAt == nil {
		return "running"
	}
	if s.ExitCode == nil {
		return "orphaned"
	}
	return "ended"
}

// sessionSummary formats the trailing column of `sessions list`. For
// ended sessions it shows the exit code and a duration; for running
// sessions, the working directory; for orphans, just the working dir
// (their duration is meaningless — the daemon-restart timestamp).
func sessionSummary(s api.Session) string {
	switch sessionStatus(s) {
	case "ended":
		dur := s.EndedAt.Sub(s.StartedAt)
		return fmt.Sprintf("exit=%d  in %s  cwd=%s", *s.ExitCode, dur.Round(time.Second), s.WorkingDir)
	case "orphaned":
		return fmt.Sprintf("cwd=%s", s.WorkingDir)
	default: // running
		return fmt.Sprintf("cwd=%s", s.WorkingDir)
	}
}

// fetchSessionsList issues `GET /v1/sessions` and decodes the body.
// Empty query parameters are not sent — the daemon's defaults apply
// (no filter, return all matching).
func fetchSessionsList(q sessionsListQuery) (*api.SessionListResponse, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(base + "/sessions")
	if err != nil {
		return nil, err
	}
	qs := u.Query()
	if q.activeOnly {
		qs.Set("active_only", "true")
	}
	if q.agent != "" {
		qs.Set("agent", q.agent)
	}
	if q.since != "" {
		qs.Set("since", q.since)
	}
	if q.limit > 0 {
		qs.Set("limit", strconv.Itoa(q.limit))
	}
	u.RawQuery = qs.Encode()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(raw))
	}
	var out api.SessionListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &out, nil
}

// fetchSessionsGet issues `GET /v1/sessions/{id}` and returns the
// session record plus the HTTP status. 404 is surfaced via the
// status int so callers can distinguish "not found" from a transport
// error without parsing strings.
func fetchSessionsGet(sessionID string) (*api.Session, int, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return nil, 0, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(base + "/sessions/" + url.PathEscape(sessionID))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, http.StatusNotFound, nil
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, resp.StatusCode, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(raw))
	}
	var s api.Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decoding response: %w", err)
	}
	return &s, resp.StatusCode, nil
}
