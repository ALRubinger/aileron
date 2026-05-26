package sandbox

import (
	"net"
	"net/url"
	"strings"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// HostPolicy enforces the connector manifest's `[capabilities.network]`
// declaration: outbound calls to undeclared hosts are denied at the
// syscall layer per ADR-0005 acceptance criterion #3.
//
// Hosts are pinned `host:port` pairs (per ADR-0002 §"network access");
// no wildcards are honored. The match is exact on both the host and the
// port.
type HostPolicy struct {
	allowed map[string]struct{}
}

// NewHostPolicy builds a HostPolicy from a connector manifest. A
// missing `[capabilities.network]` block produces a deny-all policy.
func NewHostPolicy(m *cstore.Manifest) *HostPolicy {
	allowed := map[string]struct{}{}
	if m != nil && m.Capabilities.Network != nil {
		for _, h := range m.Capabilities.Network.Hosts {
			allowed[normalizeHostPort(h)] = struct{}{}
		}
	}
	return &HostPolicy{allowed: allowed}
}

// CheckURL parses `rawURL` and verifies its host:port is in the allowed
// set. Returns a structured *Error of class `capability_denied` and
// boundary `sandbox` when the call is refused.
//
// HTTPS URLs default to port 443; HTTP URLs default to port 80; URLs
// with explicit ports use those. Schemes other than http/https are
// rejected — the connector ABI exposes HTTP only in v1, and any other
// scheme is a misuse of the host import.
func (p *HostPolicy) CheckURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return newCapabilityDenied("invalid URL", map[string]any{
			"requested": rawURL,
			"reason":    err.Error(),
		})
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return newCapabilityDenied("scheme not permitted", map[string]any{
			"requested": rawURL,
			"scheme":    u.Scheme,
		})
	}
	host := u.Hostname()
	if host == "" {
		return newCapabilityDenied("missing host", map[string]any{
			"requested": rawURL,
		})
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	candidate := host + ":" + port
	if _, ok := p.allowed[candidate]; !ok {
		granted := make([]string, 0, len(p.allowed))
		for h := range p.allowed {
			granted = append(granted, h)
		}
		return newCapabilityDenied("host:port not in declared grant", map[string]any{
			"requested": "network:" + candidate,
			"granted":   prefixed(granted, "network:"),
		})
	}
	return nil
}

// CheckHostPort verifies a `host:port` string is in the allowed
// set. Mirrors [HostPolicy.CheckURL]'s contract for the spawn-proxy
// path, where the proxy receives a host:port directly from the
// CONNECT request and does not have a full URL to parse.
//
// Returns a structured `*Error` of class `capability_denied` and
// boundary `sandbox` when the call is refused.
func (p *HostPolicy) CheckHostPort(hostPort string) error {
	candidate := normalizeHostPort(hostPort)
	if _, ok := p.allowed[candidate]; !ok {
		granted := make([]string, 0, len(p.allowed))
		for h := range p.allowed {
			granted = append(granted, h)
		}
		return newCapabilityDenied("host:port not in declared grant", map[string]any{
			"requested": "network:" + candidate,
			"granted":   prefixed(granted, "network:"),
		})
	}
	return nil
}

// AllowedHosts returns a sorted-stable copy of the policy's grant for
// auditing/error-detail purposes.
func (p *HostPolicy) AllowedHosts() []string {
	out := make([]string, 0, len(p.allowed))
	for h := range p.allowed {
		out = append(out, h)
	}
	return out
}

// normalizeHostPort lowercases the host portion of a `host:port` pair
// since DNS is case-insensitive (RFC 1035) but the manifest may have
// arbitrary casing.
func normalizeHostPort(hp string) string {
	host, port, err := net.SplitHostPort(hp)
	if err != nil {
		return hp
	}
	return strings.ToLower(host) + ":" + port
}

// prefixed returns a new slice where each element is `prefix + s`.
func prefixed(in []string, prefix string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = prefix + s
	}
	return out
}
