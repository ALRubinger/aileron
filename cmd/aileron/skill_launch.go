package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"sort"
	"strings"
	"syscall"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/pull"
	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
	"github.com/ALRubinger/aileron/internal/model"
)

// The Launch SPIs are wired here from package-level seams so CLI tests
// exercise the orchestration with fakes and no live daemon, mirroring
// skill_freeze.go's newDigestResolver/newFeatureComposer discipline. Each seam
// returns the daemon-backed implementation in production.
var newLaunchDispatcher = func() runtime.ActionDispatcher { return daemonDispatcher{} }
var newLaunchApprover = func() runtime.Approver { return daemonApprover{} }
var newLaunchAuditSink = func(stderr io.Writer) runtime.AuditSink {
	return daemonAuditSink{stderr: stderr, actorID: operatorActorID()}
}

// operatorActorID resolves the CLI operator's identity as "<user>@<host>" so
// every launch-scoped audit record correlates back to the human who ran the
// invocation rather than the runtime component that emitted it (#1875). It is a
// package-level seam so tests can stamp a deterministic identity without
// depending on the host's real user/hostname. Lookup errors degrade to
// "unknown" for the missing half rather than failing the launch: audit
// provenance is best-effort (see daemonAuditSink), and a partial identity is
// still more useful than mislabeling the operator as the runtime service. This
// is the cheap operator-identity floor; a vault-anchored configured identity is
// a deliberate follow-up (#1875).
//
// A non-empty AILERON_OPERATOR_ID env var takes precedence over the
// user@host floor. On the composed-environment model the CLI that emits
// audit records runs INSIDE the sealed container, where user.Current() and
// os.Hostname() resolve to the image's fixed non-root user and the ephemeral
// container id (agent@<container-id>) — identical for every operator, which
// defeats attribution. The host resolves the real operator identity once and
// carries it into the boot via AILERON_OPERATOR_ID (see
// containerImageRunner.Run), mirroring the existing daemon-coords injection.
// A host-run launch leaves the env unset and keeps the user@host floor
// (#1881).
var operatorActorID = func() string {
	if id := strings.TrimSpace(os.Getenv("AILERON_OPERATOR_ID")); id != "" {
		return id
	}
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host := "unknown"
	if h, err := os.Hostname(); err == nil && h != "" {
		host = h
	}
	return name + "@" + host
}

// newLaunchImageRunner returns the production image runner that boots the
// verified pinned environment image and runs the plan inside it (#1731). It
// is a package-level seam so CLI tests swap in a fake that records the exact
// image string and never touches Docker, mirroring the other launch seams.
// The production runner injects the daemon-backed env resolver so the
// re-entered in-container launch reaches the SAME host daemon action + audit
// boundary and its records surface in the host `aileron audit list` (#1759),
// and the daemon-backed plan proxy bootstrapper so the booted plan container's
// HTTPS egress routes through the ADR-0019 daemon forward proxy for host-binding
// credential injection (#1828). Both seams stay passthrough (inject nothing)
// when no daemon config is resolvable, gating on config presence, so a no-daemon
// launch still boots.
var newLaunchImageRunner = func() runtime.ImageRunner {
	return containerImageRunner{daemonEnv: daemonImageEnv{}, proxy: daemonPlanProxyBootstrapper{}}
}

// newLaunchImageDigestResolver returns the production
// runtime.LocalImageDigestResolver that re-checks, at boot time, that a
// composed-tools pin's local tag still resolves in the local daemon to the
// pin's attested digest (#1863). It is a package-level seam so CLI tests swap in
// a fake that records the tag and never touches Docker, mirroring the other
// launch seams. The production resolver reuses the SAME localImageDigest logic
// (RepoDigests-then-.Id) that produced the pin's digest at freeze time, so the
// compare is apples-to-apples. The runtime consults it ONLY on the composed boot
// path and fails closed on a digest mismatch or a resolve error.
var newLaunchImageDigestResolver = func() runtime.LocalImageDigestResolver {
	return containerImageDigestResolver{}
}

