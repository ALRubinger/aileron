package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// hubConnectorEntry mirrors the wire shape of api.HubConnectorEntry.
// Declared locally so this CLI binary doesn't pull the full generated
// types graph just to render Hub discovery output, matching the
// convention followed by binding/sync/connector-check rendering
// helpers.
type hubConnectorEntry struct {
	FQN             string `json:"fqn"`
	Description     string `json:"description"`
	PublisherGithub string `json:"publisher_github"`
	KeyURL          string `json:"key_url"`
	ReleasePattern  string `json:"release_pattern"`
}

// hubActionEntry mirrors api.HubActionEntry.
type hubActionEntry struct {
	FQN             string   `json:"fqn"`
	Description     string   `json:"description"`
	PublisherGithub string   `json:"publisher_github"`
	ConnectorFQN    string   `json:"connector_fqn"`
	Intents         []string `json:"intents,omitempty"`
	Category        string   `json:"category,omitempty"`
}

// hubSuiteEntry mirrors api.HubSuiteEntry.
type hubSuiteEntry struct {
	FQN                string   `json:"fqn"`
	Description        string   `json:"description"`
	PublisherGithub    string   `json:"publisher_github"`
	MemberActions      []string `json:"member_actions"`
	ConnectorsRequired []string `json:"connectors_required,omitempty"`
	Category           string   `json:"category,omitempty"`
}

type hubConnectorList struct {
	Connectors []hubConnectorEntry `json:"connectors"`
}

type hubActionList struct {
	Actions []hubActionEntry `json:"actions"`
}

type hubSuiteList struct {
	Suites []hubSuiteEntry `json:"suites"`
}

// runHub dispatches `aileron hub <subcommand>`. The Hub is the
// community discovery catalog (ADR-0013, action-first amendment): the
// daemon shallow-clones aileron-connectors-hub per query and serves
// discovery via /v1/hub/*. This CLI is a thin client over those
// endpoints; the catalog holds three entry types (suites, actions,
// connectors) and the CLI surfaces all three.
func runHub(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron hub <list|search|show> [args...]")
		return 1
	}
	switch args[0] {
	case "list":
		return runHubList(args[1:], stdout, stderr)
	case "search":
		return runHubSearch(args[1:], stdout, stderr)
	case "show":
		return runHubShow(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown hub subcommand: %q\n", args[0])
		fmt.Fprintln(stderr, "usage: aileron hub <list|search|show> [args...]")
		return 1
	}
}

// runHubList prints catalog entries. The optional positional argument
// selects which catalog (connectors, actions, or suites); omitting it
// prints all three types in IA order (suites → actions → connectors)
// since intent-first browse is the action-first default per ADR-0013.
// `--json` renders NDJSON: one object per line in single-type mode, or
// `{"suites":[...],"actions":[...],"connectors":[...]}` in combined
// mode.
func runHubList(args []string, stdout, stderr io.Writer) int {
	category, rest := takePositional(args)
	flags := flag.NewFlagSet("hub list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Render entries as NDJSON (single-type) or one combined JSON object (default mode)")
	if err := flags.Parse(rest); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aileron hub list [connectors|actions|suites] [--json]")
		return 1
	}
	switch category {
	case "":
		return doHubListAll(*asJSON, stdout, stderr)
	case "connectors":
		return doHubListConnectors("", "", *asJSON, stdout, stderr)
	case "actions":
		return doHubListActions("", "", *asJSON, stdout, stderr)
	case "suites":
		return doHubListSuites("", "", *asJSON, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown catalog: %q\n", category)
		fmt.Fprintln(stderr, "usage: aileron hub list [connectors|actions|suites] [--json]")
		return 1
	}
}

