package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/launch"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// toolProxyBootstrapper is the seam the rung-3 tool-container dispatch uses to
// route a step's HTTPS egress through the ADR-0019 daemon forward proxy so a
// matched host binding injects the operator's vault-bound credential at the
// boundary (#1769). It is injected onto containerToolImageRunner rather than
// read from ambient config: a zero-value runner has proxy == nil and stays
// passthrough, which keeps the CLI unit tests deterministic irrespective of any
// live ~/.aileron daemon on the dev/CI box. Production wires the daemon-backed
// bootstrapper via newLaunchToolImageRunner.
type toolProxyBootstrapper interface {
	// Prepare provisions a fresh session CA + authed proxy URL for one tool
	// dispatch and returns the env/CA enrichment, a cleanup func to remove the
	// session CA after the run, and ok=false when daemon config is absent
	// (passthrough). A non-nil error means daemon config resolved but the CA
	// could not be written; the dispatch must fail closed rather than egress
	// un-proxied.
	Prepare(runtimeName, stepID string) (toolProxyBootstrap, func(), bool, error)
}

// toolProxyBootstrap carries the enrichment applied to a tool dispatch's
// RunOptions: the environment variables that point the container's HTTPS client
// at the proxy and trust the mounted CA, plus the read-only CA bind mount.
type toolProxyBootstrap struct {
	// Env is merged onto RunOptions.Env: the CA-bundle vars pointing at the
	// mounted CA, HTTPS_PROXY/https_proxy carrying the authed proxy URL, and the
	// placeholder AWS creds that make botocore pre-sign so the proxy can re-sign.
	Env map[string]string
	// Mount is the read-only CA bind mount appended to the run's volumes.
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

// Placeholder AWS credentials seeded into the tool container so botocore
// pre-signs the request locally; the daemon proxy Header.Del's the client
// signature and re-signs with the vault secret at the boundary. These are
// non-secret constants, harmless when unused by a non-AWS tool: they carry no
// entitlement and never reach an AWS endpoint (the proxy discards the
// placeholder-derived signature). They exist only so a request leaves the
// container in a shape the proxy can re-sign.
const (
	placeholderAWSAccessKeyID = "AKIAIOSFODNN7PLACEHLDR"
	placeholderAWSSecretKey   = "placeholderAileronInjectsRealSecretXXXXXX"
)

// daemonToolProxyBootstrapper is the production toolProxyBootstrapper. It
// resolves the daemon base URL, auth token, and state dir WITHOUT triggering a
// daemon spawn (it reads env/discovery only), so a launch with no running
// daemon stays passthrough rather than forcing a daemon boot. When config
// resolves it provisions the session CA via launch.PrepareToolContainerProxy
// and assembles the env map.
//
// Activation gates on daemon-config PRESENCE (token + base URL from
// AILERON_TOKEN/AILERON_API_URL or ~/.aileron discovery), not on daemon
// LIVENESS: a stale ~/.aileron discovery entry (token present) with no daemon
// actually listening still enriches HTTPS_PROXY/CA and routes every rung-3 tool
// step through a non-existent proxy, failing with CONNECT connection-refused — a
// regression versus direct-egress passthrough. Config-presence gating is
// accepted; liveness-probing / fail-open is deferred to a follow-up. See
// ADR-0019, "Rung-3 tool-container egress reuses the data plane".
type daemonToolProxyBootstrapper struct{}

func (daemonToolProxyBootstrapper) Prepare(runtimeName, stepID string) (toolProxyBootstrap, func(), bool, error) {
	stateDir, err := defaultStateDir()
	if err != nil {
		return toolProxyBootstrap{}, nil, false, nil
	}
	root, ok := resolveDaemonRootURL(stateDir)
	if !ok {
		return toolProxyBootstrap{}, nil, false, nil
	}
	token, ok := resolveDaemonProxyToken(stateDir)
	if !ok {
		return toolProxyBootstrap{}, nil, false, nil
	}

	// Mint a UNIQUE session id per dispatch so no two dispatches (and no agent
	// launch) share a session CA directory. The cleanup closure is defined
	// against that id up front and returned on BOTH the success and the
	// error path, so a PrepareToolContainerProxy that fails after writing a
	// partial CA dir never leaks daemon-side state: the caller runs cleanup.
	sessionID := "flightplan-tool-" + sanitizeSessionSegment(stepID) + "-" + randomSuffix()
	cleanup := func() { _ = launch.CleanupToolContainerProxy(stateDir, sessionID) }

	prepared, err := launch.PrepareToolContainerProxy(stateDir, sessionID, root, runtimeName, token)
	if err != nil {
		return toolProxyBootstrap{}, cleanup, false, fmt.Errorf("skill launch: provision tool container proxy: %w", err)
	}

	env := map[string]string{
		"HTTPS_PROXY":           prepared.ProxyURL,
		"https_proxy":           prepared.ProxyURL,
		"AWS_ACCESS_KEY_ID":     placeholderAWSAccessKeyID,
		"AWS_SECRET_ACCESS_KEY": placeholderAWSSecretKey,
	}
	for _, v := range caBundleEnvVars {
		env[v] = prepared.CAContainerPath
	}

	return toolProxyBootstrap{Env: env, Mount: prepared.CAMount}, cleanup, true, nil
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
// a missing token means the proxy would 407 and the dispatch must stay
// passthrough.
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

// sanitizeSessionSegment reduces a step id to a safe path segment for the
// session directory name so an odd step id can never escape the sessions tree
// (the launch primitive also guards this). It keeps alphanumerics, '-' and '_'
// and replaces everything else with '-', defaulting to "step" when empty.
func sanitizeSessionSegment(stepID string) string {
	var b strings.Builder
	for _, r := range stepID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "step"
	}
	return b.String()
}
