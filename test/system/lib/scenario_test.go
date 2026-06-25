// Contract tests for the pure scenario decision predicates (R8.1/R8.3/R8.4/R8.5
// decision cores plus the config-file MCP tail). Each test feeds the
// docker-derived facts as parameters (mirroring probes_test.go's parameter-fed
// pattern) so the decisions are verified with no docker. Tests assert the
// returned error contract and, on the failure path, a faithful diagnostic.
package systestlib_test

import (
	"strings"
	"testing"

	systestlib "github.com/ALRubinger/aileron/test/system/lib"
)

func TestProbeImageRunning(t *testing.T) {
	const expected = "ghcr.io/alrubinger/aileron-sandbox-codex:edge"
	tests := []struct {
		name                         string
		actualImage, running, status string
		wantErr                      bool
		errContains                  []string
	}{
		{
			name:        "matching image, running and status ok returns nil",
			actualImage: "ghcr.io/alrubinger/aileron-sandbox-codex:edge",
			running:     "true",
			status:      "running",
			wantErr:     false,
		},
		{
			name:        "digest pin on the same repo is accepted",
			actualImage: "ghcr.io/alrubinger/aileron-sandbox-codex@sha256:deadbeef",
			running:     "true",
			status:      "running",
			wantErr:     false,
		},
		{
			name:        "wrong repo fails on the image check",
			actualImage: "ghcr.io/alrubinger/aileron-sandbox-claude:edge",
			running:     "true",
			status:      "running",
			wantErr:     true,
			errContains: []string{"R8.1 container image"},
		},
		{
			name:        "right image but not running fails on .State.Running",
			actualImage: expected,
			running:     "false",
			status:      "exited",
			wantErr:     true,
			errContains: []string{"R8.1 container .State.Running"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := systestlib.ProbeImageRunning(expected, tc.actualImage, tc.running, tc.status)
			assertErr(t, err, tc.wantErr, tc.errContains)
		})
	}
}

func TestProbeMCPConfigContains(t *testing.T) {
	const (
		path   = "/home/agent/.codex/config.toml"
		marker = "[mcp_servers.aileron]"
	)
	if err := systestlib.ProbeMCPConfigContains("foo\n[mcp_servers.aileron]\nbar", path, marker); err != nil {
		t.Errorf("present marker returned error %v; want nil", err)
	}
	err := systestlib.ProbeMCPConfigContains("[mcp_servers.other]", path, marker)
	if err == nil {
		t.Fatal("absent marker returned nil; want error")
	}
	if !strings.Contains(err.Error(), marker) {
		t.Errorf("error %q does not name the marker %q", err.Error(), marker)
	}
}

func TestProbeCredentials(t *testing.T) {
	const (
		authPath = "/home/agent/.codex/auth.json"
		authDir  = "/home/agent/.codex"
	)
	okMounts := "bind:/home/agent/workspace\nbind:/home/agent/.codex\nvolume:/data"
	tests := []struct {
		name           string
		authFileExists bool
		mode, mounts   string
		isWindowsHost  bool
		wantErr        bool
		errContains    []string
	}{
		{
			name:           "exists, 0600, parent bind-mounted returns nil",
			authFileExists: true,
			mode:           "600",
			mounts:         okMounts,
			wantErr:        false,
		},
		{
			name:           "missing auth file fails",
			authFileExists: false,
			mode:           "600",
			mounts:         okMounts,
			wantErr:        true,
			errContains:    []string{"auth file"},
		},
		{
			name:           "wrong mode fails on the 0600 check",
			authFileExists: true,
			mode:           "644",
			mounts:         okMounts,
			wantErr:        true,
			errContains:    []string{"mode is 0600"},
		},
		{
			name:           "no bind mount for the auth dir fails",
			authFileExists: true,
			mode:           "600",
			mounts:         "bind:/home/agent/workspace\nvolume:/data",
			wantErr:        true,
			errContains:    []string{"no bind mount"},
		},
		{
			name:           "a bind mount of a parent-of-parent dir does not satisfy the check",
			authFileExists: true,
			mode:           "600",
			mounts:         "bind:/home/agent",
			wantErr:        true,
			errContains:    []string{"no bind mount"},
		},
		{
			// Docker Desktop on Windows presents the bind-mounted auth file as
			// 0777 because it does not project the host's Unix mode bits. The
			// 0600 check is skipped, so the otherwise-passing facts still return
			// nil rather than failing on the host-uncontrollable mode.
			name:           "windows host skips the 0600 mode check",
			authFileExists: true,
			mode:           "777",
			mounts:         okMounts,
			isWindowsHost:  true,
			wantErr:        false,
		},
		{
			// The mode skip is Windows-only: the same 0777 mode still fails on a
			// non-Windows host where the bit is faithfully carried.
			name:           "non-windows host still enforces the 0600 mode check",
			authFileExists: true,
			mode:           "777",
			mounts:         okMounts,
			isWindowsHost:  false,
			wantErr:        true,
			errContains:    []string{"mode is 0600"},
		},
		{
			// Mode is skipped on Windows, but file presence and the parent-dir
			// bind mount are still enforced on every host.
			name:           "windows host still requires the parent bind mount",
			authFileExists: true,
			mode:           "777",
			mounts:         "bind:/home/agent/workspace\nvolume:/data",
			isWindowsHost:  true,
			wantErr:        true,
			errContains:    []string{"no bind mount"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := systestlib.ProbeCredentials(tc.authFileExists, tc.mode, authPath, authDir, tc.mounts, tc.isWindowsHost)
			assertErr(t, err, tc.wantErr, tc.errContains)
		})
	}
}

func TestProbeDaemonReachable(t *testing.T) {
	const goodURL = "http://host.docker.internal:8080"
	tests := []struct {
		name        string
		url         string
		isLinux     bool
		extraHosts  string
		wantErr     bool
		errContains []string
	}{
		{
			name:    "non-linux host needs only the loopback rewrite",
			url:     goodURL,
			isLinux: false,
			wantErr: false,
		},
		{
			name:       "linux host with host-gateway present returns nil",
			url:        goodURL,
			isLinux:    true,
			extraHosts: "host.docker.internal:host-gateway\n",
			wantErr:    false,
		},
		{
			name:        "url not rewritten to host.docker.internal fails",
			url:         "http://127.0.0.1:8080",
			isLinux:     false,
			wantErr:     true,
			errContains: []string{"host.docker.internal"},
		},
		{
			name:        "linux host missing the host-gateway extra host fails",
			url:         goodURL,
			isLinux:     true,
			extraHosts:  "",
			wantErr:     true,
			errContains: []string{"host-gateway"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := systestlib.ProbeDaemonReachable(tc.url, tc.isLinux, tc.extraHosts)
			assertErr(t, err, tc.wantErr, tc.errContains)
		})
	}
}

func TestProbeTeardown(t *testing.T) {
	if err := systestlib.ProbeTeardown("aileron-sbx-", ""); err != nil {
		t.Errorf("empty surviving block returned error %v; want nil", err)
	}
	if err := systestlib.ProbeTeardown("aileron-sbx-", "   \n  \n"); err != nil {
		t.Errorf("whitespace-only surviving block returned error %v; want nil", err)
	}
	err := systestlib.ProbeTeardown("aileron-sbx-", "aileron-sbx-codex-123\n")
	if err == nil {
		t.Fatal("a surviving container returned nil; want the R8.5 error")
	}
	for _, sub := range []string{"R8.5", "survived teardown", "aileron-sbx-codex-123"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not contain %q", err.Error(), sub)
		}
	}
}