// runHubSearch is the cross-type keyword filter. By default queries all
// three catalogs and groups results by type. `--type` narrows to a
// single catalog when the caller already knows what they want.
func runHubSearch(args []string, stdout, stderr io.Writer) int {
	args = reorderArgsFlagsFirst(args, map[string]bool{"type": true})
	flags := flag.NewFlagSet("hub search", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Render entries as NDJSON (single-type) or one combined JSON object (default mode)")
	typeFilter := flags.String("type", "", "Restrict search to one catalog: connectors, actions, or suites")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: aileron hub search <query> [--type connectors|actions|suites] [--json]")
		return 1
	}
	query := flags.Arg(0)
	if strings.TrimSpace(query) == "" {
		fmt.Fprintln(stderr, "error: query cannot be empty")
		return 1
	}
	switch *typeFilter {
	case "":
		return doHubSearchAll(query, *asJSON, stdout, stderr)
	case "connectors":
		return doHubListConnectors(query, query, *asJSON, stdout, stderr)
	case "actions":
		return doHubListActions(query, query, *asJSON, stdout, stderr)
	case "suites":
		return doHubListSuites(query, query, *asJSON, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown --type value: %q\n", *typeFilter)
		fmt.Fprintln(stderr, "valid values: connectors, actions, suites")
		return 1
	}
}

// runHubShow prints the full record for a single Hub entry. The
// catalog is inferred by trying each endpoint in order
// (connector → action → suite) and returning the first hit. `--type`
// skips the dispatch and queries the named catalog directly.
func runHubShow(args []string, stdout, stderr io.Writer) int {
	args = reorderArgsFlagsFirst(args, map[string]bool{"type": true})
	flags := flag.NewFlagSet("hub show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Render the entry as a single JSON object")
	typeFilter := flags.String("type", "", "Skip dispatch and query the named catalog: connectors, actions, or suites")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: aileron hub show <fqn> [--type connectors|actions|suites] [--json]")
		return 1
	}
	fqn := flags.Arg(0)
	if strings.TrimSpace(fqn) == "" {
		fmt.Fprintln(stderr, "error: fqn cannot be empty")
		return 1
	}

	switch *typeFilter {
	case "":
		return doHubShowDispatch(fqn, *asJSON, stdout, stderr)
	case "connectors":
		return doHubShowConnector(fqn, *asJSON, stdout, stderr)
	case "actions":
		return doHubShowAction(fqn, *asJSON, stdout, stderr)
	case "suites":
		return doHubShowSuite(fqn, *asJSON, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown --type value: %q\n", *typeFilter)
		fmt.Fprintln(stderr, "valid values: connectors, actions, suites")
		return 1
	}
}

// takePositional pulls a leading non-flag argument off args and returns
// it plus the remainder. Lets `hub list connectors --json` work as
// expected without making `flag.Parse` stumble over the positional.
func takePositional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// reorderArgsFlagsFirst moves every `--flag` (and its value, for
// value-flags) ahead of any positional arguments so Go's stdlib
// `flag.Parse` — which stops scanning at the first non-flag arg — can
// pick up flags regardless of whether the user typed them before or
// after the positional. valueFlags names the flags that consume a
// following arg ("type" → `--type X`); boolean flags ("json") are
// passed through unchanged.
func reorderArgsFlagsFirst(args []string, valueFlags map[string]bool) []string {
	var positional []string
	var flagPart []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagPart = append(flagPart, a)
			name := strings.TrimLeft(a, "-")
			// `--flag=value` already self-contained; only consume a
			// trailing arg when the user typed `--flag value`.
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				flagPart = append(flagPart, args[i+1])
				i++
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flagPart, positional...)
}

// --- list helpers, one per catalog ---

func doHubListConnectors(query, emptyHint string, asJSON bool, stdout, stderr io.Writer) int {
	entries, code := fetchHubConnectors(query, stderr)
	if code != 0 {
		return code
	}
	if len(entries) == 0 {
		emitEmpty(stdout, asJSON, "connectors", emptyHint)
		return 0
	}
	if asJSON {
		return encodeNDJSON(stdout, stderr, asAnySlice(entries))
	}
	renderConnectorTable(stdout, entries)
	return 0
}

