package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"
)

// dockerBridgeProbeTimeout bounds the `docker network inspect` /
// `ip route` subprocess calls used to derive the bridge-gateway IP. The
// daemon must not hang at startup if the Docker daemon is unresponsive.
const dockerBridgeProbeTimeout = 5 * time.Second

// Indirections over package-level dependencies so unit tests can drive
// the derivation logic without a real Docker daemon. Production wires
// these to the real OS / Docker.
var (
	// hostGOOS is runtime.GOOS behind a seam so tests can simulate
	// non-Linux hosts (where no bridge bind is needed).
	hostGOOS = func() string { return goruntime.GOOS }
	// dockerNetworkInspectBridge returns the raw stdout of
	// `docker network inspect bridge`. Returns an error when Docker is
	// absent or the command fails.
	dockerNetworkInspectBridge = realDockerNetworkInspectBridge
	// docker0RouteGateway returns the bridge-gateway IP derived from the
	// host's route to the docker0 interface, used as a fallback when
	// `docker network inspect` yields no usable gateway.
	docker0RouteGateway = realDocker0RouteGateway
	// dockerOnPath reports whether a `docker` binary is resolvable on
	// PATH. Behind a seam so the bridge-bind regression test can force
	// the Linux+Docker path without a real Docker install.
	dockerOnPath = func() bool {
		_, err := exec.LookPath("docker")
		return err == nil
	}
)

// bridgeBindNeeded reports whether this host needs a second listener on
// the Docker bridge-gateway IP for in-container reachability.
//
// Only Linux+Docker needs it: there `host.docker.internal` resolves
// (via `--add-host host.docker.internal:host-gateway`) to the bridge
// gateway IP, NOT loopback, so a loopback-only daemon is unreachable
// from the sandbox container. On macOS / Windows, Docker Desktop
// forwards `host.docker.internal` to the host loopback, so the existing
// 127.0.0.1 listener already covers the container path and no second
// bind is needed. Non-Docker Linux likewise needs nothing.
func bridgeBindNeeded() bool {
	if hostGOOS() != "linux" {
		return false
	}
	return dockerOnPath()
}

// deriveDockerBridgeGatewayIP returns the Docker default-bridge gateway
// IP (the host end of docker0, what `host-gateway` resolves to on
// Linux). It tries `docker network inspect bridge` first (authoritative:
// the gateway Docker actually configured), falling back to the host's
// route to the docker0 interface.
//
// A failed derivation is returned as an error: per the issue the daemon
// must fail loud and actionable rather than silently binding wider
// (e.g. 0.0.0.0). Callers gate this behind [bridgeBindNeeded] so it only
// runs on Linux+Docker.
func deriveDockerBridgeGatewayIP(ctx context.Context) (string, error) {
	ip, primaryErr := bridgeGatewayFromNetworkInspect(ctx)
	if primaryErr == nil {
		return ip, nil
	}

	ip, fallbackErr := docker0RouteGateway(ctx)
	if fallbackErr == nil {
		return ip, nil
	}

	return "", fmt.Errorf("derive docker bridge-gateway IP: docker network inspect bridge failed (%v) and docker0 route fallback failed (%v); "+
		"the daemon needs this IP to be reachable from the sandbox container on Linux — "+
		"verify Docker is running and the default bridge network exists", primaryErr, fallbackErr)
}

// dockerNetwork mirrors the subset of `docker network inspect` JSON the
// derivation needs: the IPAM gateway addresses configured on the bridge
// network.
type dockerNetwork struct {
	IPAM struct {
		Config []struct {
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// bridgeGatewayFromNetworkInspect parses `docker network inspect bridge`
// and returns the first IPv4 gateway in IPAM.Config. IPv6-only gateways
// are skipped: the `--add-host host-gateway` path and docker0 default to
// IPv4, so an IPv4 gateway is what the container reaches.
func bridgeGatewayFromNetworkInspect(ctx context.Context) (string, error) {
	out, err := dockerNetworkInspectBridge(ctx)
	if err != nil {
		return "", err
	}
	var nets []dockerNetwork
	if err := json.Unmarshal(out, &nets); err != nil {
		return "", fmt.Errorf("parse docker network inspect output: %w", err)
	}
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			gw := strings.TrimSpace(c.Gateway)
			if gw == "" {
				continue
			}
			ip := net.ParseIP(gw)
			if ip == nil || ip.To4() == nil {
				continue
			}
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 gateway in docker bridge IPAM config")
}

// realDockerNetworkInspectBridge runs `docker network inspect bridge`
// and returns its stdout. Bounded by [dockerBridgeProbeTimeout].
func realDockerNetworkInspectBridge(ctx context.Context) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, dockerBridgeProbeTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, "docker", "network", "inspect", "bridge")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker network inspect bridge: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// realDocker0RouteGateway derives the bridge-gateway IP from the host's
// route to the docker0 interface via `ip -j route show dev docker0`. The
// docker0 interface's own address is the gateway containers reach, so we
// read the route's `prefsrc` (the source address the host uses on that
// link), which equals the bridge-gateway IP.
//
// Used only as a fallback when `docker network inspect bridge` fails;
// callers gate it behind Linux+Docker.
func realDocker0RouteGateway(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, dockerBridgeProbeTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, "ip", "-j", "route", "show", "dev", "docker0")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ip route show dev docker0: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var routes []struct {
		PrefSrc string `json:"prefsrc"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &routes); err != nil {
		return "", fmt.Errorf("parse ip route output: %w", err)
	}
	for _, r := range routes {
		src := strings.TrimSpace(r.PrefSrc)
		if src == "" {
			continue
		}
		ip := net.ParseIP(src)
		if ip == nil || ip.To4() == nil {
			continue
		}
		return ip.String(), nil
	}
	return "", fmt.Errorf("no IPv4 prefsrc on docker0 route")
}
