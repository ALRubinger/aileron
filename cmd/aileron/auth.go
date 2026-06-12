package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/hostimport"
)

const authUsage = `usage:
  aileron auth <agent> --import-from-host

Seed the vault from an already-authenticated host install.
<agent> must be claude or codex.`

// runAuth dispatches `aileron auth <agent> ...`. The only verb in v1 is
// `--import-from-host`, which reads an already-authenticated host
// install's credential state for claude or codex, validates it against
// the same schema the agent's AuthSpec Capture enforces, and PUTs it to
// the vault through the daemon at agents/<name>/oauth.
//
// The auth namespace is reserved to later grow --status / --logout;
// those are out of scope for #986.
func runAuth(args []string, registry *launch.Registry, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, authUsage)
		return 1
	}

	// Require <agent> as the first positional so the flag parser does
	// not have to reorder. Flags follow the agent name.
	agentName := args[0]
	flags := flag.NewFlagSet("auth", flag.ContinueOnError)
	flags.SetOutput(stderr)
	importFromHost := flags.Bool("import-from-host", false,
		"Read the host install's credential state and seed the vault")
	if err := flags.Parse(args[1:]); err != nil {
		return 1
	}
	if !*importFromHost {
		fmt.Fprintln(stderr, "error: --import-from-host is required (the only auth mode in v1)")
		fmt.Fprintln(stderr, authUsage)
		return 1
	}

	return runAuthImport(agentName, registry, stdout, stderr)
}

// runAuthImport executes the host-import flow for one agent.
func runAuthImport(agentName string, registry *launch.Registry, stdout, stderr io.Writer) int {
	// Resolve the agent and its credential FileBinding so we validate
	// host bytes through the exact Capture the launcher uses. This is
	// also where we reject unsupported agents: only claude and codex
	// have a host install hostimport can read.
	binding, err := agentOAuthBinding(registry, agentName)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	raw, err := hostimport.Extract(agentName, hostimport.Options{})
	if err != nil {
		if errors.Is(err, hostimport.ErrNotAuthenticated) {
			fmt.Fprintf(stderr, "error: no host credentials found for %s\n", agentName)
			fmt.Fprintf(stderr, "log in first, then re-run; or use interactive in-container login: aileron launch %s --sandbox=docker\n", agentName)
			return 1
		}
		// Any other extraction error (including the Linux-keyring
		// deferral, whose message already names the recovery paths) is
		// surfaced verbatim.
		fmt.Fprintf(stderr, "error reading host credentials: %v\n", err)
		return 1
	}

	// Validate via the agent's Capture — identical to the launcher's
	// clean-exit capture. Capture returns a vault.Secret whose Value is
	// the validated bytes; we PUT those bytes byte-verbatim.
	secret, err := binding.Capture(raw)
	if err != nil {
		fmt.Fprintf(stderr, "error: host credentials for %s are not a valid envelope: %v\n", agentName, err)
		return 1
	}

	// Marshaling a struct with a single []byte field cannot fail, so the
	// error is discarded here (matching runBindingSetup's precedent).
	body, _ := json.Marshal(agentCredentialsBody{Value: secret.Value})
	status, respBody, err := vaultDoRequest(http.MethodPut,
		"/vault/agents/"+agentName+"/credentials", body)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	switch status {
	case http.StatusNoContent:
		fmt.Fprintf(stdout, "Imported host credentials to agents/%s/oauth\n", agentName)
		return 0
	case http.StatusLocked:
		fmt.Fprintln(stderr, "error: vault is locked; unlock it first")
		return 1
	case http.StatusServiceUnavailable:
		fmt.Fprintln(stderr, "error: daemon is not configured with a vault")
		return 1
	default:
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return 1
	}
}

// agentOAuthBinding resolves the agent's credential FileBinding — the
// one whose VaultPath is agents/<name>/oauth — so the caller can run
// host bytes through its Capture. It returns a clear unsupported-agent
// error for any agent hostimport cannot read (goose/opencode/pi) and
// for unknown names.
func agentOAuthBinding(registry *launch.Registry, agentName string) (launch.FileBinding, error) {
	switch agentName {
	case hostimport.AgentClaude, hostimport.AgentCodex:
		// supported
	default:
		return launch.FileBinding{}, fmt.Errorf("host import is not supported for %q (supported: %s, %s)",
			agentName, hostimport.AgentClaude, hostimport.AgentCodex)
	}

	agent, ok := registry.Get(agentName)
	if !ok {
		return launch.FileBinding{}, fmt.Errorf("unknown agent: %q", agentName)
	}
	want := "agents/" + agentName + "/oauth"
	for _, fb := range agent.AuthSpec().FileBindings {
		if fb.VaultPath == want {
			if fb.Capture == nil {
				return launch.FileBinding{}, fmt.Errorf("agent %q binding %s has no Capture validator", agentName, want)
			}
			return fb, nil
		}
	}
	return launch.FileBinding{}, fmt.Errorf("agent %q has no credential binding at %s", agentName, want)
}
