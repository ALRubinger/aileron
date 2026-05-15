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

[[inputs]]
name = "channel"
type = "string"
description = "Slack channel to post to (e.g. '#engineering')."

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

// TestParse_InputDisplayMetadata pins the TOML round-trip of the
// optional per-input `label` and `multiline` keys. They affect only
// the approval surface (per the ADR-0003 amendment) and have no
// bearing on the LLM-facing JSON Schema.
func TestParse_InputDisplayMetadata(t *testing.T) {
	data := `+++
name = "send-email"
version = "1.0.0"
source = "hub://aileron/send-email@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc"
capabilities = ["gmail:send"]

[match]
intent = "send an email"

[[inputs]]
name = "to"
type = "string"
description = "Recipient address."
label = "To"

[[inputs]]
name = "subject"
type = "string"
description = "Subject line."
label = "Subject"

[[inputs]]
name = "body"
type = "string"
description = "Plain-text body."
label = "Body"
multiline = true

[[execute]]
id = "send"
connector = "github://aileron/google"
op = "send_email"
+++

# Send Email
`
	m, err := Parse("send-email.md", []byte(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(m.Inputs) != 3 {
		t.Fatalf("got %d inputs, want 3", len(m.Inputs))
	}
	if got, want := m.Inputs[0].Label, "To"; got != want {
		t.Errorf("Inputs[0].Label = %q, want %q", got, want)
	}
	if m.Inputs[0].Multiline {
		t.Errorf("Inputs[0].Multiline = true, want false (unset)")
	}
	if got, want := m.Inputs[2].Label, "Body"; got != want {
		t.Errorf("Inputs[2].Label = %q, want %q", got, want)
	}
	if !m.Inputs[2].Multiline {
		t.Errorf("Inputs[2].Multiline = false, want true")
	}
	if err := Validate(m, "send-email.md"); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

// TestParse_InputItemsType pins the TOML round-trip of the optional
// per-input `items_type` key on array inputs. The field declares the
// LLM-facing element shape so MCP clients project array elements
// correctly rather than defaulting to `string[]`.
func TestParse_InputItemsType(t *testing.T) {
	data := `+++
name = "update-doc"
version = "1.0.0"
source = "hub://aileron/update-doc@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc"
capabilities = ["docs:write"]

[match]
intent = "update a doc"

[[inputs]]
name = "document_id"
type = "string"
description = "The Doc id."

[[inputs]]
name = "requests"
type = "array"
items_type = "object"
description = "Array of Docs API Request objects."

[[inputs]]
name = "tags"
type = "array"
description = "Optional plain-string tag list (no items_type)."

[[execute]]
id = "patch"
connector = "github://aileron/google"
op = "batch_update"
+++

# Update Doc
`
	m, err := Parse("update-doc.md", []byte(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(m.Inputs) != 3 {
		t.Fatalf("got %d inputs, want 3", len(m.Inputs))
	}
	if got := m.Inputs[1].ItemsType; got != "object" {
		t.Errorf("Inputs[1].ItemsType = %q, want %q", got, "object")
	}
	if got := m.Inputs[2].ItemsType; got != "" {
		t.Errorf("Inputs[2].ItemsType = %q, want empty (omitted)", got)
	}
	if err := Validate(m, "update-doc.md"); err != nil {
		t.Fatalf("Validate() error = %v", err)
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

// TestParse_ApprovalRequired verifies that an action manifest declaring
// `[approval] required = true` produces an Approval policy with
// `Required: true`, so the runtime can later gate execution on a user
// decision. The default-no-approval behavior is verified separately.
func TestParse_ApprovalRequired(t *testing.T) {
	data := `+++
name = "send-email"
version = "1.0.0"
source = "hub://aileron/send-email@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc"
capabilities = ["send"]

[match]
intent = "send an email"

[[execute]]
id = "send"
connector = "github://aileron/google"
op = "send_email"

[approval]
required = true
+++
`
	m, err := Parse("send-email.md", []byte(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !m.ApprovalRequired() {
		t.Errorf("ApprovalRequired() = false, want true (manifest declares [approval] required = true)")
	}
	if m.Approval == nil || !m.Approval.Required {
		t.Errorf("Approval = %+v, want non-nil with Required=true", m.Approval)
	}
}

// TestParse_NoApprovalBlockDefaultsFalse asserts that an action manifest
// without an `[approval]` block leaves Approval nil, and ApprovalRequired
// reports false. This is the legacy-action behavior the runtime already
// supports — actions run immediately without gating.
func TestParse_NoApprovalBlockDefaultsFalse(t *testing.T) {
	data := `+++
name = "ping"
version = "1.0.0"
source = "hub://aileron/ping@1.0.0"

[[requires.connectors]]
name = "github://aileron/x"
version = "1.0.0"
hash = "sha256:abc"
capabilities = ["read"]

[match]
intent = "ping"

[[execute]]
id = "p"
connector = "github://aileron/x"
op = "ping"
+++
`
	m, err := Parse("ping.md", []byte(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if m.Approval != nil {
		t.Errorf("Approval = %+v, want nil for action without [approval] block", m.Approval)
	}
	if m.ApprovalRequired() {
		t.Errorf("ApprovalRequired() = true, want false (no [approval] block)")
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

// TestParse_ApprovalPreviewBlock verifies that a manifest declaring
// `[approval.preview]` (ADR-0016) parses into a populated PreviewPolicy
// carrying the op name, the args template, and the render map. The
// preview is part of the manifest contract; without successful parse,
// the runtime would fall through to the default "no preview" path
// even when the author declared one.
func TestParse_ApprovalPreviewBlock(t *testing.T) {
	data := `+++
name = "send-draft"
version = "1.0.0"
source = "hub://aileron/send-draft@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc"
capabilities = ["send_draft", "get_draft"]

[match]
intent = "send a draft"

[[inputs]]
name = "draft_id"
type = "string"
description = "Draft id to send"

[[execute]]
id = "send"
connector = "github://aileron/google"
op = "send_draft"

[approval]
required = true

[approval.preview]
op = "get_draft"
args = { id = "${args.draft_id}" }
render = { To = "message.payload.headers.To", Subject = "message.payload.headers.Subject", Preview = "message.snippet" }
+++
`
	m, err := Parse("send-draft.md", []byte(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	preview := m.ApprovalPreview()
	if preview == nil {
		t.Fatal("ApprovalPreview() = nil, want non-nil for manifest declaring [approval.preview]")
	}
	if preview.Op != "get_draft" {
		t.Errorf("preview.Op = %q, want %q", preview.Op, "get_draft")
	}
	if got := preview.Args["id"]; got != "${args.draft_id}" {
		t.Errorf("preview.Args[id] = %v, want ${args.draft_id}", got)
	}
	if got := preview.Render["To"]; got != "message.payload.headers.To" {
		t.Errorf("preview.Render[To] = %q, want %q", got, "message.payload.headers.To")
	}
	if got := preview.Render["Subject"]; got != "message.payload.headers.Subject" {
		t.Errorf("preview.Render[Subject] = %q, want path", got)
	}
	if got := preview.Render["Preview"]; got != "message.snippet" {
		t.Errorf("preview.Render[Preview] = %q, want %q", got, "message.snippet")
	}
}

// TestParse_ApprovalPreviewRenderOrderPreservesDeclaration asserts that
// the parser captures the manifest's declaration order for the render
// keys via PreviewPolicy.RenderOrder. The approval prompt renders
// labels in this order; without preservation, Go's randomized map
// iteration would yield a different field order on every call.
func TestParse_ApprovalPreviewRenderOrderPreservesDeclaration(t *testing.T) {
	data := `+++
name = "send-draft"
version = "1.0.0"
source = "hub://aileron/send-draft@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc"
capabilities = ["send_draft", "get_draft"]

[match]
intent = "send a draft"

[[inputs]]
name = "draft_id"
type = "string"
description = "Draft id"

[[execute]]
id = "send"
connector = "github://aileron/google"
op = "send_draft"

[approval]
required = true

[approval.preview]
op = "get_draft"
args = { id = "${args.draft_id}" }

[approval.preview.render]
To = "message.payload.headers.To"
Subject = "message.payload.headers.Subject"
Preview = "message.snippet"
+++
`
	m, err := Parse("send-draft.md", []byte(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	preview := m.ApprovalPreview()
	if preview == nil {
		t.Fatal("ApprovalPreview() = nil, want non-nil")
	}
	want := []string{"To", "Subject", "Preview"}
	if got := preview.RenderOrder; len(got) != len(want) {
		t.Fatalf("RenderOrder length = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i, label := range want {
		if preview.RenderOrder[i] != label {
			t.Errorf("RenderOrder[%d] = %q, want %q (full order: %v)", i, preview.RenderOrder[i], label, preview.RenderOrder)
		}
	}
}

// TestParse_ApprovalPreviewMultiline asserts that the optional
// `multiline` list in `[approval.preview]` parses into
// PreviewPolicy.Multiline as the declared label sequence. The approval
// surface uses this list to render those labels as scrollable
// blockquotes rather than single-line key/value entries (ADR-0016).
func TestParse_ApprovalPreviewMultiline(t *testing.T) {
	data := `+++
name = "send-draft"
version = "1.0.0"
source = "hub://aileron/send-draft@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc"
capabilities = ["send_draft", "get_draft"]

[match]
intent = "send a draft"

[[inputs]]
name = "draft_id"
type = "string"
description = "Draft id"

[[execute]]
id = "send"
connector = "github://aileron/google"
op = "send_draft"

[approval]
required = true

[approval.preview]
op = "get_draft"
args = { id = "${args.draft_id}" }
render = { To = "message.payload.headers.To", Subject = "message.payload.headers.Subject", Body = "message.snippet" }
multiline = ["Body"]
+++
`
	m, err := Parse("send-draft.md", []byte(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	preview := m.ApprovalPreview()
	if preview == nil {
		t.Fatal("ApprovalPreview() = nil, want non-nil")
	}
	want := []string{"Body"}
	if got := preview.Multiline; len(got) != len(want) {
		t.Fatalf("Multiline length = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i, label := range want {
		if preview.Multiline[i] != label {
			t.Errorf("Multiline[%d] = %q, want %q", i, preview.Multiline[i], label)
		}
	}
}

// TestParse_ApprovalPreviewMultilineOmittedIsEmpty asserts the
// migration contract from ADR-0016: a manifest that omits `multiline`
// parses into an empty list, reproducing the prior all-inline render
// behavior. Without this guarantee, every existing manifest would need
// an explicit `multiline = []` to keep working.
func TestParse_ApprovalPreviewMultilineOmittedIsEmpty(t *testing.T) {
	data := `+++
name = "send-draft"
version = "1.0.0"
source = "hub://aileron/send-draft@1.0.0"

[[requires.connectors]]
name = "github://aileron/google"
version = "1.0.0"
hash = "sha256:abc"
capabilities = ["send_draft", "get_draft"]

[match]
intent = "send a draft"

[[inputs]]
name = "draft_id"
type = "string"
description = "Draft id"

[[execute]]
id = "send"
connector = "github://aileron/google"
op = "send_draft"

[approval]
required = true

[approval.preview]
op = "get_draft"
args = { id = "${args.draft_id}" }
render = { To = "message.payload.headers.To" }
+++
`
	m, err := Parse("send-draft.md", []byte(data))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	preview := m.ApprovalPreview()
	if preview == nil {
		t.Fatal("ApprovalPreview() = nil, want non-nil")
	}
	if len(preview.Multiline) != 0 {
		t.Errorf("Multiline = %v, want empty (manifest omitted the directive)", preview.Multiline)
	}
}
