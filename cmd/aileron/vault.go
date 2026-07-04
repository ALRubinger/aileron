package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/vaultscope"
)

// Environment variable that supplies the vault passphrase non-
// interactively. Honored by every command that would otherwise prompt.
// Documented in #492 item 5c as the CI / scripts escape hatch.
const envVaultPassphrase = "AILERON_VAULT_PASSPHRASE"

const vaultUsage = `usage:
  aileron vault init [--passphrase-file <path>]
  aileron vault put agents/<name>/<purpose> --from-file <path>
  aileron vault put user/<service> [--from-file <path>]
  aileron vault delete <path-as-listed> [--yes]
  aileron vault list [--scope agent|user|all] [--prefix agents/] [--include-control-plane] [--json]

vault list with no --scope prints the union of every namespace in the
vault (agent, user, connector/binding credentials), each line prefixed
with its scope. --scope narrows to a single typed namespace.

vault delete accepts any path vault list prints: an agent entry
(agents/<name>/<purpose>, e.g. agents/claude/oauth), a user entry
(user/<service>, e.g. user/github), a locally-stored secret
(secret/<name>, e.g. secret/linear_token), or a binding
(<kind>/<service>/<identity>, e.g. aws_sigv4/athena/default). It
dispatches to the matching namespace's delete endpoint; secret/ entries
are removed from the local vault (where secret set writes them).
Control-plane entries (connected-accounts/, llm-config/) are managed by
the control plane, not this CLI.

vault put targets the agent namespace (agents/<name>/<purpose>, from a
file) or the user namespace (user/<service>). For a user credential the
value is entered at a hidden prompt by default, or read verbatim from
--from-file. Routing user credentials through this verb base64-encodes
them correctly on the wire; a hand-crafted curl PUT that passes a raw
secret (a 40-char AWS secret key is itself valid base64) silently decodes
to garbage. You still create a binding with ` + "`binding setup`" + `, not
` + "`vault put`" + `.`

// runVault dispatches `aileron vault <subcommand>`.
//
// init opens the local vault file directly (first-run flow); the
// put/delete/list verbs are thin daemon-backed HTTP clients that never
// open the vault file themselves. put targets the agent namespace
// (agents/<name>/<purpose>, from a file) or the user namespace
// (user/<service>, from a hidden prompt or a file) — you still create a
// binding with `binding setup`, not `vault put`; delete classifies the
// path and dispatches to the matching namespace-scoped daemon endpoint
// (agents/, user/, or a binding), so anything `vault list` prints is
// deletable.
func runVault(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, vaultUsage)
		return 1
	}
	switch args[0] {
	case "init":
		return runVaultInit(args[1:], stdout, stderr)
	case "put":
		return runVaultPut(args[1:], stdout, stderr)
	case "delete":
		return runVaultDelete(args[1:], stdin, stdout, stderr)
	case "list":
		return runVaultList(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown vault command: %q\n", args[0])
		fmt.Fprintln(stderr, vaultUsage)
		return 1
	}
}

// agentPathNameAndPurpose validates that arg names an agents-only
// credential entry and returns the agent <name> and credential
// <purpose>. The vault put verb is namespace-locked: it refuses anything
// outside the agents namespace client-side before issuing an HTTP call,
// since you create a binding with `binding setup`, never `vault put`.
// (vault delete is more general — it dispatches every listed namespace
// to that namespace's own delete endpoint; see vaultDeleteTargetFor.)
//
// Only the fully-qualified path form is accepted, exactly what `vault
// list` prints for an agent entry (#1317), now generalized to any
// purpose (#1361):
//   - agents/<name>/<purpose>, e.g. `agents/claude/oauth`,
//     `agents/claude/apikey`
//
// The parse and the <purpose> allow-list are delegated to the shared
// classifier (vaultscope.AgentNameAndPurposeFromVaultPath) so the CLI
// accepts exactly what `vault list` surfaces. This is a thin wrapper that
// keeps the `(name, purpose, err)` signature the put/delete call sites
// use; any non-conforming path collapses into the single
// `agents/<name>/<purpose>` rejection.
func agentPathNameAndPurpose(arg string) (name, purpose string, err error) {
	name, purpose, ok := vaultscope.AgentNameAndPurposeFromVaultPath(arg)
	if !ok {
		return "", "", fmt.Errorf("name must be agents/<name>/<purpose> (got %q)", arg)
	}
	return name, purpose, nil
}

