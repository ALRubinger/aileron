package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/launch/agents"
	"golang.org/x/term"
)

// isTTYFn reports whether the CLI is attached to an interactive terminal on
// stdin. It is a package-level seam so tests can force the prompt path
// (true) or the non-interactive default path (false) without a real TTY.
var isTTYFn = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// claudeVaultPresence reports which of Claude's two credential slots are
// already populated in the daemon's vault: the subscription OAuth envelope
// (agents/claude/oauth) and/or the raw API key (agents/claude/apikey).
// resolveClaudeAuthMode consults it so a launch with a credential already
// stored does not re-ask a question the CLI can answer itself (#1381).
type claudeVaultPresence struct {
	subscription bool // agents/claude/oauth populated
	apiKey       bool // agents/claude/apikey populated
}

// claude credential purposes on the daemon's vault wire surface. These are
// the third path segment of agents.claudeVaultPath ("agents/claude/oauth")
// and agents.claudeAPIKeyVaultPath ("agents/claude/apikey"); the daemon's
// GET /v1/vault/agents/claude/credentials route selects between them via the
// `purpose` query parameter.
const (
	claudePurposeSubscription = "oauth"
	claudePurposeAPIKey       = "apikey"
)

// claudeVaultProbeTimeout caps the loopback round-trips the presence probe
// makes to the daemon. ensureVaultUnlockedFn has already guaranteed the
// daemon is up with the vault unlocked, so these are small, fast GETs; a
// generous bound keeps a wedged daemon from stalling the launch.
const claudeVaultProbeTimeout = 5 * time.Second

// claudeVaultPresenceFn is the package-level seam for the vault-presence
// probe so tests can drive resolveClaudeAuthMode across all four slot
// states without a running daemon. Production wiring talks to the daemon
// via the same discovery.Read(stateDir) -> URL/token pattern open.go uses.
var claudeVaultPresenceFn = probeClaudeVaultPresence

// probeClaudeVaultPresence asks the running daemon which Claude credential
// slots are populated. It mirrors the daemon-access pattern in open.go:
// resolve the state dir, read daemon.json for the URL + token, then GET the
// per-agent credential endpoint once per purpose.
//
// A populated slot returns 200; an empty slot returns 404 with
// {error:{code:"vault_not_found"}}. The probe is best-effort: any failure
// to resolve or reach the daemon (or any non-200/404 status) is reported as
// "slot absent" for that purpose so resolution falls back to its prior
// behavior (prompt on a TTY, subscription default otherwise) rather than
// erroring the launch. ensureVaultUnlockedFn runs before dispatch, so in the
// normal case the daemon is up and the vault is unlocked.
func probeClaudeVaultPresence(ctx context.Context) claudeVaultPresence {
	stateDir, err := defaultStateDirFn()
	if err != nil {
		return claudeVaultPresence{}
	}
	info, err := discoveryReadFn(stateDir)
	if err != nil {
		return claudeVaultPresence{}
	}
	base := strings.TrimRight(info.URL, "/")
	client := &http.Client{Timeout: claudeVaultProbeTimeout}
	probe := func(purpose string) bool {
		return claudeCredentialPresent(ctx, client, base, info.Token, purpose)
	}
	return claudeVaultPresence{
		subscription: probe(claudePurposeSubscription),
		apiKey:       probe(claudePurposeAPIKey),
	}
}