func doHubListActions(query, emptyHint string, asJSON bool, stdout, stderr io.Writer) int {
	entries, code := fetchHubActions(query, stderr)
	if code != 0 {
		return code
	}
	if len(entries) == 0 {
		emitEmpty(stdout, asJSON, "actions", emptyHint)
		return 0
	}
	if asJSON {
		return encodeNDJSON(stdout, stderr, asAnySlice(entries))
	}
	renderActionTable(stdout, entries)
	return 0
}

func doHubListSuites(query, emptyHint string, asJSON bool, stdout, stderr io.Writer) int {
	entries, code := fetchHubSuites(query, stderr)
	if code != 0 {
		return code
	}
	if len(entries) == 0 {
		emitEmpty(stdout, asJSON, "suites", emptyHint)
		return 0
	}
	if asJSON {
		return encodeNDJSON(stdout, stderr, asAnySlice(entries))
	}
	renderSuiteTable(stdout, entries)
	return 0
}

// doHubListAll prints all three catalogs in IA order (Suites first,
// then Actions, then Providers/connectors). JSON mode emits one
// combined object so consumers can pull each type out by key.
func doHubListAll(asJSON bool, stdout, stderr io.Writer) int {
	suites, code := fetchHubSuites("", stderr)
	if code != 0 {
		return code
	}
	actions, code := fetchHubActions("", stderr)
	if code != 0 {
		return code
	}
	connectors, code := fetchHubConnectors("", stderr)
	if code != 0 {
		return code
	}
	if asJSON {
		out := struct {
			Suites     []hubSuiteEntry     `json:"suites"`
			Actions    []hubActionEntry    `json:"actions"`
			Connectors []hubConnectorEntry `json:"connectors"`
		}{Suites: suites, Actions: actions, Connectors: connectors}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}
	if len(suites)+len(actions)+len(connectors) == 0 {
		fmt.Fprintln(stdout, "Hub catalog is empty.")
		return 0
	}
	if len(suites) > 0 {
		fmt.Fprintln(stdout, "── SUITES ──")
		renderSuiteTable(stdout, suites)
		fmt.Fprintln(stdout)
	}
	if len(actions) > 0 {
		fmt.Fprintln(stdout, "── ACTIONS ──")
		renderActionTable(stdout, actions)
		fmt.Fprintln(stdout)
	}
	if len(connectors) > 0 {
		fmt.Fprintln(stdout, "── PROVIDERS ──")
		renderConnectorTable(stdout, connectors)
	}
	return 0
}

// doHubSearchAll runs the same keyword filter across all three catalogs
// and groups the results by type. Empty results in any one catalog are
// omitted from the human-rendered output but always present (as `[]`)
// in JSON mode.
func doHubSearchAll(query string, asJSON bool, stdout, stderr io.Writer) int {
	suites, code := fetchHubSuites(query, stderr)
	if code != 0 {
		return code
	}
	actions, code := fetchHubActions(query, stderr)
	if code != 0 {
		return code
	}
	connectors, code := fetchHubConnectors(query, stderr)
	if code != 0 {
		return code
	}
	if asJSON {
		out := struct {
			Suites     []hubSuiteEntry     `json:"suites"`
			Actions    []hubActionEntry    `json:"actions"`
			Connectors []hubConnectorEntry `json:"connectors"`
		}{Suites: suites, Actions: actions, Connectors: connectors}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}
	if len(suites)+len(actions)+len(connectors) == 0 {
		fmt.Fprintf(stdout, "No Hub entries match %q.\n", query)
		return 0
	}
	if len(suites) > 0 {
		fmt.Fprintln(stdout, "── SUITES ──")
		renderSuiteTable(stdout, suites)
		fmt.Fprintln(stdout)
	}
	if len(actions) > 0 {
		fmt.Fprintln(stdout, "── ACTIONS ──")
		renderActionTable(stdout, actions)
		fmt.Fprintln(stdout)
	}
	if len(connectors) > 0 {
		fmt.Fprintln(stdout, "── PROVIDERS ──")
		renderConnectorTable(stdout, connectors)
	}
	return 0
}