// agentCredentialsBody is the local minimal subset of the
// api.AgentCredentials wire shape the put verb marshals. Defined here
// so cmd/aileron does not depend on internal/api/gen (matching the
// bindingRow precedent). encoding/json base64-encodes the []byte Value
// field, matching the spec's `format: byte` declaration.
type agentCredentialsBody struct {
	Value []byte `json:"value"`
}

// vaultSummaryMetadata is the local subset of api.AgentCredentialsMetadata
// the list verbs decode, shared by both the agent and user summaries.
type vaultSummaryMetadata struct {
	Type        string            `json:"type,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// agentSummary is the local subset of api.AgentCredentialSummary the
// list verb decodes. Notably it has no Value field — the daemon never
// returns credential bytes from the list endpoint (ADR-0011).
//
// Purpose is the `<purpose>` segment of the agents/<name>/<purpose>
// vault path. It is a pointer because the daemon omits it for entries
// that predate per-purpose listing; a nil Purpose is rendered as the
// default `oauth` so older daemons keep producing the historical lines.
type agentSummary struct {
	Name     string                `json:"name"`
	Purpose  *string               `json:"purpose,omitempty"`
	Metadata *vaultSummaryMetadata `json:"metadata,omitempty"`
}

// defaultAgentCredentialPurpose is the `<purpose>` segment used when the
// daemon omits it (entries predating per-purpose listing). Kept `oauth`
// so a nil purpose renders byte-for-byte like the pre-#1361 list output.
const defaultAgentCredentialPurpose = "oauth"

// purposeOrDefault resolves a summary's optional Purpose to its display
// value, falling back to oauth for entries an older daemon returned
// without one.
func (a agentSummary) purposeOrDefault() string {
	if a.Purpose != nil && *a.Purpose != "" {
		return *a.Purpose
	}
	return defaultAgentCredentialPurpose
}

// userSummary is the local subset of api.UserCredentialSummary the list
// verb decodes for the user scope. Like agentSummary it has no Value
// field — the daemon never returns credential bytes from the list
// endpoint (ADR-0011).
type userSummary struct {
	Service  string                `json:"service"`
	Metadata *vaultSummaryMetadata `json:"metadata,omitempty"`
}

// runVaultPut stores a credential envelope via the daemon. It dispatches
// on the target namespace:
//
//   - agents/<name>/<purpose> — file-only. Agent credential files (Claude's
//     .credentials.json, Codex's auth.json) are exact-byte JSON artifacts,
//     so the bytes are read verbatim from --from-file (no trailing-newline
//     munging) and there is no interactive entry.
//   - user/<service> — the value may be entered at a hidden prompt (the
//     default) or read from --from-file. User credentials are frequently
//     raw secrets an operator holds in hand (an AWS secret key, an API
//     token); typing one at a no-echo prompt keeps it out of argv and shell
//     history, and — crucially — routing it through this verb base64-encodes
//     it correctly on the wire. Hand-crafting the PUT with a raw
//     already-"looks-like-base64" secret (a 40-char AWS secret key is valid
//     base64) silently decodes to garbage server-side and later surfaces as
//     AWS SignatureDoesNotMatch (#1822).
//
// Any other namespace is rejected before any HTTP call: you create a binding
// with `binding setup`, not `vault put`.
func runVaultPut(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("vault put", flag.ContinueOnError)
	flags.SetOutput(stderr)
	fromFile := flags.String("from-file", "", "Read the credential bytes verbatim from the named file (required for agents/, optional for user/)")
	flags.Bool("yes", false, "Accepted for symmetry with delete; put never prompts for confirmation")
	positionals, err := parseInterspersedFlags(flags, args)
	if err != nil {
		return 1
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, "usage: aileron vault put agents/<name>/<purpose> --from-file <path>")
		fmt.Fprintln(stderr, "       aileron vault put user/<service> [--from-file <path>]")
		return 1
	}
	arg := positionals[0]
	scope, _ := vaultscope.Classify(arg)
	switch scope {
	case vaultscope.ScopeAgent:
		return runVaultPutAgent(arg, *fromFile, stdout, stderr)
	case vaultscope.ScopeUser:
		return runVaultPutUser(arg, *fromFile, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: vault put accepts agents/<name>/<purpose> or user/<service> (got %q)\n", arg)
		fmt.Fprintln(stderr, "create a binding with `binding setup`, not `vault put`.")
		return 1
	}
}

// runVaultPutAgent stores an agent credential envelope read verbatim from a
// file at agents/<name>/<purpose>. The file bytes are stored as-is
// (whole-file read, no trailing-newline munging) since agent credential
// files are exact-byte artifacts; interactive entry is deliberately not
// offered for this namespace.
func runVaultPutAgent(arg, fromFile string, stdout, stderr io.Writer) int {
	// Classify already confirmed the agents/<name>/<purpose> shape, so this
	// wrapper cannot error; the check is defensive.
	name, purpose, err := agentPathNameAndPurpose(arg)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if fromFile == "" {
		fmt.Fprintln(stderr, "error: --from-file <path> is required for agent credentials")
		return 1
	}
	data, err := os.ReadFile(fromFile)
	if err != nil {
		fmt.Fprintf(stderr, "error reading %q: %v\n", fromFile, err)
		return 1
	}
	if len(data) == 0 {
		fmt.Fprintf(stderr, "error: %q is empty\n", fromFile)
		return 1
	}
	return vaultPutCredential(agentCredentialPath(name, purpose), data,
		"agents/"+name+"/"+purpose, stdout, stderr)
}

// runVaultPutUser stores a user-level credential envelope at user/<service>.
// The value comes from --from-file (read verbatim) or, when that flag is
// absent, an interactive hidden prompt on the controlling terminal so the
// secret never reaches argv or shell history. Either way the bytes are
// base64-encoded on the wire by agentCredentialsBody, which is the whole
// point: it is the correct encoding the raw curl PUT footgun gets wrong
// (#1822).
func runVaultPutUser(arg, fromFile string, stdout, stderr io.Writer) int {
	// Classify already confirmed the user/<service> shape and that <service>
	// passes the allow-list; the check is defensive.
	service, ok := vaultscope.UserServiceFromVaultPath(arg)
	if !ok {
		fmt.Fprintf(stderr, "error: invalid user service path %q\n", arg)
		return 1
	}

	var data []byte
	if fromFile != "" {
		b, err := os.ReadFile(fromFile)
		if err != nil {
			fmt.Fprintf(stderr, "error reading %q: %v\n", fromFile, err)
			return 1
		}
		data = b
	} else {
		// Hidden, no-echo entry mirrors `secret set`: print the prompt to
		// stderr, then read with no additional prompt (promptPassphrase is an
		// overridable var so tests can inject the typed value).
		fmt.Fprintf(stderr, "Value for user/%s: ", service)
		value, err := promptPassphrase("", nil)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintln(stderr) // newline after hidden input
		data = []byte(value)
	}
	if len(data) == 0 {
		fmt.Fprintln(stderr, "error: value cannot be empty")
		return 1
	}
	return vaultPutCredential(userCredentialDaemonPath(service), data,
		"user/"+service, stdout, stderr)
}

// vaultPutCredential base64-encodes data into the shared AgentCredentials
// wire shape, PUTs it to httpPath via the daemon, and maps the response to a
// CLI exit code. label is the namespaced identifier echoed on success.
func vaultPutCredential(httpPath string, data []byte, label string, stdout, stderr io.Writer) int {
	body, err := json.Marshal(agentCredentialsBody{Value: data})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	status, respBody, err := vaultDoRequest(http.MethodPut, httpPath, body)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	switch status {
	case http.StatusNoContent:
		fmt.Fprintf(stdout, "Stored %s\n", label)
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

// userCredentialDaemonPath builds the daemon path for a user-level
// credential. The service segment is path-escaped defensively; callers pass
// values already constrained by vaultscope.UserServiceFromVaultPath, but
// escaping keeps a stray special character from corrupting the request. It
// is the write-side twin of the DELETE path vaultDeleteTargetFor builds.
func userCredentialDaemonPath(service string) string {
	return "/vault/user/" + url.PathEscape(service) + "/credentials"
}

// vaultDeleteTarget describes a classified, CLI-deletable vault path: the
// daemon DELETE path to issue, the prompt label, and the namespace-aware
// success and 404 messages. Each namespace's own namespace-scoped endpoint
// is reused (agents/, user/, or the binding endpoint that `binding revoke`
// already drives), so no path-scoping is bypassed (ADR-0025).
type vaultDeleteTarget struct {
	httpPath   string // daemon path, e.g. /vault/agents/claude/credentials?purpose=oauth
	label      string // confirmation/display label, e.g. agents/claude/oauth
	successMsg string // printed on 204
	notFound   string // printed on 404
}

// vaultDeleteTargetFor classifies arg into the matching deletable
// namespace, mirroring the shared vaultscope.Classify order so the CLI
// recognizes exactly what `vault list` surfaces. The order is
// load-bearing: agents/<name>/<purpose> also matches the binding name
// grammar, so it must be checked first.
//
// It returns (target, true) for a deletable path. For a path in a
// namespace the CLI cannot delete — the tenant-keyed control-plane
// namespaces (connected-accounts/, llm-config/), managed by the control
// plane rather than this CLI, and any unrecognized `other` path — it
// returns (zero, false); the caller prints the rejection.
func vaultDeleteTargetFor(arg string) (vaultDeleteTarget, bool) {
	// The shared classifier owns namespace detection (its ordering already
	// checks agents/<name>/<purpose> before the binding grammar, which the
	// two shapes share). Only the delete-specific HTTP paths, prompt labels,
	// and success/404 messages are assembled locally.
	scope, _ := vaultscope.Classify(arg)
	switch scope {
	case vaultscope.ScopeAgent:
		// agents/<name>/<purpose> — reuse the existing agent credential
		// endpoint. Classify already confirmed the path conforms, so the
		// wrapper cannot error here.
		name, purpose, _ := vaultscope.AgentNameAndPurposeFromVaultPath(arg)
		label := "agents/" + name + "/" + purpose
		return vaultDeleteTarget{
			httpPath:   agentCredentialPath(name, purpose),
			label:      label,
			successMsg: "Deleted " + label,
			notFound:   "error: no credential entry for " + label,
		}, true
	case vaultscope.ScopeUser:
		// user/<service> — DELETE /vault/user/<service>/credentials.
		service, _ := vaultscope.UserServiceFromVaultPath(arg)
		label := "user/" + service
		return vaultDeleteTarget{
			httpPath:   userCredentialDaemonPath(service),
			label:      label,
			successMsg: "Deleted " + label,
			notFound:   "error: no credential entry for " + label,
		}, true
	case vaultscope.ScopeBinding:
		// A binding (<kind>/<service>/<identity>) — DELETE /bindings/<name>,
		// identical to `aileron binding revoke`.
		return vaultDeleteTarget{
			httpPath:   "/bindings/" + url.PathEscape(arg),
			label:      arg,
			successMsg: "Deleted " + arg,
			notFound:   "error: binding not found: " + arg,
		}, true
	default:
		// Control-plane namespaces (connected-account, llm-config) are owned
		// by the control plane, not the operator CLI; secret/ is handled by
		// the local-vault branch before this call; and `other` is an
		// unrecognized shape. None is CLI-deletable here.
		return vaultDeleteTarget{}, false
	}
}

// runVaultDelete removes the credential at the given path via the daemon.
// The path is classified (agents/<name>/<purpose>, user/<service>, or a
// binding) and dispatched to that namespace's own delete endpoint, so any
// path `vault list` prints is deletable — symmetric with list. Unless
// --yes is passed it confirms interactively; this CLI prompt is the only
// human gate (the daemon applies no approval block for operator vault
// management per the plan).
func runVaultDelete(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("vault delete", flag.ContinueOnError)
	flags.SetOutput(stderr)
	yes := flags.Bool("yes", false, "Skip the confirmation prompt")
	positionals, err := parseInterspersedFlags(flags, args)
	if err != nil {
		return 1
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, "usage: aileron vault delete <path-as-listed> [--yes] (any path vault list prints)")
		return 1
	}

	// secret/<name> entries (written by `aileron secret set`) live in the
	// local file vault, not behind a daemon namespace endpoint. Delete them
	// through the same local-vault access `secret set` uses so
	// `vault list` and `vault delete` stay symmetric (#1704, #1716).
	if scope, _ := vaultscope.Classify(positionals[0]); scope == vaultscope.ScopeSecret {
		return runVaultDeleteSecret(positionals[0], *yes, stdin, stdout, stderr)
	}

	target, ok := vaultDeleteTargetFor(positionals[0])
	if !ok {
		fmt.Fprintf(stderr, "error: %q is not a CLI-deletable vault path\n", positionals[0])
		fmt.Fprintln(stderr, "vault delete accepts agents/<name>/<purpose>, user/<service>, secret/<name>, or a")
		fmt.Fprintln(stderr, "binding (<kind>/<service>/<identity>) — exactly what vault list prints. Control-plane")
		fmt.Fprintln(stderr, "entries (connected-accounts/, llm-config/) are managed by the control plane.")
		return 1
	}

	if !*yes {
		answer := promptLine(stdin, stdout, fmt.Sprintf("Delete %s? [y/N]: ", target.label))
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			fmt.Fprintln(stdout, "cancelled")
			return 0
		}
	}

	status, respBody, err := vaultDoRequest(http.MethodDelete, target.httpPath, nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	switch status {
	case http.StatusNoContent:
		fmt.Fprintln(stdout, target.successMsg)
		return 0
	case http.StatusNotFound:
		fmt.Fprintln(stderr, target.notFound)
		return 1
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

// runVaultDeleteSecret deletes a secret/<name> entry directly from the
// local file vault, mirroring how `aileron secret set` writes it
// (launch.DefaultVaultPath + readVaultPassphrase + launch.OpenLocalVault).
// `vault list` surfaces secret/ entries but the daemon exposes no
// namespace-scoped delete endpoint for them, so routing this branch to
// the local vault restores the list/delete symmetry #1704 established
// (the asymmetry called out in #1716). It is a `vault delete` branch, not
// a separate `secret rm` command, so operators paste back exactly what
// `vault list` prints.
func runVaultDeleteSecret(arg string, yes bool, stdin io.Reader, stdout, stderr io.Writer) int {
	// secret/<name> is a single-segment namespace, exactly like
	// `secret set` writes: reject a name that itself contains a '/'.
	name := strings.TrimPrefix(arg, vaultscope.SecretVaultPrefix)
	if name == "" || strings.Contains(name, "/") {
		fmt.Fprintf(stderr, "error: secret name must be a single segment (no '/'): %q\n", arg)
		return 1
	}
	storedPath := vaultscope.SecretVaultPrefix + name

	if !yes {
		answer := promptLine(stdin, stdout, fmt.Sprintf("Delete %s? [y/N]: ", storedPath))
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			fmt.Fprintln(stdout, "cancelled")
			return 0
		}
	}

	vaultPath := launch.DefaultVaultPath()
	passphrase, _, err := readVaultPassphrase("", "Vault passphrase: ", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if passphrase == "" {
		fmt.Fprintln(stderr, "error: passphrase cannot be empty")
		return 1
	}

	v, err := launch.OpenLocalVault(vaultPath, passphrase)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	// The local file vault's Delete is idempotent (no error when the path
	// is absent), but the daemon-backed branch reports a not-found miss.
	// Check existence first so `vault delete secret/<name>` gives the same
	// "no credential entry" signal for a name that was never stored.
	entries, err := v.List(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	found := false
	for _, e := range entries {
		if e.Path == storedPath {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(stderr, "error: no credential entry for %s\n", storedPath)
		return 1
	}

	if err := v.Delete(context.Background(), storedPath); err != nil {
		fmt.Fprintf(stderr, "error deleting secret: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Deleted %s\n", storedPath)
	return 0
}

// runVaultList prints the credential entries the daemon holds. Output
// mirrors `aileron secret list`: one identifier per line by default, or
// NDJSON (one entry per line) with --json.
//
// With no --scope (the default) it surfaces the grouped UNION of every
// locally-owned namespace via the /vault endpoint: agent credentials,
// user credentials, and capability bindings (which include connector
// credentials like connectors/<provider>/default and oauth2/<provider>/...
// bindings). Each entry's full vault path is printed, prefixed in text
// mode by its `scope:` label so the grouping is legible (#1402). This
// fixes the old default, which queried only the agents and user typed
// endpoints and silently hid connector/binding credentials.
//
// The two tenant-keyed control-plane namespaces (connected-accounts/ and
// llm-config/) are excluded from the union by default; --include-control-plane
// adds them.
//
// The --scope flag narrows to a single typed namespace, preserved for
// back-compat:
//   - agent: per-agent credential envelopes at agents/<name>/<purpose>,
//     one line per (name, purpose).
//   - user: user-level credentials at user/<service>, keyed by service.
//   - all: agents then user (the historical "both typed endpoints" view).
//
// The legacy --prefix flag still restricts to agents/ for backward
// symmetry; passing --prefix together with a non-agent --scope is a usage
// error since the two would disagree.
func runVaultList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("vault list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	prefix := flags.String("prefix", "", "Restrict to a path prefix (only agents/ is supported)")
	scope := flags.String("scope", "", "Narrow to a single namespace: agent, user, or all (default: union of every namespace)")
	includeControlPlane := flags.Bool("include-control-plane", false, "Also list the control-plane namespaces (connected-accounts/, llm-config/) in the union view")
	asJSON := flags.Bool("json", false, "Render entries as NDJSON, one entry per line")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if *prefix != "" && *prefix != "agents/" {
		fmt.Fprintf(stderr, "error: only the agents/ prefix is supported (got %q)\n", *prefix)
		return 1
	}

	// No --scope: the grouped union over /vault. The legacy --prefix agents/
	// is treated as the agent scope for back-compat, so it routes to the
	// typed agent path below rather than the union.
	if *scope == "" && *prefix != "agents/" {
		return runVaultListUnion(*includeControlPlane, *asJSON, stdout, stderr)
	}
	if *includeControlPlane {
		fmt.Fprintln(stderr, "error: --include-control-plane applies only to the default union view, not --scope/--prefix")
		return 1
	}

	listAgents, listUser := false, false
	switch *scope {
	case "", "agent": // "" reaches here only with --prefix agents/
		listAgents = true
	case "user":
		listUser = true
	case "all":
		listAgents, listUser = true, true
	default:
		fmt.Fprintf(stderr, "error: --scope must be agent, user, or all (got %q)\n", *scope)
		return 1
	}
	// The legacy --prefix agents/ filter narrows to the agent namespace; it
	// cannot coexist with a scope that also asks for user entries.
	if *prefix == "agents/" && listUser {
		fmt.Fprintf(stderr, "error: --prefix agents/ conflicts with --scope %s\n", *scope)
		return 1
	}

	// In text mode entries are collected and printed at the end (so the
	// "nothing stored" message fires only when every scope is empty). In
	// JSON mode each entry is streamed as it decodes; emitted counts the
	// rows written so the empty-array fallback is correct across scopes.
	var lines []string
	var enc *json.Encoder
	emitted := 0
	if *asJSON {
		enc = json.NewEncoder(stdout)
	}

	if listAgents {
		respBody, code := vaultListFetch("/vault/agents", stderr)
		if code != 0 {
			return code
		}
		var out struct {
			Agents []agentSummary `json:"agents"`
		}
		if err := json.Unmarshal(respBody, &out); err != nil {
			fmt.Fprintf(stderr, "error: decode response: %v\n", err)
			return 1
		}
		for _, a := range out.Agents {
			if *asJSON {
				if err := enc.Encode(a); err != nil {
					fmt.Fprintf(stderr, "encode: %v\n", err)
					return 1
				}
				emitted++
			} else {
				lines = append(lines, "agents/"+a.Name+"/"+a.purposeOrDefault())
			}
		}
	}

	if listUser {
		respBody, code := vaultListFetch("/vault/user", stderr)
		if code != 0 {
			return code
		}
		var out struct {
			Services []userSummary `json:"services"`
		}
		if err := json.Unmarshal(respBody, &out); err != nil {
			fmt.Fprintf(stderr, "error: decode response: %v\n", err)
			return 1
		}
		for _, u := range out.Services {
			if *asJSON {
				if err := enc.Encode(u); err != nil {
					fmt.Fprintf(stderr, "encode: %v\n", err)
					return 1
				}
				emitted++
			} else {
				lines = append(lines, "user/"+u.Service)
			}
		}
	}

	if *asJSON {
		if emitted == 0 {
			// json.Encoder already streamed any entries above. When there
			// were none, emit an empty array so scripts get parseable JSON,
			// mirroring `aileron secret list --json`.
			fmt.Fprintln(stdout, "[]")
		}
		return 0
	}

	if len(lines) == 0 {
		fmt.Fprintln(stdout, vaultListEmptyMessage(*scope))
		return 0
	}
	for _, l := range lines {
		fmt.Fprintln(stdout, l)
	}
	return 0
}

// vaultListEmptyMessage returns the human-readable "nothing stored"
// message for the given scope.
func vaultListEmptyMessage(scope string) string {
	switch scope {
	case "user":
		return "No user credentials stored."
	case "all":
		return "No credentials stored."
	default:
		return "No agent credentials stored."
	}
}

// vaultEntry is the local subset of api.VaultEntry the union list verb
// decodes. Like the agent/user summaries it has no Value field — the
// daemon never returns credential bytes from any list endpoint (ADR-0011).
type vaultEntry struct {
	Path     string                `json:"path"`
	Scope    string                `json:"scope"`
	Metadata *vaultSummaryMetadata `json:"metadata,omitempty"`
}

// runVaultListUnion renders the grouped union of every vault namespace via
// the GET /vault endpoint. In text mode each entry is one line of
// `scope:  path` so the namespace grouping is legible; in --json mode each
// entry is streamed as NDJSON (one object per line) exactly like the typed
// scopes. includeControlPlane threads through as the
// `include_control_plane` query parameter.
func runVaultListUnion(includeControlPlane, asJSON bool, stdout, stderr io.Writer) int {
	path := "/vault"
	if includeControlPlane {
		path += "?include_control_plane=true"
	}
	respBody, code := vaultListFetch(path, stderr)
	if code != 0 {
		return code
	}
	var out struct {
		Entries []vaultEntry `json:"entries"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		fmt.Fprintf(stderr, "error: decode response: %v\n", err)
		return 1
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		for _, e := range out.Entries {
			if err := enc.Encode(e); err != nil {
				fmt.Fprintf(stderr, "encode: %v\n", err)
				return 1
			}
		}
		if len(out.Entries) == 0 {
			fmt.Fprintln(stdout, "[]")
		}
		return 0
	}

	if len(out.Entries) == 0 {
		fmt.Fprintln(stdout, "No credentials stored.")
		return 0
	}
	// Width-align the printed "scope:" token (label plus its trailing
	// colon) so the paths line up in a column.
	width := 0
	for _, e := range out.Entries {
		if w := len(e.Scope) + 1; w > width {
			width = w
		}
	}
	for _, e := range out.Entries {
		fmt.Fprintf(stdout, "%-*s  %s\n", width, e.Scope+":", e.Path)
	}
	return 0
}

