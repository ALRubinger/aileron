package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// planProxyBootstrapper is the seam the whole-plan image boot uses to route the
// booted plan container's HTTPS egress through the ADR-0019 daemon forward proxy
// so a matched host binding injects the operator's vault-bound credential at the
// boundary (#1828). It is injected onto containerImageRunner rather than read
// from ambient config: a zero-value runner has proxy == nil and stays
// passthrough, which keeps the CLI unit tests deterministic irrespective of any
// live ~/.aileron daemon on the dev/CI box. Production wires the daemon-backed
// bootstrapper via newLaunchImageRunner.
type planProxyBootstrapper interface {
	// Prepare provisions a fresh session CA + authed proxy URL for one whole-plan
	// boot and returns the env/CA enrichment, a cleanup func to remove the
	// session CA after the run, and ok=false when daemon config is absent
	// (passthrough). A non-nil error means daemon config resolved but the boot
	// could not be provisioned (CA write failed, or the image's declared
	// credential conventions were malformed); the boot must fail closed rather
	// than egress un-proxied. On the error path the returned cleanup is still
	// runnable so a partial session CA never leaks.
	Prepare(ctx context.Context, runtimeName, image, planName string) (planProxyBootstrap, func(), bool, error)
}

// planProxyBootstrap carries the enrichment applied to a whole-plan boot's
// RunOptions: the environment variables that point the container's HTTPS client
// at the proxy and trust the mounted CA, plus the read-only CA bind mount.
type planProxyBootstrap struct {
	// Env is merged onto RunOptions.Env: the CA-bundle vars pointing at the
	// mounted CA, HTTPS_PROXY/https_proxy carrying the authed proxy URL,
	// NO_PROXY/no_proxy so loopback/daemon traffic bypasses the proxy, and the
	// catalog-derived placeholder creds that make each declared tool pre-sign so
	// the proxy can re-sign or swap at the boundary.
	Env map[string]string
	// Mount is the read-only CA bind mount appended to the boot's volumes.
	Mount sandboxcontainer.Volume
}

// caBundleEnvVars is the declarative list of CA-bundle environment variables set
// to the in-container CA path so the common HTTPS clients (botocore, requests,
// node, openssl, git, curl) trust the daemon's per-host leaf. It is a slice so
// the set is testable and additions stay in one place. Tools that honor no
// CA-bundle env var and consult only the system trust store are out of scope
// for #1769 (see ADR-0019).
var caBundleEnvVars = []string{
	"AWS_CA_BUNDLE",
	"REQUESTS_CA_BUNDLE",
	"NODE_EXTRA_CA_CERTS",
	"SSL_CERT_FILE",
	"GIT_SSL_CAINFO",
	"CURL_CA_BUNDLE",
}

// bootNoProxyHosts are the hosts the booted plan container must reach WITHOUT
// routing through the egress proxy. Unlike a per-step tool container, the booted
// plan container calls the host daemon (AILERON_API_URL at host.docker.internal,
// #1759) for every action + audit POST; loopback and the daemon host must never
// CONNECT through the forward proxy. This is a deliberate delta from the removed
// per-dispatch env set, which carried no NO_PROXY.
const bootNoProxyHosts = "localhost,127.0.0.1,::1,host.docker.internal"

// containerImageMetadataLabel reads the devcontainer.metadata OCI label of the
// pinned image (empty when unlabeled or uninspectable). It is a package variable
// so the CLI tests swap it for a fake and never shell out to
// `docker image inspect`, mirroring the containerBakedCLIVersion seam. The
// label carries the declared tools' merged customizations.aileron.credential
// blocks, from which the boot derives the placeholder env union.
var containerImageMetadataLabel = func(ctx context.Context, runtimeName, image string) string {
	return sandboxcontainer.ImageMetadataLabel(ctx, sandboxcontainer.DefaultRunner(), runtimeName, image)
}

// containerEnsureImageLocal materializes image into the local container daemon
// before launch-time code reads daemon-local image labels. The registry resolver
// verifies the signed pin through ORAS and returns a content-addressed boot ref,
// but that does not load the image into Docker; a later docker run would
// auto-pull it after this label read. Pulling here closes that cold-cache race.
var containerEnsureImageLocal = func(ctx context.Context, runtimeName, image string) error {
	return ensureImageLocal(ctx, sandboxcontainer.DefaultRunner(), runtimeName, image)
}

func ensureImageLocal(ctx context.Context, runner sandboxcontainer.Runner, runtimeName, image string) error {
	if err := runner.Run(ctx, runtimeName, []string{"image", "inspect", image}, io.Discard, io.Discard); err == nil {
		return nil
	}
	if err := runner.Run(ctx, runtimeName, []string{"pull", image}, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("%s pull %s: %w", runtimeName, image, err)
	}
	return nil
}

