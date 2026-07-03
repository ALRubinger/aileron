//go:build integration_sandbox

// Real-container end-to-end coverage for the Flight Plan image-boot audit-trail
// reachability contract (#1759).
//
// This test proves the load-bearing #1759 contract: a record emitted by the
// re-entered `aileron skill launch` INSIDE the booted environment image
// surfaces on the HOST daemon's audit trail. The image-boot path injects the
// host daemon coordinates (AILERON_API_URL + AILERON_TOKEN, host-rewritten to
// host.docker.internal, /v1-suffixed) into the container so the inner launch
// posts its actions + audit records to the SAME host daemon rather than an
// ephemeral in-container one nothing on the host can query.
//
// Unlike the CLI unit tests (which assert the RunOptions.Env the runner
// constructs with a fake resolver and no Docker), this test drives a real
// container boot through containerImageRunner.Run against a LIVE host daemon,
// then queries that daemon's GET /v1/audit to assert the launch record landed
// on the host store.
//
// FORBIDDEN (the #1757 mirror-test failure mode): this test must NOT re-drive
// the host-side audit sink as a proxy for container delivery. It observes the
// host daemon's store directly, so a boot that failed to reach the host daemon
// FAILS rather than passing on a locally-driven sink.
//
// The wiring is env-driven so one body serves the CI job and a local run:
//
//	AILERON_SANDBOX_FLIGHTPLAN_IMAGE  pinned environment image ref to boot
//	AILERON_API_URL                   live host daemon API base (with /v1)
//	AILERON_TOKEN                     host daemon auth token
//	AILERON_SANDBOX_FLIGHTPLAN_UNIT   frozen unit name to launch inside the image
//	AILERON_SANDBOX_FLIGHTPLAN_STORE  host store dir holding the frozen unit
//
// Per repo policy (no skip-when-unsupported) this test is FAIL-FAST: absence of
// Docker, the env, or a reachable daemon is a job-configuration FAILURE, not a
// t.Skip. CI's integration_sandbox job provisions the toolchain, boots a host
// daemon, freezes a unit into the store, and sets the env.
//
// Run with:
//
//	go test -tags=integration_sandbox -run TestFlightPlanImageAuditReachesHost ./cmd/aileron/...
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// TestFlightPlanImageAuditReachesHost boots a real pinned Flight Plan image and
// asserts a record emitted by the in-container launch surfaces on the HOST
// daemon's audit trail (#1759).
func TestFlightPlanImageAuditReachesHost(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		// Fail-fast, not skip: repo policy forbids skip-when-unsupported. The CI
		// job documents `docker` as a prerequisite.
		t.Fatalf("docker not on PATH; install Docker to run this integration test: %v", err)
	}

	image := strings.TrimSpace(os.Getenv("AILERON_SANDBOX_FLIGHTPLAN_IMAGE"))
	if image == "" {
		t.Fatalf("AILERON_SANDBOX_FLIGHTPLAN_IMAGE must name the pinned environment image to boot (required, not skipped)")
	}
	apiURL := strings.TrimSpace(os.Getenv("AILERON_API_URL"))
	token := strings.TrimSpace(os.Getenv("AILERON_TOKEN"))
	if apiURL == "" || token == "" {
		t.Fatalf("AILERON_API_URL and AILERON_TOKEN must point at a live host daemon (required, not skipped)")
	}
	unit := strings.TrimSpace(os.Getenv("AILERON_SANDBOX_FLIGHTPLAN_UNIT"))
	if unit == "" {
		t.Fatalf("AILERON_SANDBOX_FLIGHTPLAN_UNIT must name the frozen unit to launch (required, not skipped)")
	}
	storeDir := strings.TrimSpace(os.Getenv("AILERON_SANDBOX_FLIGHTPLAN_STORE"))
	if storeDir == "" {
		t.Fatalf("AILERON_SANDBOX_FLIGHTPLAN_STORE must point at the host store holding the frozen unit (required, not skipped)")
	}

	// Point the process store seam at the host store the CI job froze the unit
	// into, so the boot mounts the exact verified bytes.
	origStore := skillStoreDir
	skillStoreDir = storeDir
	t.Cleanup(func() { skillStoreDir = origStore })

	// Snapshot the host audit trail before the boot so the post-boot assertion
	// keys on the NEW record rather than any pre-existing one.
	before := hostAuditCount(t, apiURL, token)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Boot the real image through the production runner, which injects the host
	// daemon coordinates. daemonImageEnv resolves them from AILERON_API_URL /
	// AILERON_TOKEN set above (spawn-free), host-rewrites the loopback host to
	// host.docker.internal, and re-appends /v1.
	if _, err := (containerImageRunner{daemonEnv: daemonImageEnv{}}).Run(ctx, runtime.ImageRunSpec{
		Image: image,
		Name:  unit,
	}); err != nil {
		t.Fatalf("image boot failed: %v", err)
	}

	// The in-container launch posts over HTTP to the host daemon; poll the host
	// audit trail until the new record lands or the deadline passes. Observing
	// the HOST store directly is the whole point: a boot that reached only an
	// ephemeral in-container daemon leaves this count unchanged and FAILS.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if hostAuditCount(t, apiURL, token) > before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new host audit record after image boot: the in-container launch did not reach the host daemon (before=%d)", before)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// hostAuditCount queries the live host daemon's GET /v1/audit and returns the
// number of records it reports, so the test can assert a NEW record landed on
// the host store after the image boot.
func hostAuditCount(t *testing.T, apiURL, token string) int {
	t.Helper()
	// Bound the request so a stalled daemon can't hang the fail-fast poll loop:
	// a hung GET would otherwise block the test until the whole go-test timeout
	// rather than failing the reachability assertion promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiURL, "/")+"/audit", nil)
	if err != nil {
		t.Fatalf("build audit list request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("query host audit trail: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("host daemon returned %d for GET /audit: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	// The audit list response wraps the records under an "events" (or "records")
	// array; count whichever is present so the assertion is robust to the exact
	// key. A decode failure is a job-config/daemon-shape failure, not a skip.
	var envelope struct {
		Events  []json.RawMessage `json:"events"`
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode host audit list %q: %v", string(raw), err)
	}
	if len(envelope.Events) > 0 {
		return len(envelope.Events)
	}
	return len(envelope.Records)
}