// vaultListFetch issues a GET against a vault list endpoint and maps the
// daemon's status codes. On success it returns the body and a zero exit
// code; on any handled error it prints to stderr and returns a non-zero
// code the caller propagates.
func vaultListFetch(path string, stderr io.Writer) ([]byte, int) {
	status, respBody, err := vaultDoRequest(http.MethodGet, path, nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return nil, 1
	}
	switch status {
	case http.StatusOK:
		return respBody, 0
	case http.StatusServiceUnavailable:
		fmt.Fprintln(stderr, "error: daemon is not configured with a vault")
		return nil, 1
	default:
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return nil, 1
	}
}

// agentCredentialPath builds the daemon path for an agent credential,
// threading the purpose through the `?purpose=` query parameter the
// GET/PUT/DELETE endpoints accept. The HTTP path always uses the stable
// URL word `credentials`; the purpose selects which credential the agent
// addresses (server-side default is oauth, but the CLI is explicit so
// the request is unambiguous). Both segments are url-escaped defensively
// (path-escape the name, query-escape the purpose); callers pass values
// already constrained by agentPathNameAndPurpose, but escaping keeps a
// stray special character from corrupting the request.
func agentCredentialPath(name, purpose string) string {
	return "/vault/agents/" + url.PathEscape(name) + "/credentials?purpose=" + url.QueryEscape(purpose)
}