// daemonPlanProxyBootstrapper is the production planProxyBootstrapper. It
// resolves the daemon base URL, auth token, and state dir WITHOUT triggering a
// daemon spawn (it reads env/discovery only), so a launch with no running
// daemon stays passthrough rather than forcing a daemon boot. When config
// resolves it provisions the session CA via launch.PrepareContainerProxy, reads
// the booted image's declared credential conventions from its
// devcontainer.metadata label, and assembles the env map.
//
// Activation gates on daemon-config PRESENCE (token + base URL from
// AILERON_TOKEN/AILERON_API_URL or ~/.aileron discovery), not on daemon
// LIVENESS: a stale ~/.aileron discovery entry (token present) with no daemon
// actually listening still enriches HTTPS_PROXY/CA and routes the booted plan's
// egress through a non-existent proxy, failing with CONNECT connection-refused —
// a regression versus direct-egress passthrough. Config-presence gating is
// accepted; liveness-probing / fail-open is deferred to a follow-up. See
// ADR-0019.
type daemonPlanProxyBootstrapper struct{}

func (daemonPlanProxyBootstrapper) Prepare(ctx context.Context, runtimeName, image, planName string) (planProxyBootstrap, func(), bool, error) {
	stateDir, err := defaultStateDir()
	if err != nil {
		return planProxyBootstrap{}, nil, false, nil
	}
	root, ok := resolveDaemonRootURL(stateDir)
	if !ok {
		return planProxyBootstrap{}, nil, false, nil
	}
	token, ok := resolveDaemonProxyToken(stateDir)
	if !ok {
		return planProxyBootstrap{}, nil, false, nil
	}

	// Registry-origin launches may have only resolved the signed image through
	// ORAS. Materialize it into the local daemon before reading
	// devcontainer.metadata so first launch sees the same labels as a warm-cache
	// retry. No-daemon passthrough launches intentionally skip this block.
	if err := containerEnsureImageLocal(ctx, runtimeName, image); err != nil {
		return planProxyBootstrap{}, nil, false, fmt.Errorf("skill launch: materialize pinned image %q for credential metadata: %w", image, err)
	}

	// Mint a UNIQUE session id per boot so no two boots (and no agent launch)
	// share a session CA directory. The cleanup closure is defined against that
	// id up front and returned on BOTH the success and the error path, so a
	// PrepareContainerProxy that fails after writing a partial CA dir, or a
	// malformed-metadata refusal after the CA is written, never leaks daemon-side
	// state: the caller runs cleanup.
	sessionID := "flightplan-boot-" + sanitizeSessionSegment(planName) + "-" + randomSuffix()
	cleanup := func() { _ = launch.CleanupContainerProxy(stateDir, sessionID) }

	prepared, err := launch.PrepareContainerProxy(stateDir, sessionID, root, runtimeName, token)
	if err != nil {
		return planProxyBootstrap{}, cleanup, false, fmt.Errorf("skill launch: provision plan container proxy: %w", err)
	}

	// Read the declared tools' credential conventions off the exact signed pin
	// being booted (its devcontainer.metadata label) and derive the placeholder
	// env union. An unlabeled/uninspectable image reads as "" and contributes an
	// empty union (fail-soft, matching ImageMetadataLabel's posture); a present
	// but malformed convention is a loud error that refuses the boot (a
	// present-but-broken convention must not silently ship nothing — #1825).
	convs, err := composition.ConventionsFromMetadata([]byte(containerImageMetadataLabel(ctx, runtimeName, image)))
	if err != nil {
		return planProxyBootstrap{}, cleanup, false, fmt.Errorf("skill launch: read image credential conventions: %w", err)
	}
	placeholders, err := composition.PlaceholderEnv(convs)
	if err != nil {
		return planProxyBootstrap{}, cleanup, false, fmt.Errorf("skill launch: assemble placeholder env: %w", err)
	}

	// Seed the catalog-derived placeholders FIRST, then let the reserved
	// proxy/CA keys overwrite them, so an image-declared placeholder can never
	// clobber the authoritative HTTPS_PROXY/NO_PROXY/CA-bundle settings and
	// weaken the fail-closed egress mediation. A placeholder that (mis)declares a
	// reserved env is silently overridden by the reserved value below rather than
	// winning.
	env := map[string]string{}
	for k, v := range placeholders {
		env[k] = v
	}
	env["HTTPS_PROXY"] = prepared.ProxyURL
	env["https_proxy"] = prepared.ProxyURL
	env["NO_PROXY"] = bootNoProxyHosts
	env["no_proxy"] = bootNoProxyHosts
	for _, v := range caBundleEnvVars {
		env[v] = prepared.CAContainerPath
	}

	return planProxyBootstrap{Env: env, Mount: prepared.CAMount}, cleanup, true, nil
}

// imageDaemonEnv is the seam the Flight Plan image-boot path (#1759) uses to
// inject the host daemon coordinates into the booted container's environment so
// the re-entered `aileron skill launch` inside the image reaches the SAME host
// daemon action + audit boundary rather than resolving/spawning an ephemeral
// in-container daemon nothing on the host can query. It is injected onto
// containerImageRunner rather than read from ambient config: a zero-value runner
// has daemonEnv == nil and stays passthrough (no injected env), which keeps the
// CLI unit tests deterministic irrespective of any live ~/.aileron daemon.
// Production wires the daemon-backed resolver via newLaunchImageRunner.
type imageDaemonEnv interface {
	// Env resolves the daemon env map (AILERON_API_URL + AILERON_TOKEN) for a
	// container boot on the named runtime, WITHOUT spawning a daemon. ok=false
	// means daemon config is absent (URL or token unresolvable) and the boot must
	// carry no injected env rather than break a no-daemon launch.
	Env(runtimeName string) (map[string]string, bool)
}

