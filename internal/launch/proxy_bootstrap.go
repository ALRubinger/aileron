package launch

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// sandboxProxyEnv is the environment variable the user sets to override
// the default-on proxy bootstrap behavior for `aileron launch`. Values
// follow the same on|off|auto tri-state as the `--sandbox-proxy` flag.
const sandboxProxyEnv = "AILERON_SANDBOX_PROXY"

// Resolution decisions for sandbox proxy bootstrap. Used both as the
// resolution result and as the disabled-reason wire value posted to the
// daemon (matches OpenAPI's SandboxProxyDisabledRequestReason enum).
const (
	sandboxProxyReasonUserOptOut            = "user_opt_out"
	sandboxProxyReasonPreflightFailed       = "preflight_failed"
	sandboxProxyReasonUnsupportedSandboxMode = "unsupported_sandbox_mode"
)

// sandboxProxyStateResolution describes the resolved bootstrap state for
// a launch. The launcher consumes Enabled to decide whether to call
// prepareSandboxProxyBootstrap, Refuse to decide whether the launch
// should hard-fail before starting the container, and DisabledReason to
// drive the sandbox.proxy.disabled audit event when bootstrap is off.
type sandboxProxyStateResolution struct {
	// Enabled is true when the launcher should generate the session
	// CA, mount it, and run the agent through aileron-run-with-proxy-ca.
	Enabled bool
	// Refuse is true when the user explicitly asked for proxy bootstrap
	// but the resolved sandbox configuration cannot satisfy it without
	// preflight (e.g. --sandbox=off --sandbox-proxy=on). The launcher
	// must emit a sandbox.proxy.disabled audit with DisabledReason and
	// exit non-zero before starting the container.
	Refuse bool
	// DisabledReason carries the reason value posted to the daemon's
	// /v1/sandbox-proxy/disabled endpoint when Enabled is false. Empty
	// when Enabled is true and preflight has not yet been attempted.
	DisabledReason string
}

// resolveSandboxProxyState computes whether sandbox proxy bootstrap is
// active for a launch, given the user's flag, env-var, and sandbox mode
// inputs. The precedence is flag > env > default, with `auto` (or
// empty) deferring to the default for the sandbox mode.
//
// Default policy (per the U3 plan): for docker/podman sandbox modes,
// the default is `on`. For every other mode (including the empty
// string and `off`), bootstrap is unsupported and the resolution
// returns Enabled=false with reason unsupported_sandbox_mode. If the
// user explicitly asked for bootstrap against an unsupported mode the
// resolver sets Refuse=true so the launcher can fail preflight.
//
// Unknown values for either input fall back to default behavior so a
// typo in $AILERON_SANDBOX_PROXY can't accidentally bypass bootstrap.
func resolveSandboxProxyState(flagValue, envValue, sandboxMode string) sandboxProxyStateResolution {
	// Precedence: explicit flag > explicit env > default. "auto" (or
	// unrecognized) on the flag defers to the env; "auto" (or
	// unrecognized) on the env defers to the default. Empty inputs are
	// treated the same as "auto" so the launch CLI's default flag
	// value of "auto" is indistinguishable from the user omitting it.
	mode := normalizeSandboxProxyTriState(flagValue)
	if mode == "" || mode == "auto" {
		envMode := normalizeSandboxProxyTriState(envValue)
		if envMode != "" {
			mode = envMode
		}
	}
	if mode == "" {
		mode = "auto"
	}

	supported := sandboxProxyModeSupportsBootstrap(sandboxMode)

	switch mode {
	case "off":
		if !supported {
			return sandboxProxyStateResolution{DisabledReason: sandboxProxyReasonUnsupportedSandboxMode}
		}
		return sandboxProxyStateResolution{DisabledReason: sandboxProxyReasonUserOptOut}
	case "on":
		if !supported {
			return sandboxProxyStateResolution{
				Refuse:         true,
				DisabledReason: sandboxProxyReasonUnsupportedSandboxMode,
			}
		}
		return sandboxProxyStateResolution{Enabled: true}
	default: // auto
		if !supported {
			return sandboxProxyStateResolution{DisabledReason: sandboxProxyReasonUnsupportedSandboxMode}
		}
		return sandboxProxyStateResolution{Enabled: true}
	}
}

// normalizeSandboxProxyTriState normalizes a flag or env value to one
// of "on" / "off" / "auto", or "" when the input is empty (signalling
// "fall through to the next source"). Unrecognized values are treated
// as "" so the resolver falls back to the next source rather than
// silently bypassing bootstrap.
func normalizeSandboxProxyTriState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ""
	case "on", "1", "true", "yes", "bootstrap":
		return "on"
	case "off", "0", "false", "no":
		return "off"
	case "auto":
		return "auto"
	default:
		return ""
	}
}