// --- show helpers ---

// doHubShowDispatch tries each catalog in order (connector → action →
// suite). The first non-404 wins. On all-404, return a single
// not-found error mentioning all three catalogs.
func doHubShowDispatch(fqn string, asJSON bool, stdout, stderr io.Writer) int {
	if entry, ok, err := tryFetchConnector(fqn); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	} else if ok {
		return renderConnectorShow(entry, asJSON, stdout, stderr)
	}
	if entry, ok, err := tryFetchAction(fqn); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	} else if ok {
		return renderActionShow(entry, asJSON, stdout, stderr)
	}
	if entry, ok, err := tryFetchSuite(fqn); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	} else if ok {
		return renderSuiteShow(entry, asJSON, stdout, stderr)
	}
	fmt.Fprintf(stderr, "error: no Hub entry for %s in any catalog\n", fqn)
	return 1
}

func doHubShowConnector(fqn string, asJSON bool, stdout, stderr io.Writer) int {
	entry, ok, err := tryFetchConnector(fqn)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "error: no Hub connector entry for %s\n", fqn)
		return 1
	}
	return renderConnectorShow(entry, asJSON, stdout, stderr)
}

func doHubShowAction(fqn string, asJSON bool, stdout, stderr io.Writer) int {
	entry, ok, err := tryFetchAction(fqn)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "error: no Hub action entry for %s\n", fqn)
		return 1
	}
	return renderActionShow(entry, asJSON, stdout, stderr)
}

func doHubShowSuite(fqn string, asJSON bool, stdout, stderr io.Writer) int {
	entry, ok, err := tryFetchSuite(fqn)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "error: no Hub suite entry for %s\n", fqn)
		return 1
	}
	return renderSuiteShow(entry, asJSON, stdout, stderr)
}

// --- daemon fetch helpers ---

func fetchHubConnectors(query string, stderr io.Writer) ([]hubConnectorEntry, int) {
	path := "/hub/connectors"
	if query != "" {
		path += "?" + url.Values{"q": []string{query}}.Encode()
	}
	status, body, err := bindingDoRequest(http.MethodGet, path, nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return nil, 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(body))
		return nil, 1
	}
	var resp hubConnectorList
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return nil, 1
	}
	return resp.Connectors, 0
}

func fetchHubActions(query string, stderr io.Writer) ([]hubActionEntry, int) {
	path := "/hub/actions"
	if query != "" {
		path += "?" + url.Values{"q": []string{query}}.Encode()
	}
	status, body, err := bindingDoRequest(http.MethodGet, path, nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return nil, 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(body))
		return nil, 1
	}
	var resp hubActionList
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return nil, 1
	}
	return resp.Actions, 0
}

func fetchHubSuites(query string, stderr io.Writer) ([]hubSuiteEntry, int) {
	path := "/hub/suites"
	if query != "" {
		path += "?" + url.Values{"q": []string{query}}.Encode()
	}
	status, body, err := bindingDoRequest(http.MethodGet, path, nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return nil, 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(body))
		return nil, 1
	}
	var resp hubSuiteList
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return nil, 1
	}
	return resp.Suites, 0
}

// errFetchFailed wraps a non-404 non-200 response so the dispatcher can
// distinguish "this catalog doesn't have the entry" (continue) from
// "the daemon is broken" (abort).
var errFetchFailed = errors.New("hub fetch failed")

