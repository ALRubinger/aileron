// Contract tests for the pure-logic probe helpers ported from probes.sh,
// represented one-to-one with the cases in probes_test.sh. Every case is
// host-verifiable and docker-free: the shell stubbed `docker` on PATH and fed
// inputs via env (DOCKER_PS_OUTPUT, DOCKER_EXEC_TEST_RC,
// DOCKER_EXEC_PRINTENV_RC, DOCKER_INSPECT_OUTPUT); here those same inputs are
// passed as ordinary Go parameters. Tests assert the contract (returned
// bool/error and, on the failure path, a faithful diagnostic), never
// implementation internals.
package systestlib_test

import (
	"strings"
	"testing"

	systestlib "github.com/ALRubinger/aileron/test/system/lib"
)

func TestImageRepo(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{
			name: "strips :edge floating tag from a GHCR ref",
			ref:  "ghcr.io/alrubinger/aileron-sandbox-codex:edge",
			want: "ghcr.io/alrubinger/aileron-sandbox-codex",
		},
		{
			name: "strips @sha256 digest from a GHCR ref",
			ref:  "ghcr.io/alrubinger/aileron-sandbox-codex@sha256:abc123",
			want: "ghcr.io/alrubinger/aileron-sandbox-codex",
		},
		{
			name: "keeps a registry host:port with no tag",
			ref:  "localhost:5000/aileron-sandbox-codex",
			want: "localhost:5000/aileron-sandbox-codex",
		},
		{
			name: "strips :edge from a host-less local ref",
			ref:  "aileron/sandbox-agent-claude:edge",
			want: "aileron/sandbox-agent-claude",
		},
		{
			name: "strips :latest from a host-less local ref",
			ref:  "aileron/sandbox-agent-claude:latest",
			want: "aileron/sandbox-agent-claude",
		},
		{
			name: "bare repo with no tag or digest is unchanged",
			ref:  "aileron/sandbox-agent-claude",
			want: "aileron/sandbox-agent-claude",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := systestlib.ImageRepo(tc.ref); got != tc.want {
				t.Errorf("ImageRepo(%q) = %q; want %q", tc.ref, got, tc.want)
			}
		})
	}
}

func TestImageRefMatches(t *testing.T) {
	tests := []struct {
		name             string
		expected, actual string
		want             bool
	}{
		{
			name:     "accepts the same floating tag",
			expected: "ghcr.io/alrubinger/aileron-sandbox-codex:edge",
			actual:   "ghcr.io/alrubinger/aileron-sandbox-codex:edge",
			want:     true,
		},
		{
			// #1233 regression: a floating expected ref must match the digest the
			// launcher actually pulled on a reproducible release.
			name:     "accepts a digest pin on the same repo (release path)",
			expected: "ghcr.io/alrubinger/aileron-sandbox-codex:edge",
			actual:   "ghcr.io/alrubinger/aileron-sandbox-codex@sha256:deadbeef",
			want:     true,
		},
		{
			name:     "accepts a digest pin against a :latest expectation",
			expected: "ghcr.io/alrubinger/aileron-sandbox-codex:latest",
			actual:   "ghcr.io/alrubinger/aileron-sandbox-codex@sha256:deadbeef",
			want:     true,
		},
		{
			name:     "rejects a digest pin on a different agent repo",
			expected: "ghcr.io/alrubinger/aileron-sandbox-codex:edge",
			actual:   "ghcr.io/alrubinger/aileron-sandbox-claude@sha256:deadbeef",
			want:     false,
		},
		{
			name:     "rejects an unexpected tag on the right repo",
			expected: "ghcr.io/alrubinger/aileron-sandbox-codex:edge",
			actual:   "ghcr.io/alrubinger/aileron-sandbox-codex:v0.0.1",
			want:     false,
		},
		{
			name:     "accepts an exact digest override",
			expected: "ghcr.io/alrubinger/aileron-sandbox-codex@sha256:abc",
			actual:   "ghcr.io/alrubinger/aileron-sandbox-codex@sha256:abc",
			want:     true,
		},
		{
			name:     "accepts the local :edge tag on its own repo",
			expected: "aileron/sandbox-agent-claude:edge",
			actual:   "aileron/sandbox-agent-claude:edge",
			want:     true,
		},
		{
			name:     "accepts the local :latest tag on its own repo",
			expected: "aileron/sandbox-agent-claude:latest",
			actual:   "aileron/sandbox-agent-claude:latest",
			want:     true,
		},
		{
			name:     "rejects a different local agent repo",
			expected: "aileron/sandbox-agent-claude:edge",
			actual:   "aileron/sandbox-agent-codex:edge",
			want:     false,
		},
		{
			name:     "rejects an unexpected tag on the local repo",
			expected: "aileron/sandbox-agent-claude:edge",
			actual:   "aileron/sandbox-agent-claude:v0.0.1",
			want:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := systestlib.ImageRefMatches(tc.expected, tc.actual); got != tc.want {
				t.Errorf("ImageRefMatches(%q, %q) = %v; want %v", tc.expected, tc.actual, got, tc.want)
			}
		})
	}
}

