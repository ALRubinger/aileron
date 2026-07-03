package freeze

import (
	"strings"
	"testing"
)

// TestSealStepTrust_SealsContractedToolSteps proves the selection contract
// over the parsed fixture: each tool step with a declared trust contract gets
// an entry keyed by its id carrying exactly its declared hosts, and a tool
// step with no contract is omitted.
func TestSealStepTrust_SealsContractedToolSteps(t *testing.T) {
	m := parseM(t, []byte(toolStepsMD))
	got, err := sealStepTrust(m.Aileron.Steps)
	if err != nil {
		t.Fatalf("sealStepTrust: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("stepTrust = %+v, want exactly the two contracted steps", got)
	}
	if strings.Join(got["fetch"].Hosts, ",") != "s3.amazonaws.com,s3.amazonaws.com:443" {
		t.Errorf("fetch reach = %v", got["fetch"].Hosts)
	}
	if strings.Join(got["file"].Hosts, ",") != "api.github.com" {
		t.Errorf("file reach = %v", got["file"].Hosts)
	}
	if _, ok := got["version"]; ok {
		t.Error("a tool step with no trust contract must be omitted from stepTrust")
	}
}

// TestSealStepTrust_NonToolStepsIgnored proves only tool steps participate:
// an action-call carrying a trustContract-shaped key is not sealed (its reach
// lives in requires.actions, not the step-keyed section).
func TestSealStepTrust_NonToolStepsIgnored(t *testing.T) {
	steps := []any{
		map[string]any{"id": "call", "kind": "action-call", "actionRef": "aileron:x.y",
			"trustContract": map[string]any{"hosts": []any{"api.example.com"}}},
		map[string]any{"id": "reshape", "kind": "transform", "outputs": []any{"out"}},
	}
	got, err := sealStepTrust(steps)
	if err != nil {
		t.Fatalf("sealStepTrust: %v", err)
	}
	if got != nil {
		t.Errorf("non-tool steps must seal nothing, got %+v", got)
	}
}

// TestSealStepTrust_EmptyResultIsNil proves a plan with no contracted tool
// steps yields nil, so the marshaled lock omits the section entirely
// (byte-stability with the pre-stepTrust format).
func TestSealStepTrust_EmptyResultIsNil(t *testing.T) {
	got, err := sealStepTrust(nil)
	if err != nil {
		t.Fatalf("sealStepTrust(nil): %v", err)
	}
	if got != nil {
		t.Errorf("no steps must seal nil, got %+v", got)
	}
	lf, err := MarshalLockfile(Lockfile{StepTrust: got})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(lf), "stepTrust") {
		t.Errorf("an empty stepTrust must be omitted from the lock, got:\n%s", lf)
	}
}

// TestSealStepTrust_MalformedRefused enumerates the fail-closed guards: a
// malformed declared reach is a hard error, never a partial seal. These
// shapes are schema-rejected on any parsed manifest; sealStepTrust is the
// freeze-time defensive backstop for direct constructs.
func TestSealStepTrust_MalformedRefused(t *testing.T) {
	toolStep := func(overrides map[string]any) map[string]any {
		s := map[string]any{"id": "fetch", "kind": "tool", "command": []any{"aws"}}
		for k, v := range overrides {
			s[k] = v
		}
		return s
	}
	cases := map[string][]any{
		"non-mapping step": {"scalar"},
		"trustContract not a mapping": {
			toolStep(map[string]any{"trustContract": "scalar"}),
		},
		"trustContract with no hosts": {
			toolStep(map[string]any{"trustContract": map[string]any{"effect": "read"}}),
		},
		"empty hosts": {
			toolStep(map[string]any{"trustContract": map[string]any{"hosts": []any{}}}),
		},
		"non-array hosts": {
			toolStep(map[string]any{"trustContract": map[string]any{"hosts": "api.example.com"}}),
		},
		"non-string host": {
			toolStep(map[string]any{"trustContract": map[string]any{"hosts": []any{42}}}),
		},
		"empty host entry": {
			toolStep(map[string]any{"trustContract": map[string]any{"hosts": []any{" "}}}),
		},
		"contracted step with no id": {
			map[string]any{"kind": "tool", "command": []any{"aws"},
				"trustContract": map[string]any{"hosts": []any{"api.example.com"}}},
		},
		"duplicate contracted step id": {
			toolStep(map[string]any{"trustContract": map[string]any{"hosts": []any{"a.example.com"}}}),
			toolStep(map[string]any{"trustContract": map[string]any{"hosts": []any{"b.example.com"}}}),
		},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := sealStepTrust(steps); err == nil {
				t.Errorf("%s must be refused", name)
			}
		})
	}
}

// TestSealStepTrust_TrimsHosts proves declared hosts are trimmed exactly like
// the rest of freeze's string extraction, so a stray-whitespace host never
// seals a reach the runtime cannot match.
func TestSealStepTrust_TrimsHosts(t *testing.T) {
	steps := []any{
		map[string]any{"id": "fetch", "kind": "tool", "command": []any{"aws"},
			"trustContract": map[string]any{"hosts": []any{"  api.example.com "}}},
	}
	got, err := sealStepTrust(steps)
	if err != nil {
		t.Fatalf("sealStepTrust: %v", err)
	}
	if strings.Join(got["fetch"].Hosts, ",") != "api.example.com" {
		t.Errorf("hosts must be trimmed, got %v", got["fetch"].Hosts)
	}
}