// vaultDoRequest is a thin daemon-backed HTTP-client helper mirroring
// approvalDoRequest: it resolves the base URL via bindingAPIBaseURL
// (honoring AILERON_API_URL) and attaches the daemon authorization
// header. Returns the status and full body so callers map codes.
func vaultDoRequest(method, path string, body []byte) (int, []byte, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setDaemonAuthorization(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, out, nil
}

// runVaultInit creates a new local file vault at the canonical path.
// Errors out (without overwriting) if a vault already exists. This is
// the deliberate first-run flow per #492 item 5d — the alternative to
// letting `secret set` or `binding setup` create the vault implicitly.
//
// Passphrase source order, per #492 item 5c:
//  1. --passphrase-file <path>  (read once, no confirmation needed)
//  2. AILERON_VAULT_PASSPHRASE  (read once, no confirmation needed)
//  3. interactive prompt + confirmation
func runVaultInit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("vault init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	passphraseFile := flags.String("passphrase-file", "",
		"Read the passphrase from the named file instead of prompting (no trailing newline)")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	vaultPath := launch.DefaultVaultPath()

	state, err := vault.CheckState(vaultPath)
	if err != nil {
		fmt.Fprintf(stderr, "error checking vault: %v\n", err)
		return 1
	}
	if state == vault.StateReady {
		fmt.Fprintf(stderr, "vault already exists at %s\n", vaultPath)
		fmt.Fprintln(stderr, "delete the file first if you intend to start over (this destroys all stored secrets).")
		return 1
	}

	// Print the new-vault banner BEFORE the first prompt so the user
	// reads the irretrievable-passphrase warning before choosing one.
	// File/env sources are non-interactive (CI, scripts) and don't need
	// the warning — gate on willPromptInteractively, not on the source
	// returned by readVaultPassphrase (which only resolves after the
	// prompt fires).
	if willPromptInteractively(*passphraseFile) {
		printNewVaultBanner(stderr, vaultPath)
	}
	passphrase, source, err := readVaultPassphrase(*passphraseFile, "Vault passphrase: ", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if passphrase == "" {
		fmt.Fprintln(stderr, "error: passphrase cannot be empty")
		return 1
	}

	// Confirmation is only meaningful for interactive entry: when the
	// passphrase comes from a file or env, re-reading the same source
	// would just confirm itself, so we skip the second prompt.
	if source == passphraseSourceInteractive {
		confirm, _, err := readVaultPassphrase("", "Confirm passphrase: ", stderr)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if passphrase != confirm {
			fmt.Fprintln(stderr, "error: passphrases do not match")
			return 1
		}
	}

	if _, err := vault.Init(vaultPath, passphrase); err != nil {
		if errors.Is(err, vault.ErrVaultExists) {
			fmt.Fprintf(stderr, "vault already exists at %s\n", vaultPath)
			return 1
		}
		fmt.Fprintf(stderr, "error creating vault: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Vault created at %s\n", vaultPath)
	return 0
}

// passphraseSource indicates which source produced a passphrase; used by
// callers to decide whether to require confirmation.
type passphraseSource int

const (
	passphraseSourceInteractive passphraseSource = iota
	passphraseSourceFile
	passphraseSourceEnv
)

// readVaultPassphrase resolves a passphrase from (in order):
//  1. The supplied file path, if non-empty.
//  2. The AILERON_VAULT_PASSPHRASE env var.
//  3. An interactive prompt on the controlling terminal.
//
// Returns the passphrase and the source it came from. Trailing CR/LF is
// stripped from file/env values so common shell idioms (heredocs,
// `echo > file`) Just Work.
func readVaultPassphrase(passphraseFile, prompt string, w io.Writer) (string, passphraseSource, error) {
	if passphraseFile != "" {
		data, err := os.ReadFile(passphraseFile)
		if err != nil {
			return "", passphraseSourceFile, fmt.Errorf("reading passphrase file %q: %w", passphraseFile, err)
		}
		return strings.TrimRight(string(data), "\r\n"), passphraseSourceFile, nil
	}
	if env := os.Getenv(envVaultPassphrase); env != "" {
		return env, passphraseSourceEnv, nil
	}
	pass, err := promptPassphrase(prompt, w)
	if err != nil {
		return "", passphraseSourceInteractive, err
	}
	return pass, passphraseSourceInteractive, nil
}

// willPromptInteractively reports whether the next readVaultPassphrase
// call would fall through to the controlling terminal. Mirrors readVaultPassphrase's
// dispatch order — file > env > interactive — so callers can decide
// whether to print user-facing context (e.g. the new-vault banner)
// BEFORE the prompt fires, instead of inferring interactivity from
// the post-hoc passphraseSource return value.
func willPromptInteractively(passphraseFile string) bool {
	return passphraseFile == "" && os.Getenv(envVaultPassphrase) == ""
}

// printAileronBanner prints the Aileron ASCII-art welcome shown at the
// top of any interactive vault create/unlock prompt. Callers gate this
// on [willPromptInteractively] so non-interactive callers (env var,
// --passphrase-file) don't get a banner dumped into their logs.
func printAileronBanner(w io.Writer) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, `░█▀█░▀█▀░█░░░█▀▀░█▀▄░█▀█░█▀█`)
	fmt.Fprintln(w, `░█▀█░░█░░█░░░█▀▀░█▀▄░█░█░█░█`)
	fmt.Fprintln(w, `░▀░▀░▀▀▀░▀▀▀░▀▀▀░▀░▀░▀▀▀░▀░▀`)
	fmt.Fprintln(w, "")
}

// printNewVaultBanner prints the irretrievable-passphrase warning. Same
// language as runSecretSet's inline first-run banner — kept in sync so
// users see the same warning regardless of which path created the vault.
func printNewVaultBanner(w io.Writer, vaultPath string) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  Creating a new Aileron vault.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  The passphrase you choose protects all secrets in this vault.")
	fmt.Fprintln(w, "  It is never stored, transmitted, or recoverable. No one can")
	fmt.Fprintln(w, "  read it, tell you what it is, or help you retrieve it.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  If you lose this passphrase, you must delete the vault file")
	fmt.Fprintf(w, "  (%s) and re-add all secrets.\n", vaultPath)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  Store this passphrase securely. Do not share it.")
	fmt.Fprintln(w, "")
}