// newLaunchRegistryImageResolver returns the production
// runtime.RegistryImageResolver that pulls a published composed (or foreign-base)
// image from a plan's recorded install origin and verifies it against the signed
// lock pin per the pin's binding kind (#1903). It is consulted only when the
// loaded plan carries a registry origin (an OCI install), so a plan installed by
// OCI reference on a machine that never froze it can still boot. It is a
// package-level seam so CLI tests inject a fake that returns a canned bootRef and
// never touches the network, mirroring newLaunchImageRunner/skillPullRun. All
// oras/registry code stays in internal/flightplan/pull, so cmd/aileron stays
// oras-free.
var newLaunchRegistryImageResolver = func() runtime.RegistryImageResolver {
	return launchRegistryImageResolver{}
}

// launchRegistryImageResolver adapts pull.PullImage to the runtime seam. The
// pull is bounded by a timeout and honors Ctrl-C so a hung or unreachable
// registry fails the launch instead of blocking indefinitely, mirroring the OCI
// install path (installPullTimeout + signal.NotifyContext).
type launchRegistryImageResolver struct{}

func (launchRegistryImageResolver) Resolve(ctx context.Context, origin runtime.RegistryImageOrigin, pin freeze.ImagePin) (string, error) {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, installPullTimeout)
	defer cancel()
	res, err := pull.PullImage(ctx, pull.ImagePullOptions{
		Registry:   origin.Registry,
		VersionTag: origin.VersionTag,
		Pin:        pin,
	})
	if err != nil {
		return "", err
	}
	return res.BootRef, nil
}

// launchRegistryResolver returns the host-side registry-image resolver wired
// into the runtime, or nil on the image-boot re-entry. The nil-skip mirrors the
// InPinnedImage / publisher-verifier guards: when AILERON_SKILL_IMAGE_BOOTED is
// set this launch is the re-entry INSIDE the sealed pin, which the sentinel
// already routes in-process before any boot, so the registry path is never
// entered and no resolver is needed.
func launchRegistryResolver() runtime.RegistryImageResolver {
	if os.Getenv(envSkillImageBooted) != "" {
		return nil
	}
	return newLaunchRegistryImageResolver()
}

// newLaunchToolStepRunner returns the production tool-step runner that
// executes a `kind: tool` step as a deterministic subprocess INSIDE the
// booted plan container (#1829). It is a package-level seam so CLI tests
// swap in a fake that records the argv/scope wiring and never execs,
// mirroring newLaunchImageRunner. The production runner refuses to exec
// outside the pinned image (the AILERON_SKILL_IMAGE_BOOTED sentinel) and,
// for a step with a sealed reach, mints a daemon step-scoped proxy
// credential before the exec and releases it after, failing closed when the
// scope cannot be obtained, so a sealed step never runs unscoped.
var newLaunchToolStepRunner = func() runtime.ToolStepRunner {
	return inContainerToolStepRunner{}
}

// launchPublisherVerifier returns the host-side publisher-trust gate wired
// into the runtime (#1900), or nil on the image-boot re-entry. The nil-skip
// mirrors the InPinnedImage guard: when AILERON_SKILL_IMAGE_BOOTED is set this
// launch is the re-entry INSIDE the sealed pin, where the host already gated
// before boot and the keyring is not mounted, so wiring the gate here would
// resolve an empty container keyring and fail closed for every image-pinned
// plan. On a host launch it returns the keyring-backed verifier; the runtime
// still skips the gate for a plan that declares no publisher.
func launchPublisherVerifier(stderr io.Writer) runtime.PublisherVerifier {
	if os.Getenv(envSkillImageBooted) != "" {
		return nil
	}
	return newLaunchPublisherVerifier(stderr)
}

