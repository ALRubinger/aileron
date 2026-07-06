package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ALRubinger/aileron/internal/flightplan/credreq"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// deriveRequirements is the seam over the credential-requirement deriver
// (#2002). DeriveFromFrozen runs runtime.LoadVerified (real signature +
// content-hash verification) then the pure Derive; the seam lets the verb's
// tests inject a canned []RequiredBinding and exercise the composition
// contract (diff -> prompt -> write -> summarize) without a signed fixture.
// End-to-end derivation is covered by credreq's own derivefromfrozen_test.go.
var deriveRequirements = credreq.DeriveFromFrozen

// skillBindDescriptorPath is a seam so tests point the verb at a temp
// descriptor file instead of the operator's real
// ~/.aileron/binding-descriptors.yaml. Empty means the default user path.
var skillBindDescriptorPath = ""

// descriptorPath resolves the descriptor file the verb reads and writes: the
// test seam if set, else proxybinding.DefaultUserPath().
func descriptorPath() string {
	if skillBindDescriptorPath != "" {
		return skillBindDescriptorPath
	}
	return proxybinding.DefaultUserPath()
}

// bindVaultClient is the seam-injected vault surface the verb needs: read the
// set of user-level services already stored, and store one user-level secret.
// A daemon-backed default reaches the existing GET /vault/user and
// PUT /vault/user/<svc>/credentials endpoints; tests inject an in-memory fake.
type bindVaultClient interface {
	// ListUserServices returns the bare service segments under user/ that the
	// vault already holds (for example "prod-reader" for user/prod-reader).
	ListUserServices() ([]string, error)
	// PutUser stores value at user/<service>, base64-encoding on the wire.
	PutUser(service string, value []byte) error
}

// newBindVaultClient builds the daemon-backed vault client. It is a
// package-level seam so tests inject a fake with in-memory state.
var newBindVaultClient = func() bindVaultClient { return daemonBindVaultClient{} }

// daemonBindVaultClient talks to the local daemon's vault endpoints, reusing
// the same plumbing `vault list --user` and `vault put user/...` drive.
type daemonBindVaultClient struct{}

