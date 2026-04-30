package action

import (
	"errors"
	"strings"
	"testing"
)

// goodManifest returns a manifest that passes Validate; tests mutate one
// field at a time to assert specific failures.
func goodManifest() *Manifest {
	return &Manifest{
		Name:    "ship-update",
		Version: "1.0.0",
		Source:  "hub://aileron/ship-update@1.0.0",
		Requires: Requires{
			Connectors: []RequiresConnector{
				{
					Name:         "github://aileron/slack",
					Version:      "1.2.0",
					Hash:         "sha256:abc",
					Capabilities: []string{"chat:write"},
				},
			},
		},
		Match: Match{Intent: "tell team I shipped"},
		Execute: []ExecuteStep{
			{ID: "post", Connector: "github://aileron/slack", Op: "post_message"},
		},
	}
}

func TestValidate_HappyPath(t *testing.T) {
	if err := Validate(goodManifest(), "ok.md"); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestValidate_RejectsBadFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Manifest)
		want    string // substring expected in the error message
	}{
		{"empty name", func(m *Manifest) { m.Name = "" }, "name is required"},
		{"bad name shape", func(m *Manifest) { m.Name = "Has Spaces" }, "must match"},
		{"missing version", func(m *Manifest) { m.Version = "" }, "version is required"},
		{"non-semver version", func(m *Manifest) { m.Version = "1.x" }, "strict SemVer"},
		{"missing source", func(m *Manifest) { m.Source = "" }, "source is required"},
		{"source missing version", func(m *Manifest) { m.Source = "hub://aileron/ship-update" }, "missing @<version>"},
		{"source bad scheme", func(m *Manifest) { m.Source = "ftp://x/y@1.0.0" }, "scheme"},
		{"no connectors", func(m *Manifest) { m.Requires.Connectors = nil }, "[[requires.connectors]]"},
		{"connector empty name", func(m *Manifest) { m.Requires.Connectors[0].Name = "" }, "name is required"},
		{"connector bad scheme", func(m *Manifest) { m.Requires.Connectors[0].Name = "ftp://acme/slack" }, "scheme"},
		{"connector missing version", func(m *Manifest) { m.Requires.Connectors[0].Version = "" }, "version is required"},
		{"connector bad version", func(m *Manifest) { m.Requires.Connectors[0].Version = "x" }, "strict SemVer"},
		{"connector missing hash", func(m *Manifest) { m.Requires.Connectors[0].Hash = "" }, "hash is required"},
		{"connector bad hash prefix", func(m *Manifest) { m.Requires.Connectors[0].Hash = "md5:abc" }, "sha256:"},
		{"connector empty caps", func(m *Manifest) { m.Requires.Connectors[0].Capabilities = nil }, "capabilities is required"},
		{"connector blank cap", func(m *Manifest) { m.Requires.Connectors[0].Capabilities = []string{"   "} }, "is empty"},
		{"missing intent", func(m *Manifest) { m.Match.Intent = "" }, "intent is required"},
		{"no execute steps", func(m *Manifest) { m.Execute = nil }, "[[execute]]"},
		{"execute missing id", func(m *Manifest) { m.Execute[0].ID = "" }, "id is required"},
		{"execute duplicate id", func(m *Manifest) {
			m.Execute = append(m.Execute, ExecuteStep{ID: "post", Connector: "github://aileron/slack", Op: "x"})
		}, "duplicated"},
		{"execute missing connector", func(m *Manifest) { m.Execute[0].Connector = "" }, "connector is required"},
		{"execute undeclared connector", func(m *Manifest) { m.Execute[0].Connector = "github://other/foo" }, "not declared"},
		{"execute missing op", func(m *Manifest) { m.Execute[0].Op = "" }, "op is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := goodManifest()
			tc.mutate(m)
			err := Validate(m, "x.md")
			if err == nil {
				t.Fatalf("Validate() succeeded; want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q; want substring %q", err.Error(), tc.want)
			}
			var aerr *Error
			if !errors.As(err, &aerr) || aerr.Class != ClassValidationError {
				t.Errorf("expected *Error/ClassValidationError, got %v", err)
			}
		})
	}
}