func TestDiscoverContainer(t *testing.T) {
	const prefix = "aileron-sbx-"
	tests := []struct {
		name string
		// raw is the newline-delimited block (as DOCKER_PS_OUTPUT would feed it);
		// it is split via the package's whitespace splitter to prove the
		// docker-free arbitration matches the shell.
		raw      string
		wantName string
		wantErr  bool
		// errContains are substrings the diagnostic must carry on the failure path.
		errContains []string
	}{
		{
			name:     "exactly one match returns the name and nil error",
			raw:      "aileron-sbx-codex-123\n",
			wantName: "aileron-sbx-codex-123",
			wantErr:  false,
		},
		{
			name:        "zero matches returns an error, never a silent empty name",
			raw:         "",
			wantErr:     true,
			errContains: []string{"no running container named", prefix},
		},
		{
			name:        "more than one match returns an error rather than guessing",
			raw:         "aileron-sbx-a\naileron-sbx-b\n",
			wantErr:     true,
			errContains: []string{"2 running containers match", prefix},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := systestlib.DiscoverContainer(prefix, systestlib.SplitNames(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DiscoverContainer returned nil error; want non-nil")
				}
				if got != "" {
					t.Errorf("DiscoverContainer returned name %q on the error path; want empty", got)
				}
				for _, sub := range tc.errContains {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q does not contain %q", err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("DiscoverContainer returned error %q; want nil", err.Error())
			}
			if got != tc.wantName {
				t.Errorf("DiscoverContainer = %q; want %q", got, tc.wantName)
			}
		})
	}
}

// TestDiscoverContainerSlice drives DiscoverContainer with a pre-split []string
// (the other input shape the brief calls out) to confirm whitespace-only and
// empty entries are ignored, matching the shell's word-splitting of the
// docker ps block.
func TestDiscoverContainerSlice(t *testing.T) {
	const prefix = "aileron-sbx-"
	got, err := systestlib.DiscoverContainer(prefix, []string{"", "   ", "aileron-sbx-only", "\t"})
	if err != nil {
		t.Fatalf("DiscoverContainer returned error %q; want nil", err.Error())
	}
	if got != "aileron-sbx-only" {
		t.Errorf("DiscoverContainer = %q; want %q", got, "aileron-sbx-only")
	}
}

func TestProbeExitCode(t *testing.T) {
	tests := []struct {
		name             string
		actual, expected int
		wantErr          bool
	}{
		{name: "matching codes return nil", actual: 0, expected: 0, wantErr: false},
		{name: "differing codes return an error", actual: 1, expected: 0, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := systestlib.ProbeExitCode(tc.actual, tc.expected)
			if tc.wantErr {
				if err == nil {
					t.Fatal("ProbeExitCode returned nil error; want non-nil")
				}
				if !strings.Contains(err.Error(), "R8.6") {
					t.Errorf("error %q does not contain %q", err.Error(), "R8.6")
				}
				return
			}
			if err != nil {
				t.Fatalf("ProbeExitCode returned error %q; want nil", err.Error())
			}
		})
	}
}

func TestProbeMCPRuntime(t *testing.T) {
	tests := []struct {
		name             string
		binaryExecutable bool
		envVarsAllSet    bool
		wantErr          bool
		errContains      string
	}{
		{
			name:             "binary present and env all set returns nil",
			binaryExecutable: true,
			envVarsAllSet:    true,
			wantErr:          false,
		},
		{
			// Binary missing gates before env: even with env unset, the diagnostic
			// is the binary one, proving the faithful ordering.
			name:             "binary missing returns error and gates before env",
			binaryExecutable: false,
			envVarsAllSet:    false,
			wantErr:          true,
			errContains:      "aileron-mcp missing or not executable",
		},
		{
			name:             "binary present but a daemon env var unset returns error",
			binaryExecutable: true,
			envVarsAllSet:    false,
			wantErr:          true,
			errContains:      "env var not set",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := systestlib.ProbeMCPRuntime(tc.binaryExecutable, tc.envVarsAllSet)
			if tc.wantErr {
				if err == nil {
					t.Fatal("ProbeMCPRuntime returned nil error; want non-nil")
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProbeMCPRuntime returned error %q; want nil", err.Error())
			}
		})
	}
}

func TestProbeMCPCmdline(t *testing.T) {
	const (
		flag   = "--mcp-config"
		marker = `"aileron"`
	)
	tests := []struct {
		name             string
		binaryExecutable bool
		envVarsAllSet    bool
		cmdline          string
		wantErr          bool
		errContains      string
	}{
		{
			name:             "flag and marker both present (core ok) returns nil",
			binaryExecutable: true,
			envVarsAllSet:    true,
			cmdline:          `claude --mcp-config {"mcpServers":{"aileron":{}}} -p hi `,
			wantErr:          false,
		},
		{
			name:             "flag present but marker absent from payload returns error",
			binaryExecutable: true,
			envVarsAllSet:    true,
			cmdline:          `claude --mcp-config {"mcpServers":{"other":{}}} -p hi `,
			wantErr:          true,
			errContains:      "payload references",
		},
		{
			name:             "no --mcp-config flag at all returns error",
			binaryExecutable: true,
			envVarsAllSet:    true,
			cmdline:          `claude -p hi `,
			wantErr:          true,
			errContains:      "container command carries",
		},
		{
			// Cmdline is correct, but the runtime core fails first (binary
			// missing), proving the core gates before the cmdline check.
			name:             "runtime core failing gates before the cmdline check",
			binaryExecutable: false,
			envVarsAllSet:    true,
			cmdline:          `claude --mcp-config {"mcpServers":{"aileron":{}}} -p hi `,
			wantErr:          true,
			errContains:      "aileron-mcp missing or not executable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := systestlib.ProbeMCPCmdline(tc.binaryExecutable, tc.envVarsAllSet, tc.cmdline, flag, marker)
			if tc.wantErr {
				if err == nil {
					t.Fatal("ProbeMCPCmdline returned nil error; want non-nil")
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProbeMCPCmdline returned error %q; want nil", err.Error())
			}
		})
	}
}
