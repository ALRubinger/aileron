package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// runApproval dispatches `aileron approval <subcommand>` to one of the
// pending-action-approval management subcommands. Pending approvals are
// minted by the runtime when an action's manifest declares
// `[approval] required = true` — `RunAction` holds the agent's tool
// call open while the queue waits for a yes/no decision.
//
// Until the webapp surface (#418) ships, this CLI is the iteration
// surface for the action-approval flow. Same `/v1/action-approvals`
// endpoints, two front ends.
func runApproval(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron approval <list|approve|deny> [args...]")
		return 1
	}
	switch args[0] {
	case "list":
		return runApprovalList(args[1:], stdout, stderr)
	case "approve":
		return runApprovalDecide(args[1:], true, stdout, stderr)
	case "deny":
		return runApprovalDecide(args[1:], false, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown approval subcommand: %q\n", args[0])
		fmt.Fprintln(stderr, "usage: aileron approval <list|approve|deny> [args...]")
		return 1
	}
}

// runApprovalList prints the queue of pending approvals.
func runApprovalList(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: aileron approval list")
		return 1
	}
	resp, err := approvalDoRequest(http.MethodGet, "/action-approvals", nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(stderr, "server returned %d: %s\n", resp.StatusCode, body)
		return 1
	}

	var out struct {
		Items []struct {
			ID           string         `json:"id"`
			ActionName   string         `json:"action_name"`
			ConnectorFQN string         `json:"connector_fqn"`
			Args         map[string]any `json:"args"`
			SessionID    string         `json:"session_id"`
			RequestedAt  string         `json:"requested_at"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		fmt.Fprintf(stderr, "error: decode response: %v\n", err)
		return 1
	}
	if len(out.Items) == 0 {
		fmt.Fprintln(stdout, "No pending approvals.")
		return 0
	}
	fmt.Fprintf(stdout, "Pending approvals (%d):\n", len(out.Items))
	for _, it := range out.Items {
		fmt.Fprintf(stdout, "\n  %s\n", it.ID)
		fmt.Fprintf(stdout, "    Action:    %s\n", it.ActionName)
		if it.ConnectorFQN != "" {
			fmt.Fprintf(stdout, "    Connector: %s\n", it.ConnectorFQN)
		}
		if it.SessionID != "" {
			fmt.Fprintf(stdout, "    Session:   %s\n", it.SessionID)
		}
		fmt.Fprintf(stdout, "    Requested: %s\n", it.RequestedAt)
		if len(it.Args) > 0 {
			fmt.Fprintln(stdout, "    Args:")
			for k, v := range it.Args {
				fmt.Fprintf(stdout, "      %s: %v\n", k, v)
			}
		}
	}
	fmt.Fprintln(stdout, "\nApprove with: aileron approval approve <id>")
	fmt.Fprintln(stdout, "Deny with:    aileron approval deny <id> [--reason \"...\"]")
	return 0
}

// runApprovalDecide submits an approve or deny decision for one
// pending approval. Optional `--reason "..."` (or trailing positional
// reason) is forwarded to the agent so it can surface the rationale
// to the user on the deny path.
func runApprovalDecide(args []string, approved bool, stdout, stderr io.Writer) int {
	verb := "approve"
	if !approved {
		verb = "deny"
	}
	if len(args) == 0 {
		fmt.Fprintf(stderr, "usage: aileron approval %s <id> [--reason \"...\"]\n", verb)
		return 1
	}
	id := args[0]
	reason := ""
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--reason":
			if i+1 >= len(rest) {
				fmt.Fprintln(stderr, "error: --reason requires a value")
				return 1
			}
			reason = rest[i+1]
			i++
		default:
			// Treat any non-flag trailing arg as the reason for
			// ergonomics: `aileron approval deny act-... wrong-recipient`
			// works without needing the flag.
			if reason == "" && !strings.HasPrefix(rest[i], "--") {
				reason = rest[i]
			} else {
				fmt.Fprintf(stderr, "unrecognized argument: %s\n", rest[i])
				return 1
			}
		}
	}

	body, _ := json.Marshal(map[string]any{
		"approved": approved,
		"reason":   reason,
	})
	resp, err := approvalDoRequest(http.MethodPost, "/action-approvals/"+id+"/decide", body)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		fmt.Fprintf(stderr, "error: approval %q is unknown or already resolved\n", id)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(stderr, "server returned %d: %s\n", resp.StatusCode, respBody)
		return 1
	}
	if approved {
		fmt.Fprintf(stdout, "✓ Approved %s\n", id)
	} else {
		fmt.Fprintf(stdout, "✓ Denied %s\n", id)
	}
	return 0
}

// approvalDoRequest is a thin HTTP-client helper that posts to the
// running aileron server. Reuses the binding-side default URL helper
// (`bindingAPIBaseURL` in main.go) so AILERON_API_URL overrides apply
// uniformly across all subcommands.
func approvalDoRequest(method, path string, body []byte) (*http.Response, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setDaemonAuthorization(req)
	return http.DefaultClient.Do(req)
}