func (daemonBindVaultClient) ListUserServices() ([]string, error) {
	status, body, err := vaultDoRequest(http.MethodGet, "/vault/user", nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		return nil, fmt.Errorf("daemon is not configured with a vault")
	default:
		return nil, fmt.Errorf("server returned %d listing user credentials: %s", status, string(body))
	}
	var out struct {
		Services []struct {
			Service string `json:"service"`
		} `json:"services"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode user credential list: %w", err)
	}
	services := make([]string, 0, len(out.Services))
	for _, s := range out.Services {
		services = append(services, s.Service)
	}
	return services, nil
}

func (daemonBindVaultClient) PutUser(service string, value []byte) error {
	body, err := json.Marshal(agentCredentialsBody{Value: value})
	if err != nil {
		return err
	}
	status, resp, err := vaultDoRequest(http.MethodPut, userCredentialDaemonPath(service), body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent:
		return nil
	case http.StatusLocked:
		return fmt.Errorf("vault is locked; unlock it first")
	case http.StatusServiceUnavailable:
		return fmt.Errorf("daemon is not configured with a vault")
	default:
		return fmt.Errorf("server returned %d storing user/%s: %s", status, service, string(resp))
	}
}

// reqStatus records, for one derived requirement, whether the vault already
// holds its secret and whether the loaded descriptors already satisfy it. A
// requirement is satisfied only when both are true.
type reqStatus struct {
	req               credreq.RequiredBinding
	vaultPresent      bool
	descriptorPresent bool
}

func (s reqStatus) satisfied() bool { return s.vaultPresent && s.descriptorPresent }

// diffRequirements is the pure diff: for each requirement it reports whether
// the vault set already holds the credential ref and whether the loaded
// descriptor entries already satisfy it. It performs no I/O so it is
// independently unit-testable. vaultRefs is the set of "user/<service>" refs
// the vault already holds.
func diffRequirements(reqs []credreq.RequiredBinding, vaultRefs map[string]bool, existing []proxybinding.Entry) []reqStatus {
	out := make([]reqStatus, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, reqStatus{
			req:               r,
			vaultPresent:      vaultRefs[r.CredentialRef],
			descriptorPresent: descriptorSatisfies(r, existing),
		})
	}
	return out
}

// descriptorSatisfies reports whether the existing descriptor entries already
// carry the binding a requirement names. A host-less-identity (sigv4)
// requirement is satisfied by any entry with a matching (kind, identity_label,
// scheme). A host-keyed (bearer) requirement is satisfied only when every
// non-templated host it names has an entry with a matching host,
// credential_ref, and scheme; templated hosts are skipped because they cannot
// be onboarded automatically. A requirement whose only hosts are templated is
// never considered satisfied (there is nothing writable to match).
func descriptorSatisfies(req credreq.RequiredBinding, existing []proxybinding.Entry) bool {
	if req.HostLessIdentity() {
		for i := range existing {
			e := existing[i]
			if e.Kind == req.CredentialKind && e.IdentityLabel == req.IdentityLabel && e.Scheme == req.Scheme {
				return true
			}
		}
		return false
	}

	templated := templatedHostSet(req)
	matchedAtLeastOne := false
	for _, host := range req.Hosts {
		if templated[host] {
			continue
		}
		matchedAtLeastOne = true
		if !descriptorHasHost(existing, host, req.CredentialRef, req.Scheme) {
			return false
		}
	}
	return matchedAtLeastOne
}

// descriptorHasHost reports whether some entry binds host to the given
// credential ref and scheme. Host comparison is case-insensitive, matching the
// descriptor loader's host dedup semantics.
func descriptorHasHost(existing []proxybinding.Entry, host, credentialRef, scheme string) bool {
	for i := range existing {
		e := existing[i]
		if strings.EqualFold(e.Host, host) && e.CredentialRef == credentialRef && e.Scheme == scheme {
			return true
		}
	}
	return false
}

// templatedHostSet builds a lookup of the requirement's templated hosts.
func templatedHostSet(req credreq.RequiredBinding) map[string]bool {
	if len(req.TemplatedHosts) == 0 {
		return nil
	}
	set := make(map[string]bool, len(req.TemplatedHosts))
	for _, h := range req.TemplatedHosts {
		set[h] = true
	}
	return set
}

// runSkillBind onboards a frozen plan's credential requirements: resolve the
// installed frozen version, derive its RequiredBinding set, diff each
// requirement against the vault and the descriptors, interactively fill only
// the gaps, write the descriptor entries in a single atomic Upsert, print a
// per-requirement summary, and remind the operator to restart the daemon.
func runSkillBind(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("skill bind", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("version", "", "Frozen version id to bind (defaults to the most recently frozen version)")
	positionals, err := parseInterspersedFlags(flags, args)
	if err != nil {
		return 1
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, skillUsage)
		return 1
	}
	name := positionals[0]

	s := store.New(skillStoreDir)
	id, _, err := resolveFrozenVersion(s, name, *version)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	reqs, err := deriveRequirements(s, name, id)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if len(reqs) == 0 {
		fmt.Fprintf(stdout, "Plan %q declares no credential requirements; nothing to onboard.\n", name)
		return 0
	}

	client := newBindVaultClient()
	services, err := client.ListUserServices()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	vaultRefs := make(map[string]bool, len(services))
	for _, svc := range services {
		vaultRefs["user/"+svc] = true
	}

	existing, err := proxybinding.Load(proxybinding.LoadOptions{UserPath: descriptorPath()})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	statuses := diffRequirements(reqs, vaultRefs, existing)

	// haveSecret tracks which credential refs already hold a vault secret,
	// seeded from the vault snapshot and updated after each PutUser. Two
	// host-keyed requirements can share one CredentialRef (same identity, distinct
	// host sets), so once the first has stored the shared secret the second must
	// not re-prompt for it; the descriptor diff is still per-host, so each host's
	// entry is written independently.
	haveSecret := make(map[string]bool, len(vaultRefs))
	for ref := range vaultRefs {
		haveSecret[ref] = true
	}

	br := bufio.NewReader(stdin)
	var (
		toWrite    []proxybinding.Entry
		summary    []string
		advisories []string
	)
	for _, st := range statuses {
		if st.satisfied() {
			summary = append(summary, fmt.Sprintf("  satisfied: %s (%s)", st.req.CredentialRef, requirementLabel(st.req)))
			continue
		}
		needSecret := !haveSecret[st.req.CredentialRef]
		entries, adv, storedSecret, err := fillRequirement(st.req, needSecret, !st.descriptorPresent, client, br, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "error: %s: %v\n", st.req.CredentialRef, err)
			return 1
		}
		if storedSecret {
			haveSecret[st.req.CredentialRef] = true
		}
		toWrite = append(toWrite, entries...)
		advisories = append(advisories, adv...)
		// "onboarded" reports a completed action (a secret stored or a descriptor
		// entry written). A requirement that produced only an advisory (for
		// example a host-keyed binding whose only host is a template) and stored
		// nothing is reported as pending so the summary never overstates progress.
		if len(entries) > 0 || storedSecret {
			summary = append(summary, fmt.Sprintf("  onboarded: %s (%s)", st.req.CredentialRef, requirementLabel(st.req)))
		} else {
			summary = append(summary, fmt.Sprintf("  pending:   %s (%s)", st.req.CredentialRef, requirementLabel(st.req)))
		}
	}

	if len(toWrite) > 0 {
		if err := proxybinding.Upsert(descriptorPath(), toWrite...); err != nil {
			fmt.Fprintf(stderr, "error: write binding descriptor: %v\n", err)
			return 1
		}
	}

	fmt.Fprintf(stdout, "Onboarding for plan %q:\n", name)
	for _, line := range summary {
		fmt.Fprintln(stdout, line)
	}
	for _, adv := range advisories {
		fmt.Fprintf(stdout, "  advisory: %s\n", adv)
	}
	if len(toWrite) > 0 {
		fmt.Fprintf(stdout, "Wrote %d descriptor entr%s to %s\n", len(toWrite), plural(len(toWrite), "y", "ies"), descriptorPath())
		fmt.Fprintln(stdout, "These bindings are live: the daemon reloads binding descriptors from the file on the next request (#1887), so no restart is needed. If a request still isn't matched, `aileron daemon stop` then `aileron daemon start` reloads them as a fallback.")
	}
	return 0
}

// fillRequirement captures the secret and non-secret params for a requirement
// that is not fully satisfied. needSecret and needDescriptor are computed by the
// caller against a live view of the vault and descriptors, so a
// partially-onboarded requirement fills only its missing half and a secret
// shared by two requirements is prompted once. It returns the descriptor entries
// the caller batches into a single Upsert, any advisories (for templated hosts),
// and whether it stored a secret (so the caller keeps its live vault view in
// sync and reports an accurate summary).
func fillRequirement(req credreq.RequiredBinding, needSecret, needDescriptor bool, client bindVaultClient, stdin *bufio.Reader, stdout, stderr io.Writer) ([]proxybinding.Entry, []string, bool, error) {
	storedSecret := false
	if needSecret {
		secret, err := promptPassphrase(fmt.Sprintf("Secret for %s: ", req.CredentialRef), stderr)
		if err != nil {
			return nil, nil, false, err
		}
		if secret == "" {
			return nil, nil, false, fmt.Errorf("secret cannot be empty")
		}
		service := strings.TrimPrefix(req.CredentialRef, "user/")
		if err := client.PutUser(service, []byte(secret)); err != nil {
			return nil, nil, false, err
		}
		storedSecret = true
	}

	if !needDescriptor {
		return nil, nil, storedSecret, nil
	}

	if req.HostLessIdentity() {
		if req.IdentityLabel == "" {
			return nil, nil, storedSecret, fmt.Errorf("cannot write a valid %s descriptor without an identity_label; the frozen plan declares a %s credential with no identityLabel", req.Scheme, req.CredentialKind)
		}
		akid := strings.TrimSpace(promptLine(stdin, stdout, fmt.Sprintf("AWS access key ID for %s: ", req.CredentialRef)))
		if akid == "" {
			return nil, nil, storedSecret, fmt.Errorf("access key ID cannot be empty")
		}
		entry := proxybinding.Entry{
			Kind:          req.CredentialKind,
			IdentityLabel: req.IdentityLabel,
			CredentialRef: req.CredentialRef,
			Scheme:        req.Scheme,
			AccessKeyID:   akid,
		}
		for _, w := range entry.Warnings() {
			fmt.Fprintf(stderr, "warning: %s\n", w)
		}
		return []proxybinding.Entry{entry}, nil, storedSecret, nil
	}

	// Host-keyed (bearer): one entry per non-templated host. A templated host
	// carries an unresolved input token and cannot be onboarded automatically.
	templated := templatedHostSet(req)
	var (
		entries    []proxybinding.Entry
		advisories []string
	)
	for _, host := range req.Hosts {
		if templated[host] {
			advisories = append(advisories, fmt.Sprintf("host %q carries an input template and cannot be onboarded automatically; add it by hand after resolving the input", host))
			continue
		}
		entries = append(entries, proxybinding.Entry{
			Host:          host,
			CredentialRef: req.CredentialRef,
			Scheme:        req.Scheme,
		})
	}
	return entries, advisories, storedSecret, nil
}

// requirementLabel is a short human description of what a requirement binds:
// its identity for a host-less-identity binding, or its host list for a
// host-keyed one.
func requirementLabel(req credreq.RequiredBinding) string {
	if req.HostLessIdentity() {
		return fmt.Sprintf("identity %s/%s", req.CredentialKind, req.IdentityLabel)
	}
	return "hosts " + strings.Join(req.Hosts, ", ")
}

// plural returns singular when n == 1, else plural.
func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
