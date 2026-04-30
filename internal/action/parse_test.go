package action

import (
	"errors"
	"strings"
	"testing"
)

const validActionFile = `+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc123"
capabilities = ["chat:write", "channels:read"]

[[requires.connectors]]
name = "github://aileron/git"
version = "2.1.0"
hash = "sha256:def456"
capabilities = ["read"]

[match]
intent = "tell team I shipped"

[[execute]]
id = "recent_merge"
connector = "github://aileron/git"
op = "read_recent_merge"

[[execute]]
id = "post"
connector = "github://aileron/slack"
op = "post_message"

[execute.inputs]
channel = "${args.channel}"
+++

# Ship Update

Posts a "shipped" announcement to a Slack channel.
`

func TestParse_ValidFrontmatterAndBody(t *testing.T) {
	m, err := Parse("ship-update.md", []byte(validActionFile))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if m.Name != "ship-update" {
		t.Errorf("Name = %q, want ship-update", m.Name)
	}
	if m.Version != "1.0.0" {
		t.Errorf("Version = %q, want 1.0.0", m.Version)
	}
	if m.Source != "hub://aileron/ship-update@1.0.0" {
		t.Errorf("Source = %q, want hub URI", m.Source)
	}
	if len(m.Requires.Connectors) != 2 {
		t.Fatalf("got %d connectors, want 2", len(m.Requires.Connectors))
	}
	if got := m.Requires.Connectors[0].Capabilities; len(got) != 2 || got[0] != "chat:write" {
		t.Errorf("connectors[0].Capabilities = %v, want [chat:write channels:read]", got)
	}
	if m.Match.Intent != "tell team I shipped" {
		t.Errorf("Match.Intent = %q", m.Match.Intent)
	}
	if len(m.Execute) != 2 {
		t.Fatalf("got %d execute steps, want 2", len(m.Execute))
	}
	if m.Execute[1].Inputs["channel"] != "${args.channel}" {
		t.Errorf("execute[1].inputs.channel = %v", m.Execute[1].Inputs["channel"])
	}
	if !strings.Contains(m.Body, "# Ship Update") {
		t.Errorf("Body missing heading; got %q", m.Body)
	}
	if strings.Contains(m.Body, "+++") {
		t.Errorf("Body should not contain frontmatter delimiter; got %q", m.Body)
	}
}

func TestParse_MissingOpeningDelimiter(t *testing.T) {
	data := `name = "x"
+++
body
`
	_, err := Parse("bad.md", []byte(data))
	if err == nil {
		t.Fatal("Parse() succeeded; want missing-opening-delimiter error")
	}
	var aerr *Error
	if !errors.As(err, &aerr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if aerr.Class != ClassParseError {
		t.Errorf("Class = %s, want %s", aerr.Class, ClassParseError)
	}
	if aerr.File != "bad.md" {
		t.Errorf("File = %q", aerr.File)
	}
}

func TestParse_MissingClosingDelimiter(t *testing.T) {
	data := `+++
name = "x"
version = "1.0.0"
source = "hub://aileron/x@1.0.0"
`
	_, err := Parse("noclose.md", []byte(data))
	if err == nil {
		t.Fatal("Parse() succeeded; want missing-closing-delimiter error")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassParseError {
		t.Fatalf("expected ClassParseError, got %v", err)
	}
	if !strings.Contains(aerr.Message, "closing") {
		t.Errorf("message %q does not mention closing delimiter", aerr.Message)
	}
}

func TestParse_TOMLSyntaxErrorReportsLine(t *testing.T) {
	data := `+++
name = "x"
this is not valid toml = =
+++
`
	_, err := Parse("bad-toml.md", []byte(data))
	if err == nil {
		t.Fatal("Parse() succeeded; want TOML syntax error")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassParseError {
		t.Fatalf("expected ClassParseError, got %v", err)
	}
	if aerr.Line == 0 {
		t.Errorf("expected Line > 0, got %d (msg=%s)", aerr.Line, aerr.Message)
	}
}

func TestParse_UnknownFrontmatterKeyRejected(t *testing.T) {
	data := `+++
name = "x"
version = "1.0.0"
source = "hub://aileron/x@1.0.0"
unexpected_field = "nope"

[[requires.connectors]]
name = "github://aileron/slack"
version = "1.0.0"
hash = "sha256:abc"
capabilities = ["chat:write"]

[match]
intent = "x"

[[execute]]
id = "a"
connector = "github://aileron/slack"
op = "post"
+++
`
	_, err := Parse("unknown.md", []byte(data))
	if err == nil {
		t.Fatal("Parse() succeeded; want unknown-key error")
	}
	if !strings.Contains(err.Error(), "unexpected_field") {
		t.Errorf("error %q does not name the unknown field", err.Error())
	}
}

func TestParse_LeadingBOMTolerated(t *testing.T) {
	data := "\ufeff" + validActionFile
	if _, err := Parse("bom.md", []byte(data)); err != nil {
		t.Fatalf("Parse() with BOM error = %v", err)
	}
}

func TestParse_LeadingBlankLinesTolerated(t *testing.T) {
	data := "\n\n" + validActionFile
	if _, err := Parse("blank.md", []byte(data)); err != nil {
		t.Fatalf("Parse() with leading blanks error = %v", err)
	}
}

func TestParse_NoBodyIsAllowed(t *testing.T) {
	data := `+++
name = "noop"
version = "1.0.0"
source = "hub://aileron/noop@1.0.0"

[[requires.connectors]]
name = "github://aileron/slack"
version = "1.0.0"
hash = "sha256:abc"
capabilities = ["chat:write"]

[match]
intent = "noop"

[[execute]]
id = "a"
connector = "github://aileron/slack"
op = "post"
+++
`
	m, err := Parse("nobody.md", []byte(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if m.Body != "" {
		t.Errorf("Body = %q, want empty", m.Body)
	}
}
