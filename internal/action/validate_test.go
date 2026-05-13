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

// TestValidate_AcceptsInputDisplayMetadata pins the contract that
// optional `label` and `multiline` fields on [[inputs]] entries are
// accepted alongside the LLM-facing fields. Both apply only to the
// approval surface (per ADR-0003 amendment); the LLM-visible JSON
// Schema derived from inputs is unaffected.
func TestValidate_AcceptsInputDisplayMetadata(t *testing.T) {
	cases := []struct {
		name  string
		input Input
	}{
		{"label only", Input{Name: "to", Type: "string", Description: "Recipients", Label: "To"}},
		{"multiline only", Input{Name: "body", Type: "string", Description: "Body", Multiline: true}},
		{"label and multiline", Input{Name: "body", Type: "string", Description: "Body", Label: "Body", Multiline: true}},
		{"both unset (default)", Input{Name: "to", Type: "string", Description: "Recipients"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := goodManifest()
			m.Inputs = []Input{tc.input}
			if err := Validate(m, "x.md"); err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

// TestValidate_RejectsBadInputDisplayMetadata pins the rejection
// rules: blank-but-non-empty label, and multiline on a non-string
// input. Both are footguns the approval surface cannot render
// meaningfully, so the parser refuses them.
func TestValidate_RejectsBadInputDisplayMetadata(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"whitespace label", func(m *Manifest) {
			m.Inputs = []Input{{Name: "to", Type: "string", Description: "Recipients", Label: "   "}}
		}, "label is whitespace-only"},
		{"multiline on integer", func(m *Manifest) {
			m.Inputs = []Input{{Name: "n", Type: "integer", Description: "count", Multiline: true}}
		}, "multiline=true is only allowed when type=\"string\""},
		{"multiline on boolean", func(m *Manifest) {
			m.Inputs = []Input{{Name: "b", Type: "boolean", Description: "flag", Multiline: true}}
		}, "multiline=true is only allowed when type=\"string\""},
		{"multiline on number", func(m *Manifest) {
			m.Inputs = []Input{{Name: "r", Type: "number", Description: "ratio", Multiline: true}}
		}, "multiline=true is only allowed when type=\"string\""},
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

// previewManifest returns a manifest declaring a valid
// [approval.preview] directive plus the matching [[inputs]] block the
// preview's args reference. Tests mutate one field at a time to assert
// specific validation failures per ADR-0016.
func previewManifest() *Manifest {
	m := goodManifest()
	m.Inputs = []Input{{Name: "draft_id", Type: "string", Description: "Draft id"}}
	m.Approval = &ApprovalPolicy{
		Required: true,
		Preview: &PreviewPolicy{
			Op:   "get_draft",
			Args: map[string]any{"id": "${args.draft_id}"},
			Render: map[string]string{
				"To":      "message.payload.headers.To",
				"Subject": "message.payload.headers.Subject",
			},
		},
	}
	return m
}

func TestValidate_PreviewHappyPath(t *testing.T) {
	if err := Validate(previewManifest(), "preview.md"); err != nil {
		t.Fatalf("Validate(previewManifest) error = %v, want nil", err)
	}
}

func TestValidate_PreviewRejectsBadFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"empty op", func(m *Manifest) { m.Approval.Preview.Op = "" }, "[approval.preview].op is required"},
		{"whitespace op", func(m *Manifest) { m.Approval.Preview.Op = "   " }, "[approval.preview].op is required"},
		{"empty render", func(m *Manifest) { m.Approval.Preview.Render = nil }, "[approval.preview].render must declare at least one field"},
		{"nil render", func(m *Manifest) { m.Approval.Preview.Render = map[string]string{} }, "[approval.preview].render must declare at least one field"},
		{"empty render path", func(m *Manifest) {
			m.Approval.Preview.Render = map[string]string{"To": ""}
		}, "path is empty"},
		{"undeclared args ref", func(m *Manifest) {
			m.Approval.Preview.Args = map[string]any{"id": "${args.unknown}"}
		}, "no [[inputs]] block declares"},
		{"multiline references missing render key", func(m *Manifest) {
			m.Approval.Preview.Multiline = []string{"Body"}
		}, "multiline references \"Body\" but no matching key exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := previewManifest()
			tc.mutate(m)
			err := Validate(m, "preview.md")
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

// TestValidatePreviewBundle_RejectsNonIdempotentPreviewOp covers
// ADR-0016 rule 2: the preview op must be declared `idempotent = true`
// in at least one bundled action manifest. Without this rule a preview
// could trigger side-effects on the upstream service, defeating the
// "safe to invoke before approval" property.
func TestValidatePreviewBundle_RejectsNonIdempotentPreviewOp(t *testing.T) {
	gated := previewManifest()
	gated.Name = "send-draft"
	bundle := map[string]LoadedAction{
		"send-draft": {Manifest: gated, Path: "send-draft.md"},
	}
	errs := validatePreviewBundle(bundle)
	if len(errs) == 0 {
		t.Fatal("validatePreviewBundle() returned no errors; want one for missing idempotent declaration of preview op")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "not declared `idempotent = true`") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("error messages = %v; want one mentioning idempotent", errs)
	}
}

// TestValidatePreviewBundle_AcceptsIdempotentSiblingDeclaration covers
// the happy path for rule 2: a sibling action manifest that targets
// the same connector and declares the preview op as
// `idempotent = true` satisfies the bundle check.
func TestValidatePreviewBundle_AcceptsIdempotentSiblingDeclaration(t *testing.T) {
	idempotent := true
	gated := previewManifest()
	gated.Name = "send-draft"
	sibling := &Manifest{
		Name:    "get-draft",
		Version: "1.0.0",
		Source:  "hub://aileron/get-draft@1.0.0",
		Requires: Requires{Connectors: []RequiresConnector{{
			Name:         "github://aileron/slack",
			Version:      "1.0.0",
			Hash:         "sha256:abc",
			Capabilities: []string{"get_draft"},
		}}},
		Match: Match{Intent: "get a draft"},
		Execute: []ExecuteStep{{
			ID:         "fetch",
			Connector:  "github://aileron/slack",
			Op:         "get_draft",
			Idempotent: &idempotent,
		}},
	}
	bundle := map[string]LoadedAction{
		"send-draft": {Manifest: gated, Path: "send-draft.md"},
		"get-draft":  {Manifest: sibling, Path: "get-draft.md"},
	}
	if errs := validatePreviewBundle(bundle); len(errs) != 0 {
		t.Errorf("validatePreviewBundle() = %v, want nil with idempotent sibling declaration", errs)
	}
}

// TestValidatePreviewBundle_RejectsApprovalGatedPreviewOp covers
// ADR-0016 rule 3: the preview op must not itself appear in an
// approval-gated action manifest. Permitting that would let a preview
// trigger a second approval prompt, recursing the user through nested
// gates instead of producing the one-shot summary the ADR specifies.
func TestValidatePreviewBundle_RejectsApprovalGatedPreviewOp(t *testing.T) {
	idempotent := true
	gated := previewManifest()
	gated.Name = "send-draft"
	approvalGatedSibling := &Manifest{
		Name:    "get-draft-gated",
		Version: "1.0.0",
		Source:  "hub://aileron/get-draft@1.0.0",
		Requires: Requires{Connectors: []RequiresConnector{{
			Name:         "github://aileron/slack",
			Version:      "1.0.0",
			Hash:         "sha256:abc",
			Capabilities: []string{"get_draft"},
		}}},
		Match: Match{Intent: "get a draft"},
		Execute: []ExecuteStep{{
			ID:         "fetch",
			Connector:  "github://aileron/slack",
			Op:         "get_draft",
			Idempotent: &idempotent,
		}},
		// The sibling declares the preview op as idempotent (satisfying
		// rule 2) *and* gates it behind approval — the combination is
		// what rule 3 prohibits.
		Approval: &ApprovalPolicy{Required: true},
	}
	bundle := map[string]LoadedAction{
		"send-draft":      {Manifest: gated, Path: "send-draft.md"},
		"get-draft-gated": {Manifest: approvalGatedSibling, Path: "get-draft-gated.md"},
	}
	errs := validatePreviewBundle(bundle)
	if len(errs) == 0 {
		t.Fatal("validatePreviewBundle() returned no errors; want one for approval-gated preview op")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "gated by [approval]") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("error messages = %v; want one mentioning approval-gated preview op", errs)
	}
}