// claudeCredentialPresent issues a single GET against the daemon's per-agent
// credential endpoint for the given purpose and reports whether the slot is
// populated. Only a 200 counts as present; a 404 (empty slot) and any other
// outcome (locked vault, transport error, unexpected status) count as absent
// so a probe failure never escalates into a launch failure.
func claudeCredentialPresent(ctx context.Context, client *http.Client, base, token, purpose string) bool {
	endpoint := base + "/v1/vault/agents/claude/credentials"
	if purpose != "" && purpose != claudePurposeSubscription {
		// The default purpose (oauth) is omitted from the query string so the
		// request matches the daemon's pre-purpose wire shape; a non-default
		// purpose is appended explicitly.
		endpoint += "?purpose=" + purpose
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// resolveClaudeAuthMode selects the Claude auth mode to thread onto the
// launched Claude agent (#1340, #1381). It only performs mode *selection*;
// the AuthSpec branching for the selected mode lives in
// agents.Claude.AuthSpec.
//
// Resolution rules, in order:
//
//   - An explicit flag always wins and short-circuits before any vault
//     probe: flagVal == "api-key" -> ClaudeAuthModeAPIKey, "subscription" ->
//     ClaudeAuthModeSubscription, any other non-empty value -> usage error
//     (fail fast).
//   - On an empty flag the resolver consults the vault (#1381) so it does
//     not re-ask a question a stored credential already answers:
//   - only the subscription slot populated -> ClaudeAuthModeSubscription
//   - only the api-key slot populated       -> ClaudeAuthModeAPIKey
//   - both slots populated                  -> ClaudeAuthModeSubscription
//     (subscription is the documented default tie-break)
//     This holds on BOTH the TTY and non-TTY paths: a lone stored credential
//     resolves without prompting and WITHOUT reading stdin.
//   - Empty flag with NEITHER slot populated falls back to the prior
//     behavior: a TTY runs the interactive first-run prompt; a non-TTY
//     defaults to subscription WITHOUT reading stdin (the piped stdin
//     destined for the agent is never consumed).
func resolveClaudeAuthMode(flagVal string, isTTY bool, stdin io.Reader, stdout io.Writer) (agents.ClaudeAuthMode, error) {
	if flagVal != "" {
		mode, ok := parseClaudeAuthMode(flagVal)
		if !ok {
			return 0, fmt.Errorf("invalid --claude-auth value %q: want %q or %q", flagVal, "subscription", "api-key")
		}
		return mode, nil
	}

	// Empty flag: consult the vault before prompting or defaulting. The probe
	// is best-effort; a daemon/transport failure reports both slots absent,
	// which falls through to the prior prompt/default behavior below.
	ctx, cancel := context.WithTimeout(context.Background(), claudeVaultProbeTimeout)
	defer cancel()
	presence := claudeVaultPresenceFn(ctx)
	switch {
	case presence.subscription:
		// Both-populated and subscription-only both resolve to subscription;
		// subscription is the documented default tie-break.
		return agents.ClaudeAuthModeSubscription, nil
	case presence.apiKey:
		return agents.ClaudeAuthModeAPIKey, nil
	}

	if !isTTY {
		// Non-interactive, no stored credential: default to subscription and
		// DO NOT read stdin.
		return agents.ClaudeAuthModeSubscription, nil
	}
	return promptClaudeAuthMode(stdin, stdout)
}

// parseClaudeAuthMode maps an explicit flag value to a mode. It accepts the
// two canonical spellings plus a small set of obvious synonyms so a hurried
// invocation is not rejected. The canonical, documented values are
// "subscription" and "api-key".
func parseClaudeAuthMode(v string) (agents.ClaudeAuthMode, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "subscription", "sub", "s":
		return agents.ClaudeAuthModeSubscription, true
	case "api-key", "apikey", "api_key", "key", "k":
		return agents.ClaudeAuthModeAPIKey, true
	default:
		return 0, false
	}
}

// promptClaudeAuthMode runs the interactive first-run selection, writing the
// choices to stdout and reading one line from stdin per attempt. It re-asks
// on unrecognized input and returns an error only if stdin yields EOF before
// a valid choice (e.g. the input stream closes).
func promptClaudeAuthMode(stdin io.Reader, stdout io.Writer) (agents.ClaudeAuthMode, error) {
	br := bufio.NewReader(stdin)
	fmt.Fprintln(stdout, "How should Claude authenticate?")
	fmt.Fprintln(stdout, "  [1] subscription  Claude Pro/Max via OAuth login (default)")
	fmt.Fprintln(stdout, "  [2] api-key       Anthropic API key (ANTHROPIC_API_KEY)")
	for {
		fmt.Fprint(stdout, "Choose [1/2] (default 1: subscription): ")
		line, err := br.ReadString('\n')
		choice := strings.TrimSpace(line)
		if choice == "" {
			if err != nil {
				// EOF with no input on this attempt: take the default rather
				// than spinning forever on a closed stream.
				return agents.ClaudeAuthModeSubscription, nil
			}
			// Bare Enter selects the default.
			return agents.ClaudeAuthModeSubscription, nil
		}
		switch strings.ToLower(choice) {
		case "1":
			return agents.ClaudeAuthModeSubscription, nil
		case "2":
			return agents.ClaudeAuthModeAPIKey, nil
		}
		if mode, ok := parseClaudeAuthMode(choice); ok {
			return mode, nil
		}
		fmt.Fprintf(stdout, "  unrecognized choice %q; please answer 1 (subscription) or 2 (api-key)\n", choice)
		if err != nil {
			// Stream closed mid-prompt with no valid choice; fall back to the
			// documented default rather than erroring the whole launch.
			return agents.ClaudeAuthModeSubscription, nil
		}
	}
}