// launchSeamForTest is the LLM seam the launch wires into the runtime. It is
// nil by default, which is the v1 contract: a plan with an llm-seam step
// errors unless a provider is configured, so a default launch reaches no LLM.
// It is a package-level seam so tests can supply a deterministic seam; there
// is no production LLM seam provider wired in v1.
var launchSeamForTest runtime.LLMSeam

// inputFlag collects repeated --input name=value launch overrides.
type inputFlag struct {
	values map[string]any
}

func (f *inputFlag) String() string { return "" }

func (f *inputFlag) Set(s string) error {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return fmt.Errorf("--input must be name=value, got %q", s)
	}
	if f.values == nil {
		f.values = map[string]any{}
	}
	f.values[s[:i]] = s[i+1:]
	return nil
}

// runSkillLaunch implements `aileron skill launch <name>`. It loads a frozen
// version from the store, verifies it, resolves declared inputs, walks the
// deterministic step graph through the daemon-backed action boundary, and
// materializes declared artifacts into --out-dir. This is the Flight-Plan
// Launch runtime (#1511); it is distinct from the top-level `aileron launch`
// agent-sandbox command (a different subsystem).
func runSkillLaunch(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("skill launch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("version", "", "Frozen version id to launch (defaults to the only/most recently frozen version)")
	outDir := flags.String("out-dir", ".", "Directory file-target artifacts are written to")
	// storeDir defaults to the process store seam. The in-container re-entry on
	// the image-boot path passes the bind-mounted store path here so the inner
	// binary loads the same verified frozen unit from the mount rather than the
	// (empty) default store inside the image.
	storeDir := flags.String("store-dir", skillStoreDir, "Skill store directory (defaults to ~/.aileron/skills)")
	var inputs inputFlag
	flags.Var(&inputs, "input", "Launch input override as name=value; repeatable")
	// verbose restores the full resolved-input value dump. By default the
	// result summary prints a per-input "<type, size>" line instead of the raw
	// value, so a plan that passes a large input (e.g. an inlined HTML
	// document) no longer floods the terminal and buries the launch result
	// (#1888). -v is an alias.
	verbose := flags.Bool("verbose", false, "Print full resolved input values instead of a type+size summary")
	flags.BoolVar(verbose, "v", false, "Shorthand for --verbose")
	positionals, err := parseInterspersedFlags(flags, args)
	if err != nil {
		return 1
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, skillUsage)
		return 1
	}
	name := positionals[0]

	s := store.New(*storeDir)
	id, err := resolveLaunchVersion(s, name, *version, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	res, err := runtime.Run(context.Background(), runtime.Options{
		Store:      s,
		Name:       name,
		Version:    id,
		Inputs:     runtime.LaunchArgs(inputs.values),
		Dispatcher: newLaunchDispatcher(),
		Approver:   newLaunchApprover(),
		Audit:      newLaunchAuditSink(stderr),
		// Seam is nil in v1 production: the LLM seam is unwired, so a plan with
		// an llm-seam step errors unless a provider is supplied. Tests inject a
		// deterministic seam through launchSeamForTest.
		Seam: launchSeamForTest,
		// ImageRunner boots the verified pinned environment image and runs the
		// plan inside it. When the frozen unit pins no image, the runtime never
		// touches this seam and stays on the in-process path.
		ImageRunner: newLaunchImageRunner(),
		// ImageDigestResolver re-checks, at boot time, that a composed-tools pin's
		// local tag still resolves in the local daemon to the pin's attested
		// digest (#1863). The runtime consults it only on the composed boot path
		// and fails closed on a mismatch or resolve error; a non-composed
		// (ref@digest) pin never touches it.
		ImageDigestResolver: newLaunchImageDigestResolver(),
		// RegistryImageResolver pulls and verifies the published image for a plan
		// installed by OCI reference (#1903), so a machine that never froze the
		// plan can still boot it. The runtime consults it only when the loaded
		// plan carries a registry origin (an OCI install); a locally-frozen plan
		// boots by its local tag and never touches this seam. It is nil on the
		// image-boot re-entry (the same sentinel that sets InPinnedImage): the
		// sentinel routes in-process before any boot, so the registry path is
		// never re-entered inside the container.
		RegistryImageResolver: launchRegistryResolver(),
		// InPinnedImage: the image-boot re-entry runs with the sentinel its
		// booting runner injected; it is already inside the certified
		// environment and must run the plan in-process, not boot the pin
		// again.
		InPinnedImage: os.Getenv(envSkillImageBooted) != "",
		// PublisherVerifier is the host-side publisher-trust gate (#1900). It is
		// nil on the image-boot re-entry (the same sentinel that sets
		// InPinnedImage): the host already ran the gate before booting the pin,
		// no keyring is mounted into the sealed container, and re-checking here
		// would resolve the container's empty home to an empty keyring and fail
		// closed for every image-pinned plan. On a host launch it is the
		// keyring-backed verifier; the runtime still skips the gate for a plan
		// that declares no publisher.
		PublisherVerifier: launchPublisherVerifier(stderr),
		// ToolRunner executes each `kind: tool` step as a scoped subprocess in
		// the current (pinned) environment (#1829). No sibling container is
		// ever dispatched.
		ToolRunner: newLaunchToolStepRunner(),
		OutDir:     *outDir,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Launched %q\n", name)
	fmt.Fprintf(stdout, "  Version:     %s\n", id)
	fmt.Fprintf(stdout, "  ContentHash: %s\n", res.ContentHash)
	if len(res.ResolvedInputs) > 0 {
		fmt.Fprintln(stdout, "  Resolved inputs:")
		keys := make([]string, 0, len(res.ResolvedInputs))
		for k := range res.ResolvedInputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := res.ResolvedInputs[k]
			if *verbose {
				fmt.Fprintf(stdout, "    %s = %v\n", k, v)
			} else {
				fmt.Fprintf(stdout, "    %s = %s\n", k, summarizeInputValue(v))
			}
		}
	}
	if len(res.Artifacts) > 0 {
		fmt.Fprintln(stdout, "  Artifacts:")
		for _, a := range res.Artifacts {
			if a.Written {
				fmt.Fprintf(stdout, "    %s -> %s  %s\n", a.Name, a.Path, a.Digest)
			} else {
				fmt.Fprintf(stdout, "    %s (retained)  %s\n", a.Name, a.Digest)
			}
		}
	}
	if len(res.AuditIDs) > 0 {
		fmt.Fprintf(stdout, "  Audit records: %d\n", len(res.AuditIDs))
	}
	return 0
}

// summarizeInputValue renders a resolved launch input as a compact
// "<type, size>" summary (e.g. "<string, 48.2 KB>") rather than its full
// value. The size is the byte length of the value's default string form,
// which is exactly what an unbounded %v print would have emitted, so the
// summary tells the operator how large a value they suppressed. This keeps a
// plan with a large-valued input from flooding the terminal and burying the
// launch result (#1888); -v/--verbose restores the full-value dump.
func summarizeInputValue(v any) string {
	s := fmt.Sprintf("%v", v)
	return fmt.Sprintf("<%T, %s>", v, humanByteSize(len(s)))
}

// humanByteSize renders a byte count as a compact human-readable string
// (e.g. "48.2 KB"). Counts under 1 KiB render as bytes.
func humanByteSize(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := int64(n) / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// resolveLaunchVersion resolves the version id to launch: the explicit
// --version when given, otherwise the most-recently-frozen version.
//
// Version ids are content-hash slugs, so the store's sorted-ids order is
// lexicographic, not chronological; picking the sorted max could launch an
// older version over a newer one (issue #1880). This selects by freeze time
// (LatestFrozen), so a bare launch always runs the newest frozen version.
//
// When more than one version exists and no --version was pinned, a banner is
// printed to stdout naming the auto-selected version and the total count, so
// the implicit choice is visible and the operator knows how to pin it.
func resolveLaunchVersion(s *store.Store, name, version string, stdout io.Writer) (string, error) {
	id, count, err := resolveFrozenVersion(s, name, version)
	if err != nil {
		return "", err
	}
	if version == "" && count > 1 {
		fmt.Fprintf(stdout, "launching %s (newest of %d; use --version to pin)\n", id, count)
	}
	return id, nil
}

// resolveFrozenVersion resolves the frozen version id to operate on: the
// explicit version if given, else the most-recently-frozen version. It returns
// the resolved id and how many frozen versions exist (so a caller can emit its
// own "newest of N" hint) and is verb-neutral — it prints nothing — so publish
// reuses it without inheriting launch's "launching ..." banner.
func resolveFrozenVersion(s *store.Store, name, version string) (id string, count int, err error) {
	if version != "" {
		return version, 1, nil
	}
	id, count, err = s.LatestFrozen(name)
	if err != nil {
		return "", 0, fmt.Errorf("list frozen versions for %q: %w", name, err)
	}
	if count == 0 {
		return "", 0, fmt.Errorf("skill %q has no frozen versions; run `aileron skill freeze %s` first", name, name)
	}
	return id, count, nil
}

// daemonDispatcher dispatches an action through the daemon's
// POST /v1/actions/{name}/run endpoint. The action ref (aileron:<c>.<a>) maps
// to the daemon action name; credentials are injected host-side at the
// boundary, so the dispatcher never sees them. A 202 (approval pending) is
// surfaced as an error directing the operator to approve, since v1 launch runs
// unattended and does not poll the approval lifecycle.
type daemonDispatcher struct{}

func (daemonDispatcher) Dispatch(ctx context.Context, ref string, args map[string]any) (runtime.DispatchResult, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return runtime.DispatchResult{}, err
	}
	name := daemonActionName(ref)
	body, _ := json.Marshal(actionRunRequest{Args: args})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/actions/"+url.PathEscape(name)+"/run", bytes.NewReader(body))
	if err != nil {
		return runtime.DispatchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setDaemonAuthorization(req)
	resp, err := actionsHTTPClient.Do(req)
	if err != nil {
		return runtime.DispatchResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var out actionRunResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return runtime.DispatchResult{}, fmt.Errorf("decode action result: %w", err)
		}
		// Thread the daemon's non-secret actor provenance into the runtime so
		// the per-output audit record can attribute the produced artifact to
		// the exact connector build and identity (issue #1753). These are
		// references only; credentials are injected host-side and never reach
		// the dispatcher.
		return runtime.DispatchResult{
			Output:            parseResultPayload(out.Result),
			ConnectorVersion:  out.ConnectorVersion,
			ConnectorHash:     out.ConnectorHash,
			IdentityLabel:     out.IdentityLabel,
			CredentialBinding: out.CredentialBinding,
			ConsentDecision:   out.ConsentDecision,
		}, nil
	case http.StatusAccepted:
		return runtime.DispatchResult{}, fmt.Errorf("action %q requires approval; approve it (aileron approval list) and re-launch", ref)
	default:
		return runtime.DispatchResult{}, fmt.Errorf("daemon returned %d for %q: %s", resp.StatusCode, ref, strings.TrimSpace(string(raw)))
	}
}

// daemonActionName maps a manifest action ref (aileron:<connector>.<action>)
// to the daemon action name. v1 uses the bare action segment as the daemon
// name; the connector binding is resolved daemon-side.
func daemonActionName(ref string) string {
	r := strings.TrimPrefix(ref, "aileron:")
	if i := strings.LastIndex(r, "."); i >= 0 {
		return r[i+1:]
	}
	return r
}

// parseResultPayload decodes the daemon's string result into a JSON map so the
// runtime can bind downstream steps against it. A non-JSON or empty result
// surfaces under a "result" key so a downstream binding still resolves.
//
// The daemon's action executor wraps the last step's output in a dispatch
// envelope {"action": <name>, "output": <result>, "steps": {...}} (see
// internal/action/executor.go). Binding that whole envelope to steps.<id>.result
// nests the real result one level deep, which duplicates the payload in the
// materialized artifact, misdirects redaction/multi-output reads at the outer
// keys, and breaks the audit query-execution-id lift (issue #1801). So when the
// parsed result is that envelope, a JSON object carrying an "action" string AND
// an "output" object, unwrap it and return the inner output map. StubExecutor
// results carry "action" but no "output", so requiring both keys leaves them
// (and every other shape) passing through unchanged. The daemon's public
// /v1/actions run envelope is untouched; only the launch-side binding is fixed.
func parseResultPayload(result *string) map[string]any {
	if result == nil || *result == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(*result), &m); err == nil {
		if out, ok := dispatchEnvelopeOutput(m); ok {
			return out
		}
		return m
	}
	return map[string]any{"result": *result}
}

// dispatchEnvelopeOutput returns the inner output map when m is the daemon's
// dispatch envelope: a JSON object carrying an "action" string AND an "output"
// object. It returns ok=false for any other shape (including StubExecutor
// results, which carry "action" but no "output") so those pass through
// unchanged.
func dispatchEnvelopeOutput(m map[string]any) (map[string]any, bool) {
	if _, ok := m["action"].(string); !ok {
		return nil, false
	}
	out, ok := m["output"].(map[string]any)
	if !ok {
		return nil, false
	}
	return out, true
}

// daemonApprover defers to the daemon's own approval gate at the action
// boundary: the daemon's POST /run returns 202 when an action needs approval,
// which the dispatcher surfaces. The runtime-level approver therefore approves
// here so the routing is decided once, at the daemon boundary, rather than
// gated twice. A future interactive launch can replace this seam with one that
// drives the approval lifecycle in-process.
type daemonApprover struct{}

func (daemonApprover) Approve(_ context.Context, _ runtime.ApprovalRequest) (runtime.Decision, error) {
	return runtime.Decision{Approved: true}, nil
}

// daemonAuditSink persists each launch audit record to the daemon's audit
// trail via POST /v1/audit, so launch provenance surfaces in
// `aileron audit list` / `aileron audit show`. The daemon is the single
// writer and single id-minter; this sink translates the runtime's
// AuditRecord into the ingest request and threads the minted audit_id back
// as the Record return value.
//
// The AuditSink SPI's Record returns only a string, so a POST failure is
// best-effort: it logs to stderr and returns "" (the launch continues and
// simply surfaces a lower Audit-records count). This keeps launch
// provenance a companion to the run rather than a hard dependency, matching
// the recorder's own best-effort append discipline (ADR-0010).
type daemonAuditSink struct {
	stderr io.Writer
	// actorID is the operator identity ("<user>@<host>") stamped as the human
	// Actor on every record this sink emits, resolved once at construction via
	// operatorActorID (#1875).
	actorID string
}

func (s daemonAuditSink) Record(ctx context.Context, rec runtime.AuditRecord) string {
	// The record kind is the explicit discriminator (#1752): output, reach, and
	// launch records carry a flat `aileron.*` payload (top-level attributes,
	// matching the vault.user.credential.* convention) so the invocation filter
	// and Timeline read aileron.invocation.id at the top level (#1928); only the
	// per-action record keeps the nested pay["fields"]/actionRef/sink shape.
	var eventType string
	var payload map[string]any
	switch rec.Kind {
	case runtime.RecordKindOutput:
		eventType = string(model.EventTypeOutputMaterialized)
		payload = rec.Fields
		if payload == nil {
			// A well-formed output record always carries fields, but guard so an
			// empty record posts an object payload rather than a JSON null.
			payload = map[string]any{}
		}
	case runtime.RecordKindReach:
		// A reach record (#1784, enforcement truth from #1829) carries the same
		// flat `aileron.*` payload treatment as an output record: the reach
		// attributes surface as top-level keys (including the truthful per-record
		// `aileron.reach.enforced` boolean), not nested under `fields`. That
		// boolean is true when the step ran under a step-scoped proxy credential
		// restricted to the verified sealed reach and false otherwise; its
		// semantics are single-sourced to buildReachRecord in
		// internal/flightplan/runtime/audit.go.
		eventType = string(model.EventTypeFlightPlanLaunchReach)
		payload = rec.Fields
		if payload == nil {
			// A well-formed reach record always carries fields, but guard so an
			// empty record posts an object payload rather than a JSON null.
			payload = map[string]any{}
		}
	case runtime.RecordKindLaunch:
		// A launch record (#1928) carries the same flat `aileron.*` payload
		// treatment as an output/reach record: the per-launch summary fields
		// (sourceInputBindings plus the flat aileron.plan.* / aileron.invocation.id
		// provenance) surface as top-level keys, so the invocation filter
		// (internal/audit/mem.go) and the webapp Timeline accessor
		// (webapp/src/lib/audit/payload.ts) find aileron.invocation.id where they
		// read it. Previously nested under `fields`, which hid the launch record
		// from GET /v1/audit?invocation_id=<id>.
		eventType = string(model.EventTypeFlightPlanLaunch)
		payload = rec.Fields
		if payload == nil {
			// A well-formed launch record always carries fields, but guard so an
			// empty record posts an object payload rather than a JSON null.
			payload = map[string]any{}
		}
	default: // runtime.RecordKindAction
		eventType = string(model.EventTypeFlightPlanLaunchAction)
		payload = actionOrLaunchPayload(rec)
	}

	body, err := json.Marshal(auditIngestRequest{
		EventType: eventType,
		Actor:     auditIngestActor{Type: string(model.ActorTypeHuman), ID: s.actorID},
		Payload:   payload,
	})
	if err != nil {
		s.logErr("encode audit request: %v", err)
		return ""
	}

	base, err := bindingAPIBaseURL()
	if err != nil {
		s.logErr("resolve daemon URL: %v", err)
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audit", bytes.NewReader(body))
	if err != nil {
		s.logErr("build audit request: %v", err)
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	setDaemonAuthorization(req)
	resp, err := actionsHTTPClient.Do(req)
	if err != nil {
		s.logErr("post audit event: %v", err)
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		s.logErr("daemon returned %d for audit append: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		return ""
	}
	var out auditIngestResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		s.logErr("decode audit response: %v", err)
		return ""
	}
	return out.AuditID
}

// actionOrLaunchPayload builds the nested payload shape for the per-action
// record: actionRef and sink when present, and the declared audit fields under a
// "fields" key. The output.materialized, reach, and launch records do NOT use
// this shape; they surface their flat aileron.* map directly so the invocation
// filter and Timeline read aileron.invocation.id at the top level (#1928).
func actionOrLaunchPayload(rec runtime.AuditRecord) map[string]any {
	payload := map[string]any{}
	if rec.ActionRef != "" {
		payload["actionRef"] = rec.ActionRef
	}
	if len(rec.Fields) > 0 {
		payload["fields"] = rec.Fields
	}
	if rec.Sink != "" {
		payload["sink"] = rec.Sink
	}
	return payload
}

func (s daemonAuditSink) logErr(format string, args ...any) {
	if s.stderr == nil {
		return
	}
	fmt.Fprintf(s.stderr, "warning: launch audit not recorded: "+format+"\n", args...)
}

// auditIngestRequest mirrors the daemon's AuditIngestRequest schema
// (POST /v1/audit). Kept local to the CLI so the launch path does not
// import the generated server types.
type auditIngestRequest struct {
	EventType string           `json:"event_type"`
	Actor     auditIngestActor `json:"actor"`
	Payload   map[string]any   `json:"payload"`
}

type auditIngestActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type auditIngestResponse struct {
	AuditID string `json:"audit_id"`
}
