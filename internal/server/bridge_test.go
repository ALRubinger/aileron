package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// withStubbedHostGOOS swaps hostGOOS for the duration of the test and
// restores it on cleanup.
func withStubbedHostGOOS(t *testing.T, goos string) {
	t.Helper()
	prev := hostGOOS
	hostGOOS = func() string { return goos }
	t.Cleanup(func() { hostGOOS = prev })
}

// withStubbedDerivation swaps the two Docker-probing seams so the
// derivation logic can be exercised without a real Docker daemon.
func withStubbedDerivation(t *testing.T, inspect func(context.Context) ([]byte, error), route func(context.Context) (string, error)) {
	t.Helper()
	prevInspect := dockerNetworkInspectBridge
	prevRoute := docker0RouteGateway
	dockerNetworkInspectBridge = inspect
	docker0RouteGateway = route
	t.Cleanup(func() {
		dockerNetworkInspectBridge = prevInspect
		docker0RouteGateway = prevRoute
	})
}

// TestBridgeBindNeeded_NonLinux pins the contract that non-Linux hosts
// never need a bridge listener: Docker Desktop forwards
// host.docker.internal to the host loopback, so the existing 127.0.0.1
// listener already covers the container path.
func TestBridgeBindNeeded_NonLinux(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			withStubbedHostGOOS(t, goos)
			if bridgeBindNeeded() {
				t.Errorf("bridgeBindNeeded() = true on %s; want false (Docker Desktop forwards to loopback)", goos)
			}
		})
	}
}

// TestBridgeBindNeeded_LinuxDocker covers the positive branch: Linux
// with a docker binary on PATH needs the bridge listener.
func TestBridgeBindNeeded_LinuxDocker(t *testing.T) {
	withStubbedHostGOOS(t, "linux")
	prev := dockerOnPath
	dockerOnPath = func() bool { return true }
	t.Cleanup(func() { dockerOnPath = prev })
	if !bridgeBindNeeded() {
		t.Error("bridgeBindNeeded() = false on Linux+Docker; want true")
	}
}

// TestBridgeBindNeeded_LinuxNoDocker covers non-Docker Linux: no docker
// binary means no bridge listener is needed.
func TestBridgeBindNeeded_LinuxNoDocker(t *testing.T) {
	withStubbedHostGOOS(t, "linux")
	prev := dockerOnPath
	dockerOnPath = func() bool { return false }
	t.Cleanup(func() { dockerOnPath = prev })
	if bridgeBindNeeded() {
		t.Error("bridgeBindNeeded() = true on Linux without Docker; want false")
	}
}

// TestDeriveDockerBridgeGatewayIP_PrimaryInspect covers the happy path:
// `docker network inspect bridge` yields an IPv4 gateway and that IP is
// returned without consulting the docker0-route fallback.
func TestDeriveDockerBridgeGatewayIP_PrimaryInspect(t *testing.T) {
	inspectJSON := `[{"IPAM":{"Config":[{"Subnet":"172.17.0.0/16","Gateway":"172.17.0.1"}]}}]`
	withStubbedDerivation(t,
		func(context.Context) ([]byte, error) { return []byte(inspectJSON), nil },
		func(context.Context) (string, error) {
			t.Error("docker0 route fallback should not run when inspect succeeds")
			return "", nil
		},
	)
	ip, err := deriveDockerBridgeGatewayIP(context.Background())
	if err != nil {
		t.Fatalf("deriveDockerBridgeGatewayIP: %v", err)
	}
	if ip != "172.17.0.1" {
		t.Errorf("ip = %q, want 172.17.0.1", ip)
	}
}

// TestDeriveDockerBridgeGatewayIP_SkipsIPv6Gateway verifies the
// derivation skips IPv6-only IPAM entries and returns the first IPv4
// gateway, matching the IPv4 `host-gateway`/docker0 reachability path.
func TestDeriveDockerBridgeGatewayIP_SkipsIPv6Gateway(t *testing.T) {
	inspectJSON := `[{"IPAM":{"Config":[{"Gateway":"fd00::1"},{"Gateway":"172.18.0.1"}]}}]`
	withStubbedDerivation(t,
		func(context.Context) ([]byte, error) { return []byte(inspectJSON), nil },
		func(context.Context) (string, error) { return "", errors.New("unused") },
	)
	ip, err := deriveDockerBridgeGatewayIP(context.Background())
	if err != nil {
		t.Fatalf("deriveDockerBridgeGatewayIP: %v", err)
	}
	if ip != "172.18.0.1" {
		t.Errorf("ip = %q, want 172.18.0.1 (IPv6 gateway should be skipped)", ip)
	}
}