func tryFetchConnector(fqn string) (hubConnectorEntry, bool, error) {
	q := url.Values{"fqn": []string{fqn}}.Encode()
	status, body, err := bindingDoRequest(http.MethodGet, "/hub/connector?"+q, nil)
	if err != nil {
		return hubConnectorEntry{}, false, err
	}
	if status == http.StatusNotFound {
		return hubConnectorEntry{}, false, nil
	}
	if status != http.StatusOK {
		return hubConnectorEntry{}, false, fmt.Errorf("%w: server returned %d: %s", errFetchFailed, status, string(body))
	}
	var entry hubConnectorEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return hubConnectorEntry{}, false, fmt.Errorf("parse response: %w", err)
	}
	return entry, true, nil
}

func tryFetchAction(fqn string) (hubActionEntry, bool, error) {
	q := url.Values{"fqn": []string{fqn}}.Encode()
	status, body, err := bindingDoRequest(http.MethodGet, "/hub/action?"+q, nil)
	if err != nil {
		return hubActionEntry{}, false, err
	}
	if status == http.StatusNotFound {
		return hubActionEntry{}, false, nil
	}
	if status != http.StatusOK {
		return hubActionEntry{}, false, fmt.Errorf("%w: server returned %d: %s", errFetchFailed, status, string(body))
	}
	var entry hubActionEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return hubActionEntry{}, false, fmt.Errorf("parse response: %w", err)
	}
	return entry, true, nil
}

func tryFetchSuite(fqn string) (hubSuiteEntry, bool, error) {
	q := url.Values{"fqn": []string{fqn}}.Encode()
	status, body, err := bindingDoRequest(http.MethodGet, "/hub/suite?"+q, nil)
	if err != nil {
		return hubSuiteEntry{}, false, err
	}
	if status == http.StatusNotFound {
		return hubSuiteEntry{}, false, nil
	}
	if status != http.StatusOK {
		return hubSuiteEntry{}, false, fmt.Errorf("%w: server returned %d: %s", errFetchFailed, status, string(body))
	}
	var entry hubSuiteEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return hubSuiteEntry{}, false, fmt.Errorf("parse response: %w", err)
	}
	return entry, true, nil
}

// --- rendering ---

const descMax = 60

func truncDescription(d string) string {
	if len(d) > descMax {
		return d[:descMax-1] + "…"
	}
	return d
}

func renderConnectorTable(w io.Writer, entries []hubConnectorEntry) {
	fmt.Fprintf(w, "%-50s  %-20s  %s\n", "FQN", "PUBLISHER", "DESCRIPTION")
	for _, e := range entries {
		fmt.Fprintf(w, "%-50s  %-20s  %s\n", e.FQN, e.PublisherGithub, truncDescription(e.Description))
	}
}

func renderActionTable(w io.Writer, entries []hubActionEntry) {
	fmt.Fprintf(w, "%-60s  %-20s  %s\n", "FQN", "PUBLISHER", "DESCRIPTION")
	for _, e := range entries {
		fmt.Fprintf(w, "%-60s  %-20s  %s\n", e.FQN, e.PublisherGithub, truncDescription(e.Description))
	}
}

func renderSuiteTable(w io.Writer, entries []hubSuiteEntry) {
	fmt.Fprintf(w, "%-50s  %-20s  %-8s  %s\n", "FQN", "PUBLISHER", "ACTIONS", "DESCRIPTION")
	for _, e := range entries {
		fmt.Fprintf(w, "%-50s  %-20s  %-8d  %s\n", e.FQN, e.PublisherGithub, len(e.MemberActions), truncDescription(e.Description))
	}
}

func renderConnectorShow(entry hubConnectorEntry, asJSON bool, stdout, stderr io.Writer) int {
	if asJSON {
		return encodeOne(stdout, stderr, entry)
	}
	fmt.Fprintf(stdout, "Kind:            connector\n")
	fmt.Fprintf(stdout, "FQN:             %s\n", entry.FQN)
	fmt.Fprintf(stdout, "Description:     %s\n", entry.Description)
	fmt.Fprintf(stdout, "Publisher:       %s\n", entry.PublisherGithub)
	fmt.Fprintf(stdout, "Key URL:         %s\n", entry.KeyURL)
	fmt.Fprintf(stdout, "Release pattern: %s\n", entry.ReleasePattern)
	return 0
}

