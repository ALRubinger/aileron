package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// actionListItem is the subset of api.Action the CLI renders. Defined
// locally so this binary doesn't pull the full generated types graph
// just to display three columns and a flag.
type actionListItem struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Source  string `json:"source"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type actionListWireResponse struct {
	Items      []actionListItem      `json:"items"`
	LoadErrors []actionLoadErrorItem `json:"load_errors,omitempty"`
}

// actionLoadErrorItem mirrors the api.ActionLoadError shape the
// daemon returns. Carried separately from the manifest items so
// the table view can surface load failures without conflating them
// with successful loads. Inline-decoded rather than imported from
// internal/api/gen to keep the CLI binary's dep graph small (the
// CLI talks to the daemon over HTTP and doesn't otherwise need the
// generated client types).
type actionLoadErrorItem struct {
	Class   string `json:"class"`
	Message string `json:"message"`
	File    string `json:"file"`
	Line    int    `json:"line,omitempty"`
}

type actionRunRequest struct {
	Args map[string]any `json:"args"`
}

type actionRunResponse struct {
	AuditID string  `json:"audit_id"`
	Result  *string `json:"result,omitempty"`
}

type actionRunPendingResponse struct {
	Status     string `json:"status"`
	ApprovalID string `json:"approval_id"`
	ReviewURL  string `json:"review_url,omitempty"`
	Message    string `json:"message,omitempty"`
}

type actionRunFailureEnvelope struct {
	Error struct {
		Class   string `json:"class"`
		Message string `json:"message"`
		AuditID string `json:"audit_id,omitempty"`
	} `json:"error"`
}

// actionsHTTPClient is overridable in tests so they can exercise the
// full CLI flow without a running daemon. Production code reads the
// default value.
var actionsHTTPClient = &http.Client{Timeout: 30 * time.Second}

func actionEnabledLabel(enabled *bool) string {
	if enabled == nil || *enabled {
		return "true"
	}
	return "false"
}

// originBadge derives a short ORIGIN column value from the
// action's source URL. Used by `aileron action list` to render a
// stable column width alongside the longer source URL.
func originBadge(source string) string {
	if strings.HasPrefix(source, "hub://") {
		return "HUB"
	}
	return "—"
}

// runActionList renders `aileron action list`. Pulls the daemon's
// /v1/actions view (which already merges the user-preference overlay)
// and prints one row per installed action with its current enabled
// flag — so the user can see at a glance which ones are surfaced to
// the LLM.
func runActionList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("action list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Render results as NDJSON, one action per line")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	base, err := bindingAPIBaseURL()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	req, err := http.NewRequest(http.MethodGet, base+"/actions", nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	resp, err := actionsHTTPClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(stderr, "daemon returned %d: %s\n", resp.StatusCode, string(body))
		return 1
	}
	var out actionListWireResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		fmt.Fprintf(stderr, "decoding response: %v\n", err)
		return 1
	}

	if len(out.Items) == 0 && len(out.LoadErrors) == 0 {
		if *asJSON {
			fmt.Fprintln(stdout, "[]")
			return 0
		}
		fmt.Fprintln(stdout, "No actions installed.")
		fmt.Fprintln(stdout, "Run `aileron action add <FQN>` to install one.")
		return 0
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		for _, a := range out.Items {
			if err := enc.Encode(a); err != nil {
				fmt.Fprintf(stderr, "encode: %v\n", err)
				return 1
			}
		}
		return 0
	}

	if len(out.Items) > 0 {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tVERSION\tENABLED\tORIGIN\tSOURCE")
		disabled := 0
		for _, a := range out.Items {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				a.Name,
				a.Version,
				actionEnabledLabel(a.Enabled),
				originBadge(a.Source),
				a.Source,
			)
			if a.Enabled != nil && !*a.Enabled {
				disabled++
			}
		}
		tw.Flush()
		if disabled > 0 {
			fmt.Fprintln(stdout)
			fmt.Fprintf(stdout, "%d action(s) currently disabled. Restart your MCP server after toggling to pick up changes.\n", disabled)
		}
	}
	// Surface per-file load failures on stderr so users see them
	// immediately. Pre-fix, silent drops in the daemon's loader
	// were invisible from the CLI — a wrap-emitted action with a
	// validation error would be missing from `items` with no
	// indication anything was wrong, and the user would only
	// discover the gap when the agent couldn't find the tool.
	if len(out.LoadErrors) > 0 {
		if len(out.Items) > 0 {
			fmt.Fprintln(stderr)
		}
		fmt.Fprintf(stderr, "%d action file(s) failed to load and are not callable:\n", len(out.LoadErrors))
		for _, e := range out.LoadErrors {
			loc := e.File
			if e.Line > 0 {
				loc = fmt.Sprintf("%s:%d", e.File, e.Line)
			}
			fmt.Fprintf(stderr, "  [%s] %s — %s\n", e.Class, loc, e.Message)
		}
	}
	return 0
}

type actionRunArgFlag struct {
	values map[string]string
	count  int
}

func (f *actionRunArgFlag) String() string {
	if f == nil || len(f.values) == 0 {
		return ""
	}
	body, _ := json.Marshal(f.values)
	return string(body)
}

func (f *actionRunArgFlag) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("--arg must be k=v")
	}
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[strings.TrimSpace(key)] = val
	f.count++
	return nil
}

// runActionRun implements `aileron action run <NAME>`. It is a thin
// terminal-facing wrapper over the daemon's canonical
// POST /v1/actions/{name}/run endpoint.
func runActionRun(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("action run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var kvArgs actionRunArgFlag
	flags.Var(&kvArgs, "arg", "Action argument as k=v; repeatable")
	rawArgs := flags.String("args", "", "Raw JSON object for action args")
	asJSON := flags.Bool("json", false, "Print the daemon response envelope verbatim")
	auditIDOut := flags.String("audit-id-out", "", "Write the action audit_id to this path")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 1 || strings.TrimSpace(flags.Arg(0)) == "" {
		fmt.Fprintln(stderr, "usage: aileron action run <NAME> [--arg k=v ...] [--args <json>] [--json] [--audit-id-out <path>]")
		return 1
	}
	if kvArgs.count > 0 && strings.TrimSpace(*rawArgs) != "" {
		fmt.Fprintln(stderr, "error: --arg and --args are mutually exclusive")
		return 1
	}

	runArgs := map[string]any{}
	if strings.TrimSpace(*rawArgs) != "" {
		if err := json.Unmarshal([]byte(*rawArgs), &runArgs); err != nil {
			fmt.Fprintf(stderr, "error: --args must be a JSON object: %v\n", err)
			return 1
		}
		if runArgs == nil {
			fmt.Fprintln(stderr, "error: --args must be a JSON object")
			return 1
		}
	} else if kvArgs.count > 0 {
		for k, v := range kvArgs.values {
			runArgs[k] = v
		}
	}

	base, err := bindingAPIBaseURL()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	name := strings.TrimSpace(flags.Arg(0))
	body, _ := json.Marshal(actionRunRequest{Args: runArgs})
	req, err := http.NewRequest(http.MethodPost, base+"/actions/"+url.PathEscape(name)+"/run", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := actionsHTTPClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "error: reading response: %v\n", err)
		return 1
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		if *auditIDOut != "" {
			var out actionRunResponse
			if err := json.Unmarshal(rawBody, &out); err != nil {
				fmt.Fprintf(stderr, "decoding response: %v\n", err)
				return 1
			}
			if err := writeAuditID(*auditIDOut, out.AuditID); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
		}
		if *asJSON {
			writeRawJSON(stdout, rawBody)
			return 0
		}
		var out actionRunResponse
		if err := json.Unmarshal(rawBody, &out); err != nil {
			fmt.Fprintf(stderr, "decoding response: %v\n", err)
			return 1
		}
		result := ""
		if out.Result != nil {
			result = *out.Result
		}
		fmt.Fprintln(stdout, "wrapped output:")
		if result != "" {
			fmt.Fprintln(stdout, result)
		}
		return 0
	case http.StatusAccepted:
		if *asJSON {
			writeRawJSON(stdout, rawBody)
			return 75
		}
		var pending actionRunPendingResponse
		if err := json.Unmarshal(rawBody, &pending); err != nil {
			fmt.Fprintf(stderr, "decoding pending response: %v\n", err)
			return 1
		}
		if pending.ReviewURL != "" {
			fmt.Fprintf(stderr, "approval pending; review at %s or run `aileron approval approve %s`\n", pending.ReviewURL, pending.ApprovalID)
		} else {
			fmt.Fprintf(stderr, "approval pending; run `aileron approval approve %s`\n", pending.ApprovalID)
		}
		return 75
	default:
		if env, ok := decodeActionRunFailure(rawBody); ok && env.Error.Class != "" {
			fmt.Fprintf(stderr, "%s: %s\n", env.Error.Class, env.Error.Message)
		}
		fmt.Fprintf(stderr, "daemon returned %d: %s\n", resp.StatusCode, strings.TrimSpace(string(rawBody)))
		return 1
	}
}

func writeRawJSON(w io.Writer, raw []byte) {
	_, _ = w.Write(raw)
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		fmt.Fprintln(w)
	}
}

func writeAuditID(path, auditID string) error {
	if path == "" {
		return nil
	}
	return os.WriteFile(path, []byte(auditID+"\n"), 0o600)
}

func decodeActionRunFailure(raw []byte) (actionRunFailureEnvelope, bool) {
	var env actionRunFailureEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, false
	}
	return env, true
}

// runActionToggle implements `aileron action enable|disable <NAME>` by
// PATCHing the daemon's overlay state. The same function powers both
// directions; `enabled` is the value to apply.
func runActionToggle(args []string, enabled bool, stdout, stderr io.Writer) int {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintf(stderr, "usage: aileron action %s <NAME>\n", verb)
		return 1
	}
	name := strings.TrimSpace(args[0])

	base, err := bindingAPIBaseURL()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	body, _ := json.Marshal(map[string]bool{"enabled": enabled})
	req, err := http.NewRequest(http.MethodPatch, base+"/actions/"+name, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := actionsHTTPClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		fmt.Fprintf(stderr, "action %q is not installed; run `aileron action list` to see installed actions\n", name)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "daemon returned %d: %s\n", resp.StatusCode, strings.TrimSpace(string(rawBody)))
		return 1
	}

	if enabled {
		fmt.Fprintf(stdout, "Enabled %q.\n", name)
	} else {
		fmt.Fprintf(stdout, "Disabled %q.\n", name)
	}
	fmt.Fprintln(stdout, "Restart your MCP server to pick up the change.")
	return 0
}