func TestValidate_AcceptsAllRecognizedSchemes(t *testing.T) {
	for _, scheme := range []string{"github", "gitlab", "hub"} {
		t.Run(scheme, func(t *testing.T) {
			m := goodManifest()
			m.Source = scheme + "://aileron/ship-update@1.0.0"
			m.Requires.Connectors[0].Name = scheme + "://aileron/slack"
			m.Execute[0].Connector = m.Requires.Connectors[0].Name
			if err := Validate(m, "x.md"); err != nil {
				t.Errorf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidate_NilManifest(t *testing.T) {
	if err := Validate(nil, "x.md"); err == nil {
		t.Fatal("Validate(nil) succeeded; want error")
	}
}

// Inputs validation: per ADR-0003 the [[inputs]] block declares the
// LLM-facing parameter schema. These tests pin the contract.

func TestValidate_AcceptsAllInputTypes(t *testing.T) {
	for _, typ := range []string{"string", "integer", "number", "boolean"} {
		t.Run(typ, func(t *testing.T) {
			m := goodManifest()
			m.Inputs = []Input{{Name: "x", Type: typ, Description: "the x"}}
			if err := Validate(m, "x.md"); err != nil {
				t.Errorf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidate_AcceptsRequiredOverride(t *testing.T) {
	m := goodManifest()
	f := false
	m.Inputs = []Input{
		{Name: "must", Type: "string", Description: "required by default"},
		{Name: "may", Type: "string", Required: &f, Description: "optional"},
	}
	if err := Validate(m, "x.md"); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !m.Inputs[0].IsRequired() {
		t.Error("first input IsRequired() = false, want true (default)")
	}
	if m.Inputs[1].IsRequired() {
		t.Error("second input IsRequired() = true, want false")
	}
}

func TestValidate_RejectsBadInputs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"empty input name", func(m *Manifest) {
			m.Inputs = []Input{{Name: "", Type: "string", Description: "x"}}
		}, "inputs[0].name is required"},
		{"bad input name", func(m *Manifest) {
			m.Inputs = []Input{{Name: "Bad-Name", Type: "string", Description: "x"}}
		}, "must match"},
		{"duplicate input names", func(m *Manifest) {
			m.Inputs = []Input{
				{Name: "x", Type: "string", Description: "first"},
				{Name: "x", Type: "string", Description: "second"},
			}
		}, "duplicated"},
		{"missing input type", func(m *Manifest) {
			m.Inputs = []Input{{Name: "x", Type: "", Description: "x"}}
		}, "type is required"},
		{"unknown input type", func(m *Manifest) {
			m.Inputs = []Input{{Name: "x", Type: "object", Description: "x"}}
		}, "must be one of"},
		{"missing input description", func(m *Manifest) {
			m.Inputs = []Input{{Name: "x", Type: "string", Description: ""}}
		}, "description is required"},
		{"undeclared args ref", func(m *Manifest) {
			m.Execute[0].Inputs = map[string]any{"channel": "${args.missing}"}
		}, "no [[inputs]] block declares"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := goodManifest()
			tc.mutate(m)
			err := Validate(m, "x.md")
			if err == nil {
				t.Fatalf("Validate() succeeded; want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q; want substring %q", err.Error(), tc.want)
			}
			var aerr *Error
			if !errors.As(err, &aerr) || aerr.Class != ClassValidationError {
				t.Errorf("expected *Error/ClassValidationError, got %v", err)
			}
		})
	}
}

func TestValidate_AcceptsArgsRefAcrossNestedInputs(t *testing.T) {
	m := goodManifest()
	m.Inputs = []Input{{Name: "channel", Type: "string", Description: "..."}}
	m.Execute[0].Inputs = map[string]any{
		"target": map[string]any{
			"primary": "${args.channel}",
		},
		"alts": []any{"${args.channel}", "static"},
	}
	if err := Validate(m, "x.md"); err != nil {
		t.Errorf("Validate() error = %v", err)
	}
}