// daemonImageEnv is the production imageDaemonEnv. It resolves the daemon base
// URL + auth token from AILERON_API_URL / AILERON_TOKEN or ~/.aileron discovery
// WITHOUT triggering a daemon spawn (it reads env/discovery only), so a launch
// with no running daemon stays passthrough rather than forcing a daemon boot.
//
// Activation gates on daemon-config PRESENCE (token + base URL), not on daemon
// LIVENESS: a stale ~/.aileron discovery entry (token present) with no daemon
// actually listening still injects env, and the inner launch's audit POST fails
// with connection-refused — a regression versus the ephemeral in-container
// daemon. Config-presence gating is accepted; liveness-probing / fail-open is
// deferred to a follow-up, mirroring daemonPlanProxyBootstrapper.
type daemonImageEnv struct{}

func (daemonImageEnv) Env(runtimeName string) (map[string]string, bool) {
	stateDir, err := defaultStateDir()
	if err != nil {
		return nil, false
	}
	root, ok := resolveDaemonRootURL(stateDir)
	if !ok {
		return nil, false
	}
	token, ok := resolveDaemonProxyToken(stateDir)
	if !ok {
		return nil, false
	}
	return map[string]string{
		"AILERON_API_URL": daemonImageAPIURL(root, runtimeName),
		"AILERON_TOKEN":   token,
	}, true
}

// daemonImageAPIURL builds the AILERON_API_URL injected into the booted
// container: the daemon root (no /v1) is loopback-rewritten to
// host.docker.internal so the in-container process reaches the host-bound
// daemon, then the /v1 API prefix is appended. The inner launch's
// bindingAPIBaseURL uses this value directly as base + "/actions/..." and
// base + "/audit", so the injected value MUST carry the /v1 suffix (the host
// rewrite is applied to the host portion only and preserves the path). This is
// the container-facing analogue of internal/launch's daemonAPIBaseURL over
// ContainerURLForRuntime.
func daemonImageAPIURL(root, runtimeName string) string {
	rewritten := launch.ContainerURLForRuntime(root, runtimeName)
	return strings.TrimRight(rewritten, "/") + "/v1"
}

// resolveDaemonRootURL resolves the daemon root URL (WITHOUT the /v1 suffix)
// from AILERON_API_URL or discovery, without spawning a daemon. It returns
// ok=false when neither source yields a URL.
func resolveDaemonRootURL(stateDir string) (string, bool) {
	if u := strings.TrimSpace(os.Getenv("AILERON_API_URL")); u != "" {
		return stripV1Suffix(u), true
	}
	info, err := discovery.Read(stateDir)
	if err != nil || strings.TrimSpace(info.URL) == "" {
		return "", false
	}
	return stripV1Suffix(info.URL), true
}

// resolveDaemonProxyToken resolves the daemon auth token from AILERON_TOKEN or
// discovery, without spawning a daemon. It returns ok=false when neither source
// yields a token: the CONNECT handshake authenticates password=daemonToken, so
// a missing token means the proxy would 407 and the boot must stay passthrough.
//
// The token resolved here is the FULL daemon token, and daemonImageEnv.Env
// injects it verbatim as AILERON_TOKEN into the booted container. This differs
// from the agent-sandbox path (internal/launch/launcher.go:720-726), which
// injects a session-scoped caveat token carrying only the capabilities the
// session uses. Injecting the full token on the Flight Plan image-boot path is
// a deliberate, readiness-brief-sanctioned choice: this path has no
// session-caveat minting step, so no narrowed token exists to hand the
// container. Future hardening could mint a session-scoped caveat token for the
// image boot and inject that here instead.
func resolveDaemonProxyToken(stateDir string) (string, bool) {
	if t := strings.TrimSpace(os.Getenv("AILERON_TOKEN")); t != "" {
		return t, true
	}
	info, err := discovery.Read(stateDir)
	if err != nil || strings.TrimSpace(info.Token) == "" {
		return "", false
	}
	return info.Token, true
}

// stripV1Suffix trims a trailing slash and a trailing /v1 so an AILERON_API_URL
// or discovery URL that includes the /v1 API prefix yields the daemon root the
// forward proxy CONNECT is served on.
func stripV1Suffix(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	return strings.TrimSuffix(trimmed, "/v1")
}

// sanitizeSessionSegment reduces an id to a safe path segment for the session
// directory name so an odd plan name can never escape the sessions tree (the
// launch primitive also guards this). It keeps alphanumerics, '-' and '_' and
// replaces everything else with '-', defaulting to "plan" when empty.
func sanitizeSessionSegment(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "plan"
	}
	return b.String()
}
