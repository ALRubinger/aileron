package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// hubEntry mirrors the wire shape of api.HubConnectorEntry. Declared
// locally so this CLI binary doesn't pull the full generated types
// graph just to render Hub discovery output, matching the convention
// followed by binding/sync/connector-check rendering helpers.
type hubEntry struct {
	FQN             string `json:"fqn"`
	Description     string `json:"description"`
	PublisherGithub string `json:"publisher_github"`
	KeyURL          string `json:"key_url"`
	ReleasePattern  string `json:"release_pattern"`
}

// hubList mirrors api.HubConnectorList — the daemon wraps the entries
// in `{ "connectors": [...] }`.
type hubList struct {
	Connectors []hubEntry `json:"connectors"`
}

// runHub dispatches `aileron hub <subcommand>`. The Hub is the
// community connector index (ADR-0013): the daemon shallow-clones
// aileron-connectors-hub per query and serves discovery via
// /v1/hub/*. This CLI is a thin client over those endpoints.
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

// runHubList prints every entry in the Hub. Default is a fixed-width
// table; `--json` emits one JSON-encoded entry per line so scripts can
// pipe into `jq` without parsing prose.
func runHubList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("hub list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Render entries as NDJSON, one connector per line")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aileron hub list [--json]")
		return 1
	}
	return doHubList(*asJSON, "", "", stdout, stderr)
}

// runHubSearch is a keyword-filtered list. The filter is applied
// server-side (case-insensitive match on FQN and description) so the
// client doesn't have to know the matching rules.
func runHubSearch(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("hub search", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Render entries as NDJSON, one connector per line")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: aileron hub search <query> [--json]")
		return 1
	}
	query := flags.Arg(0)
	if strings.TrimSpace(query) == "" {
		fmt.Fprintln(stderr, "error: query cannot be empty")
		return 1
	}
	return doHubList(*asJSON, query, query, stdout, stderr)
}

// doHubList is the shared body of `hub list` and `hub search`. The
// search variant passes a non-empty query that the daemon filters on;
// the unfiltered list passes "" and gets every entry back.
//
// emptyHint is the query echoed back to the user when no results
// match. For plain `hub list` it's "" — there's no query to echo.
func doHubList(asJSON bool, query, emptyHint string, stdout, stderr io.Writer) int {
	path := "/hub/connectors"
	if query != "" {
		path += "?" + url.Values{"q": []string{query}}.Encode()
	}
	status, body, err := bindingDoRequest(http.MethodGet, path, nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(body))
		return 1
	}
	var resp hubList
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return 1
	}
	if len(resp.Connectors) == 0 {
		if asJSON {
			fmt.Fprintln(stdout, "[]")
			return 0
		}
		if emptyHint != "" {
			fmt.Fprintf(stdout, "No Hub connectors match %q.\n", emptyHint)
		} else {
			fmt.Fprintln(stdout, "No connectors published to the Hub.")
		}
		return 0
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		for _, e := range resp.Connectors {
			if err := enc.Encode(e); err != nil {
				fmt.Fprintf(stderr, "encode: %v\n", err)
				return 1
			}
		}
		return 0
	}
	renderHubTable(stdout, resp.Connectors)
	return 0
}

// runHubShow prints the full record for a single Hub entry, looked up
// by FQN. Default rendering is one field per line; `--json` emits the
// raw entry object so it round-trips through `jq`.
func runHubShow(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("hub show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Render the entry as a single JSON object")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: aileron hub show <fqn> [--json]")
		return 1
	}
	fqn := flags.Arg(0)
	if strings.TrimSpace(fqn) == "" {
		fmt.Fprintln(stderr, "error: fqn cannot be empty")
		return 1
	}

	q := url.Values{"fqn": []string{fqn}}.Encode()
	status, body, err := bindingDoRequest(http.MethodGet, "/hub/connector?"+q, nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status == http.StatusNotFound {
		fmt.Fprintf(stderr, "error: no Hub entry for %s\n", fqn)
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(body))
		return 1
	}
	var entry hubEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		if err := enc.Encode(entry); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "FQN:             %s\n", entry.FQN)
	fmt.Fprintf(stdout, "Description:     %s\n", entry.Description)
	fmt.Fprintf(stdout, "Publisher:       %s\n", entry.PublisherGithub)
	fmt.Fprintf(stdout, "Key URL:         %s\n", entry.KeyURL)
	fmt.Fprintf(stdout, "Release pattern: %s\n", entry.ReleasePattern)
	return 0
}

// renderHubTable prints a fixed-width FQN/PUBLISHER/DESCRIPTION table.
// Description is truncated with an ellipsis when it would otherwise
// wrap a typical terminal — the Hub schema caps it at 200 chars, well
// beyond what a list view should occupy. Show command (one row at a
// time) prints the description in full.
func renderHubTable(w io.Writer, entries []hubEntry) {
	const descMax = 60
	fmt.Fprintf(w, "%-50s  %-20s  %s\n", "FQN", "PUBLISHER", "DESCRIPTION")
	for _, e := range entries {
		desc := e.Description
		if len(desc) > descMax {
			desc = desc[:descMax-1] + "…"
		}
		fmt.Fprintf(w, "%-50s  %-20s  %s\n", e.FQN, e.PublisherGithub, desc)
	}
}