// TestDeriveDockerBridgeGatewayIP_FallsBackToRoute covers the fallback:
// when `docker network inspect` fails (or yields no usable gateway), the
// docker0-route derivation supplies the IP.
func TestDeriveDockerBridgeGatewayIP_FallsBackToRoute(t *testing.T) {
	withStubbedDerivation(t,
		func(context.Context) ([]byte, error) { return nil, errors.New("docker daemon down") },
		func(context.Context) (string, error) { return "172.17.0.1", nil },
	)
	ip, err := deriveDockerBridgeGatewayIP(context.Background())
	if err != nil {
		t.Fatalf("deriveDockerBridgeGatewayIP: %v", err)
	}
	if ip != "172.17.0.1" {
		t.Errorf("ip = %q, want 172.17.0.1 from fallback", ip)
	}
}

// TestDeriveDockerBridgeGatewayIP_NoIPv4InInspectFallsBack pins that an
// inspect output with only IPv6 gateways (no IPv4) is treated as a
// derivation miss and triggers the docker0-route fallback rather than
// erroring outright.
func TestDeriveDockerBridgeGatewayIP_NoIPv4InInspectFallsBack(t *testing.T) {
	inspectJSON := `[{"IPAM":{"Config":[{"Gateway":"fd00::1"}]}}]`
	withStubbedDerivation(t,
		func(context.Context) ([]byte, error) { return []byte(inspectJSON), nil },
		func(context.Context) (string, error) { return "172.19.0.1", nil },
	)
	ip, err := deriveDockerBridgeGatewayIP(context.Background())
	if err != nil {
		t.Fatalf("deriveDockerBridgeGatewayIP: %v", err)
	}
	if ip != "172.19.0.1" {
		t.Errorf("ip = %q, want 172.19.0.1 from fallback", ip)
	}
}

// TestDeriveDockerBridgeGatewayIP_HardErrorOnBothFail is the core
// security property from issue #1471: when neither derivation source
// yields a gateway, the daemon must fail loud and actionable rather than
// silently binding a wider address (e.g. 0.0.0.0). The error must name
// both failed sources.
func TestDeriveDockerBridgeGatewayIP_HardErrorOnBothFail(t *testing.T) {
	withStubbedDerivation(t,
		func(context.Context) ([]byte, error) { return nil, errors.New("inspect boom") },
		func(context.Context) (string, error) { return "", errors.New("route boom") },
	)
	_, err := deriveDockerBridgeGatewayIP(context.Background())
	if err == nil {
		t.Fatal("deriveDockerBridgeGatewayIP: expected hard error when both sources fail, got nil")
	}
	if !strings.Contains(err.Error(), "inspect boom") || !strings.Contains(err.Error(), "route boom") {
		t.Errorf("error = %q; want both underlying failures named", err)
	}
}

// TestDeriveDockerBridgeGatewayIP_MalformedInspectFallsBack ensures
// unparseable inspect JSON is treated as a primary-source failure and
// triggers the fallback rather than panicking or returning garbage.
func TestDeriveDockerBridgeGatewayIP_MalformedInspectFallsBack(t *testing.T) {
	withStubbedDerivation(t,
		func(context.Context) ([]byte, error) { return []byte(`{not json`), nil },
		func(context.Context) (string, error) { return "172.20.0.1", nil },
	)
	ip, err := deriveDockerBridgeGatewayIP(context.Background())
	if err != nil {
		t.Fatalf("deriveDockerBridgeGatewayIP: %v", err)
	}
	if ip != "172.20.0.1" {
		t.Errorf("ip = %q, want 172.20.0.1", ip)
	}
}
