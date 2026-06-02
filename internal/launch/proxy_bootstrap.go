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

const sandboxProxyBootstrapEnv = "AILERON_SANDBOX_PROXY_BOOTSTRAP"

type sandboxProxyBootstrap struct {
	Mode     string
	ProxyURL string
	CAPath   string
	KeyPath  string
	Mounts   []sandboxcontainer.Volume
}

func prepareSandboxProxyBootstrap(stateDir, sessionID, agentEndpointURL string) (sandboxProxyBootstrap, error) {
	if !sandboxProxyBootstrapEnabled() {
		return sandboxProxyBootstrap{}, nil
	}
	if strings.TrimSpace(sessionID) == "" {
		return sandboxProxyBootstrap{}, fmt.Errorf("session id is required")
	}
	proxyURL := strings.TrimRight(agentEndpointURL, "/")
	if proxyURL == "" {
		return sandboxProxyBootstrap{}, fmt.Errorf("agent endpoint URL is required")
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

func sandboxProxyBootstrapEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(sandboxProxyBootstrapEnv))) {
	case "1", "true", "yes", "on", "bootstrap":
		return true
	default:
		return false
	}
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