func renderActionShow(entry hubActionEntry, asJSON bool, stdout, stderr io.Writer) int {
	if asJSON {
		return encodeOne(stdout, stderr, entry)
	}
	fmt.Fprintf(stdout, "Kind:           action\n")
	fmt.Fprintf(stdout, "FQN:            %s\n", entry.FQN)
	fmt.Fprintf(stdout, "Description:    %s\n", entry.Description)
	fmt.Fprintf(stdout, "Publisher:      %s\n", entry.PublisherGithub)
	fmt.Fprintf(stdout, "Connector:      %s\n", entry.ConnectorFQN)
	if len(entry.Intents) > 0 {
		fmt.Fprintf(stdout, "Intents:        %s\n", strings.Join(entry.Intents, ", "))
	}
	if entry.Category != "" {
		fmt.Fprintf(stdout, "Category:       %s\n", entry.Category)
	}
	return 0
}

func renderSuiteShow(entry hubSuiteEntry, asJSON bool, stdout, stderr io.Writer) int {
	if asJSON {
		return encodeOne(stdout, stderr, entry)
	}
	fmt.Fprintf(stdout, "Kind:                suite\n")
	fmt.Fprintf(stdout, "FQN:                 %s\n", entry.FQN)
	fmt.Fprintf(stdout, "Description:         %s\n", entry.Description)
	fmt.Fprintf(stdout, "Publisher:           %s\n", entry.PublisherGithub)
	if entry.Category != "" {
		fmt.Fprintf(stdout, "Category:            %s\n", entry.Category)
	}
	if len(entry.MemberActions) > 0 {
		fmt.Fprintf(stdout, "Member actions (%d):\n", len(entry.MemberActions))
		for _, a := range entry.MemberActions {
			fmt.Fprintf(stdout, "  - %s\n", a)
		}
	}
	if len(entry.ConnectorsRequired) > 0 {
		fmt.Fprintf(stdout, "Connectors required (%d):\n", len(entry.ConnectorsRequired))
		for _, c := range entry.ConnectorsRequired {
			fmt.Fprintf(stdout, "  - %s\n", c)
		}
	}
	return 0
}

// emitEmpty prints the friendly empty-state message in human mode or
// `[]` in JSON mode for the given catalog kind.
func emitEmpty(w io.Writer, asJSON bool, kind, emptyHint string) {
	if asJSON {
		fmt.Fprintln(w, "[]")
		return
	}
	if emptyHint != "" {
		fmt.Fprintf(w, "No Hub %s match %q.\n", kind, emptyHint)
		return
	}
	switch kind {
	case "connectors":
		fmt.Fprintln(w, "No connectors published to the Hub.")
	case "actions":
		fmt.Fprintln(w, "No actions published to the Hub.")
	case "suites":
		fmt.Fprintln(w, "No suites published to the Hub.")
	default:
		fmt.Fprintf(w, "No Hub %s.\n", kind)
	}
}

// encodeNDJSON writes one JSON-encoded object per line. Used for the
// `--json` single-type list/search output so consumers can pipe into
// `jq -c` without an intermediate parse.
func encodeNDJSON(stdout, stderr io.Writer, entries []any) int {
	enc := json.NewEncoder(stdout)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
	}
	return 0
}

func encodeOne(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(stderr, "encode: %v\n", err)
		return 1
	}
	return 0
}

// asAnySlice promotes a typed entry slice into `[]any` so encodeNDJSON
// can iterate with one generic loop. Go's lack of covariant slice
// conversion forces this; the alternative is duplicated NDJSON loops
// per type.
func asAnySlice[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