// sandboxProxyModeSupportsBootstrap reports whether the given
// `--sandbox` mode supports proxy bootstrap. Only the container-runtime
// modes do; the host-launch path (off, empty, "auto") cannot install a
// session CA into a container that does not exist.
func sandboxProxyModeSupportsBootstrap(sandboxMode string) bool {
	switch strings.ToLower(strings.TrimSpace(sandboxMode)) {
	case "docker", "podman", sandboxcontainer.DefaultRuntime:
		return true
	default:
		return false
	}
}

type sandboxProxyBootstrap struct {
	Mode     string
	ProxyURL string
	CAPath   string
	KeyPath  string
	Mounts   []sandboxcontainer.Volume
}

func prepareSandboxProxyBootstrap(stateDir, sessionID, agentEndpointURL, daemonToken string) (sandboxProxyBootstrap, error) {
	if strings.TrimSpace(sessionID) == "" {
		return sandboxProxyBootstrap{}, fmt.Errorf("session id is required")
	}
	if err := validateProxyBootstrapSessionID(sessionID); err != nil {
		return sandboxProxyBootstrap{}, err
	}
	proxyURL := strings.TrimRight(agentEndpointURL, "/")
	if proxyURL == "" {
		return sandboxProxyBootstrap{}, fmt.Errorf("agent endpoint URL is required")
	}
	proxyURL, err := sandboxProxyURLWithSessionAuth(proxyURL, sessionID, daemonToken)
	if err != nil {
		return sandboxProxyBootstrap{}, err
	}
	root := filepath.Join(stateDir, "sessions", sessionID, "sandbox-proxy")
	caPath := filepath.Join(root, "ca.pem")
	keyPath := filepath.Join(root, "ca.key")
	if err := writeSessionCA(caPath, keyPath, sessionID); err != nil {
		return sandboxProxyBootstrap{}, err
	}
	return sandboxProxyBootstrap{
		Mode:     "bootstrap",
		ProxyURL: proxyURL,
		CAPath:   caPath,
		KeyPath:  keyPath,
		Mounts: []sandboxcontainer.Volume{{
			Source:   caPath,
			Target:   sandboxProxyCAPath,
			ReadOnly: true,
		}},
	}, nil
}

func validateProxyBootstrapSessionID(sessionID string) error {
	if sessionID != strings.TrimSpace(sessionID) {
		return fmt.Errorf("session id must not contain surrounding whitespace")
	}
	if sessionID == "." || sessionID == ".." || filepath.Clean(sessionID) != sessionID || filepath.Base(sessionID) != sessionID || strings.ContainsAny(sessionID, `/\:`) {
		return fmt.Errorf("session id %q is not a safe path segment", sessionID)
	}
	return nil
}

func applySandboxProxyBootstrapEnv(agentEnv map[string]string, bootstrap sandboxProxyBootstrap) {
	if bootstrap.Mode == "" {
		return
	}
	agentEnv["AILERON_SANDBOX_PROXY_MODE"] = bootstrap.Mode
	agentEnv["AILERON_SANDBOX_PROXY_URL"] = bootstrap.ProxyURL
	agentEnv["AILERON_SANDBOX_PROXY_CA_FILE"] = sandboxProxyCAPath
	agentEnv["HTTPS_PROXY"] = bootstrap.ProxyURL
	agentEnv["HTTP_PROXY"] = bootstrap.ProxyURL
	agentEnv["NO_PROXY"] = mergeNoProxy(agentEnv["NO_PROXY"], bootstrap.ProxyURL)
}

func mergeNoProxy(existing, proxyURL string) string {
	entries := []string{}
	seen := map[string]struct{}{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		entries = append(entries, value)
	}
	for _, value := range strings.Split(existing, ",") {
		add(value)
	}
	for _, value := range []string{"localhost", "127.0.0.1", "::1", "host.docker.internal", "host.containers.internal"} {
		add(value)
	}
	if parsed, err := url.Parse(proxyURL); err == nil {
		host := parsed.Hostname()
		add(host)
		if ip := net.ParseIP(host); ip != nil {
			add(ip.String())
		}
	}
	return strings.Join(entries, ",")
}

func sandboxProxyURLWithSessionAuth(rawURL, sessionID, daemonToken string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("sandbox proxy URL must be absolute")
	}
	if daemonToken != "" {
		parsed.User = url.UserPassword(sessionID, daemonToken)
	} else {
		parsed.User = url.User(sessionID)
	}
	return parsed.String(), nil
}

func writeSessionCA(caPath, keyPath, sessionID string) error {
	if err := os.MkdirAll(filepath.Dir(caPath), 0o700); err != nil {
		return fmt.Errorf("create sandbox proxy CA dir: %w", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate sandbox proxy CA key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate sandbox proxy CA serial: %w", err)
	}
	now := time.Now().UTC()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Aileron sandbox session CA",
			Organization: []string{"Aileron"},
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(12 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create sandbox proxy CA cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(caPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write sandbox proxy CA cert: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write sandbox proxy CA key: %w", err)
	}
	return nil
}
