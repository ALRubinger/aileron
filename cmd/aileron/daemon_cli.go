package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/daemon/spawn"
)

const daemonUsage = `usage:
  aileron daemon start    Start the local Aileron daemon (idempotent — prints URL if already running)
  aileron daemon stop     Stop the running daemon (SIGTERM); also reachable as 'aileron stop'
  aileron daemon status   Show whether the daemon is running and its vault state`

// runDaemon dispatches the `aileron daemon <subcommand>` family.
// `start` / `stop` / `status` use the discovery + spawn primitives
// directly (not the bindingDoRequest path) so they can manage the
// daemon's lifecycle without auto-spawning at the wrong moment.
func runDaemon(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, daemonUsage)
		return 1
	}
	switch args[0] {
	case "start":
		return runDaemonStart(args[1:], stdout, stderr)
	case "stop":
		return runDaemonStop(args[1:], stdout, stderr)
	case "status":
		return runDaemonStatus(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown daemon command: %q\n", args[0])
		fmt.Fprintln(stderr, daemonUsage)
		return 1
	}
}

// runDaemonStart spawns the daemon in the background. Idempotent —
// if a reachable daemon is already running, prints its URL and exits 0.
func runDaemonStart(_ []string, stdout, stderr io.Writer) int {
	stateDir, err := defaultStateDir()
	if err != nil {
		fmt.Fprintf(stderr, "aileron: %v\n", err)
		return 1
	}
	binary, err := daemonBinaryPath()
	if err != nil {
		fmt.Fprintf(stderr, "aileron: locate daemon binary: %v\n", err)
		fmt.Fprintln(stderr, "Hint: run 'task build:server' to build it next to the aileron binary.")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url, err := spawnResolveFn(ctx, spawn.Options{
		StateDir: stateDir,
		Binary:   binary,
	})
	if err != nil {
		fmt.Fprintf(stderr, "aileron: starting daemon: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Aileron daemon running at %s\n", url)
	return 0
}

// runDaemonStop sends SIGTERM to the daemon (PID from daemon.json)
// and waits for daemon.json to be removed, signalling clean shutdown.
// Idempotent — if the daemon isn't running, exits 0 with a short
// note rather than failing.
func runDaemonStop(_ []string, stdout, stderr io.Writer) int {
	stateDir, err := defaultStateDir()
	if err != nil {
		fmt.Fprintf(stderr, "aileron: %v\n", err)
		return 1
	}
	info, err := discovery.Read(stateDir)
	if err != nil {
		if errors.Is(err, discovery.ErrNotRunning) {
			fmt.Fprintln(stdout, "Aileron daemon is not running.")
			return 0
		}
		fmt.Fprintf(stderr, "aileron: read daemon.json: %v\n", err)
		return 1
	}

	notRunning, err := signalDaemonStop(info.PID)
	if notRunning {
		// PID is not alive: stale daemon.json from a prior crash.
		// Clean it up for the operator and exit 0; the daemon is
		// effectively stopped already.
		_ = discovery.Remove(stateDir)
		fmt.Fprintln(stdout, "Aileron daemon was not running (stale daemon.json removed).")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "aileron: signal daemon (pid %d): %v\n", info.PID, err)
		return 1
	}

	// Wait for the daemon to clean up its discovery files. The daemon
	// removes them in its shutdown defer (see internal/server/main.go).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := discovery.Read(stateDir); errors.Is(err, discovery.ErrNotRunning) {
			fmt.Fprintf(stdout, "Aileron daemon stopped (pid %d).\n", info.PID)
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "aileron: daemon did not exit within 5s; daemon.json still present\n")
	return 1
}

// runDaemonStatus prints a short summary of the running daemon and
// its vault state. Probes /v1/vault/status when daemon.json is
// present; "not running" when it isn't.
func runDaemonStatus(_ []string, stdout, stderr io.Writer) int {
	stateDir, err := defaultStateDir()
	if err != nil {
		fmt.Fprintf(stderr, "aileron: %v\n", err)
		return 1
	}
	info, err := discovery.Read(stateDir)
	if err != nil {
		if errors.Is(err, discovery.ErrNotRunning) {
			fmt.Fprintln(stdout, "Aileron daemon is not running.")
			fmt.Fprintln(stdout, "Hint: any 'aileron <command>' will auto-spawn it; or run 'aileron daemon start'.")
			return 0
		}
		fmt.Fprintf(stderr, "aileron: read daemon.json: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout, "Aileron daemon is running.")
	fmt.Fprintf(stdout, "  URL:        %s\n", info.URL)
	fmt.Fprintf(stdout, "  PID:        %d\n", info.PID)
	fmt.Fprintf(stdout, "  Version:    %s\n", info.Version)
	fmt.Fprintf(stdout, "  Started:    %s\n", info.StartedAt.Local().Format(time.RFC3339))
	fmt.Fprintf(stdout, "  State dir:  %s\n", stateDir)

	if locked, ok := probeLocalVaultLocked(info.URL, info.Token); ok {
		state := "unlocked"
		if locked {
			state = "locked"
		}
		fmt.Fprintf(stdout, "  Vault:      %s\n", state)
	}
	return 0
}

// probeLocalVaultLocked asks the daemon whether the local vault is
// locked. Returns (locked, true) on a successful probe; (_, false)
// when the daemon doesn't respond or doesn't expose the endpoint
// (e.g. cloud-shaped deployment without a local vault).
func probeLocalVaultLocked(baseURL, token string) (bool, bool) {
	client := &http.Client{Timeout: 1 * time.Second}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/vault/status", nil)
	if err != nil {
		return false, false
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}
	var body struct {
		Locked bool `json:"locked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, false
	}
	return body.Locked, true
}
