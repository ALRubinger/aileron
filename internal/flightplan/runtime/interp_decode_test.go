package runtime

import (
	"strings"
	"testing"
)

// TestDecode_ToolCommandMalformedTokenRefused proves decode rejects a tool
// command with a malformed `{{ inputs.<name> }}` token (defense-in-depth over
// the launch load path, #1958).
func TestDecode_ToolCommandMalformedTokenRefused(t *testing.T) {
	m := rawManifest(
		[]any{litInput("region", "string", "us-east-1")},
		nil,
		[]any{step(map[string]any{
			"id": "fetch", "kind": "tool",
			"command": []any{"aws", "--region={{ inputs.region }"},
			"outputs": []any{"listing"},
		})},
	)
	_, err := Decode(m)
	if err == nil {
		t.Fatal("a malformed command token must be refused at decode")
	}
	if !strings.Contains(err.Error(), "flightplan decode") {
		t.Errorf("want a DecodeError, got: %v", err)
	}
}

// TestDecode_ToolCommandUndeclaredInputRefused proves decode rejects a command
// token referencing an input not declared in the plan, mirroring the BindInput
// undeclared-input check.
func TestDecode_ToolCommandUndeclaredInputRefused(t *testing.T) {
	m := rawManifest(
		[]any{litInput("region", "string", "us-east-1")},
		nil,
		[]any{step(map[string]any{
			"id": "fetch", "kind": "tool",
			"command": []any{"aws", "--zone={{ inputs.zone }}"},
			"outputs": []any{"listing"},
		})},
	)
	_, err := Decode(m)
	if err == nil || !strings.Contains(err.Error(), "undeclared input") {
		t.Fatalf("a command token naming an undeclared input must be refused, got: %v", err)
	}
}

// TestDecode_ToolCommandDeclaredInputClean proves a valid command token
// referencing a declared input decodes clean. Decode does NOT re-check the
// constraint presence (that guard is freeze-only per the plan), so an
// unconstrained input here still decodes.
func TestDecode_ToolCommandDeclaredInputClean(t *testing.T) {
	m := rawManifest(
		[]any{litInput("region", "string", "us-east-1")},
		nil,
		[]any{step(map[string]any{
			"id": "fetch", "kind": "tool",
			"command": []any{"aws", "s3", "ls", "--region={{ inputs.region }}"},
			"outputs": []any{"listing"},
		})},
	)
	p, err := Decode(m)
	if err != nil {
		t.Fatalf("a valid command token referencing a declared input must decode clean: %v", err)
	}
	var fetch *Step
	for i := range p.Steps {
		if p.Steps[i].ID == "fetch" {
			fetch = &p.Steps[i]
		}
	}
	if fetch == nil {
		t.Fatal("fetch step must decode")
	}
	// The TEMPLATE argv is carried verbatim onto the typed Step (instantiation
	// is a launch-time step, never a decode rewrite).
	if strings.Join(fetch.Command, " ") != "aws s3 ls --region={{ inputs.region }}" {
		t.Errorf("decoded command = %v, want the TEMPLATE argv verbatim", fetch.Command)
	}
}

// toolTrustContract builds a minimal valid tool-step trustContract raw map
// whose hosts are the given entries (which may be templates), so a decode test
// exercises the host-token grammar/declared-input checks (#1959).
func toolTrustContract(hosts ...string) map[string]any {
	hs := make([]any, len(hosts))
	for i, h := range hosts {
		hs[i] = h
	}
	return map[string]any{
		"credential":  map[string]any{"kind": "none"},
		"hosts":       hs,
		"effect":      "read",
		"idempotency": map[string]any{"safeToRetry": true},
		"audit":       map[string]any{"fields": []any{"result"}},
	}
}

// TestDecode_ToolHostMalformedTokenRefused proves decode rejects a tool
// trustContract host with a malformed `{{ inputs.<name> }}` token
// (defense-in-depth over the launch load path, #1959).
func TestDecode_ToolHostMalformedTokenRefused(t *testing.T) {
	m := rawManifest(
		[]any{litInput("aws_region", "string", "us-east-1")},
		nil,
		[]any{step(map[string]any{
			"id": "fetch", "kind": "tool",
			"command":       []any{"aws", "athena", "list-databases"},
			"outputs":       []any{"listing"},
			"trustContract": toolTrustContract("athena.{{ inputs.aws_region }.amazonaws.com"),
		})},
	)
	_, err := Decode(m)
	if err == nil {
		t.Fatal("a malformed host token must be refused at decode")
	}
	if !strings.Contains(err.Error(), "flightplan decode") {
		t.Errorf("want a DecodeError, got: %v", err)
	}
}

// TestDecode_ToolHostUndeclaredInputRefused proves decode rejects a host token
// referencing an input not declared in the plan, mirroring the command check.
func TestDecode_ToolHostUndeclaredInputRefused(t *testing.T) {
	m := rawManifest(
		[]any{litInput("aws_region", "string", "us-east-1")},
		nil,
		[]any{step(map[string]any{
			"id": "fetch", "kind": "tool",
			"command":       []any{"aws", "athena", "list-databases"},
			"outputs":       []any{"listing"},
			"trustContract": toolTrustContract("athena.{{ inputs.zone }}.amazonaws.com"),
		})},
	)
	_, err := Decode(m)
	if err == nil || !strings.Contains(err.Error(), "undeclared input") {
		t.Fatalf("a host token naming an undeclared input must be refused, got: %v", err)
	}
}

// TestDecode_ToolHostDeclaredInputClean proves a valid host token referencing a
// declared input decodes clean and the TEMPLATE host is carried verbatim onto
// the typed Step (instantiation is a launch-time step, never a decode rewrite).
// Decode does NOT re-check constraint presence (freeze-only per the plan).
func TestDecode_ToolHostDeclaredInputClean(t *testing.T) {
	m := rawManifest(
		[]any{litInput("aws_region", "string", "us-east-1")},
		nil,
		[]any{step(map[string]any{
			"id": "fetch", "kind": "tool",
			"command":       []any{"aws", "athena", "list-databases"},
			"outputs":       []any{"listing"},
			"trustContract": toolTrustContract("athena.{{ inputs.aws_region }}.amazonaws.com"),
		})},
	)
	p, err := Decode(m)
	if err != nil {
		t.Fatalf("a valid host token referencing a declared input must decode clean: %v", err)
	}
	var fetch *Step
	for i := range p.Steps {
		if p.Steps[i].ID == "fetch" {
			fetch = &p.Steps[i]
		}
	}
	if fetch == nil || fetch.TrustContract == nil {
		t.Fatal("fetch step and its trust contract must decode")
	}
	if strings.Join(fetch.TrustContract.Hosts, ",") != "athena.{{ inputs.aws_region }}.amazonaws.com" {
		t.Errorf("decoded hosts = %v, want the TEMPLATE host verbatim", fetch.TrustContract.Hosts)
	}
}
