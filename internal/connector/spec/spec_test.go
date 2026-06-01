package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseValidatesConnectorSpec(t *testing.T) {
	spec, err := Parse("fixture.json", []byte(`{
	  "schema_version": "aileron.connector.v1",
	  "connector": {"fqn": "github://acme/aileron-connector-google", "version": "1.2.3"},
	  "tools": [{
	    "name": "google",
	    "description": "Google APIs",
	    "operations": [{
	      "name": "gmail.messages.send",
	      "summary": "Send a Gmail message",
	      "method": "POST",
	      "path": "/gmail/v1/users/me/messages/send",
	      "idempotency": "not_idempotent",
	      "approval": "required",
	      "credential": "oauth2",
	      "inputs": [{"name": "to", "type": "string", "required": true}]
	    }]
	  }]
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.Connector.FQN != "github://acme/aileron-connector-google" {
		t.Fatalf("connector fqn = %q", spec.Connector.FQN)
	}
	if got := spec.Tools[0].Operations[0].Inputs[0].Name; got != "to" {
		t.Fatalf("input name = %q", got)
	}
}

func TestParseRejectsInvalidSpec(t *testing.T) {
	_, err := Parse("bad.json", []byte(`{
	  "schema_version": "aileron.connector.v1",
	  "connector": {"fqn": "github://acme/connector"},
	  "tools": [{"name": "google", "operations": [{"name": "one"}, {"name": "one"}]}]
	}`))
	if err == nil {
		t.Fatal("expected duplicate operation error")
	}
	if !strings.Contains(err.Error(), "duplicate operation") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateRejectsRequiredFieldErrors(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want string
	}{
		{
			name: "schema",
			spec: Spec{
				SchemaVersion: "wrong",
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools:         []Tool{{Name: "google", Operations: []Operation{{Name: "list"}}}},
			},
			want: "schema_version",
		},
		{
			name: "fqn missing",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Tools:         []Tool{{Name: "google", Operations: []Operation{{Name: "list"}}}},
			},
			want: "connector.fqn is required",
		},
		{
			name: "fqn invalid",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "not-a-fqn"},
				Tools:         []Tool{{Name: "google", Operations: []Operation{{Name: "list"}}}},
			},
			want: "connector.fqn",
		},
		{
			name: "no tools",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
			},
			want: "at least one tool",
		},
		{
			name: "empty tool",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools:         []Tool{{Name: " "}},
			},
			want: "tool name is required",
		},
		{
			name: "unsafe tool",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools:         []Tool{{Name: "bad name", Operations: []Operation{{Name: "list"}}}},
			},
			want: "unsupported characters",
		},
		{
			name: "duplicate tool",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools: []Tool{
					{Name: "google", Operations: []Operation{{Name: "list"}}},
					{Name: "google", Operations: []Operation{{Name: "send"}}},
				},
			},
			want: "duplicate tool",
		},
		{
			name: "no operations",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools:         []Tool{{Name: "google"}},
			},
			want: "must declare at least one operation",
		},
		{
			name: "empty operation",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools:         []Tool{{Name: "google", Operations: []Operation{{Name: " "}}}},
			},
			want: "operation name is required",
		},
		{
			name: "empty input",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools: []Tool{{Name: "google", Operations: []Operation{{
					Name:   "list",
					Inputs: []Input{{Name: " "}},
				}}}},
			},
			want: "input name is required",
		},
		{
			name: "unsafe input",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools: []Tool{{Name: "google", Operations: []Operation{{
					Name:   "list",
					Inputs: []Input{{Name: "bad name"}},
				}}}},
			},
			want: "input name",
		},
		{
			name: "duplicate input",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools: []Tool{{Name: "google", Operations: []Operation{{
					Name:   "list",
					Inputs: []Input{{Name: "q"}, {Name: "q"}},
				}}}},
			},
			want: "duplicate input",
		},
		{
			name: "empty audit field",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools: []Tool{{Name: "google", Operations: []Operation{{
					Name:  "list",
					Audit: []AuditField{{Name: " "}},
				}}}},
			},
			want: "audit field name is required",
		},
		{
			name: "unsafe audit field",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools: []Tool{{Name: "google", Operations: []Operation{{
					Name:  "list",
					Audit: []AuditField{{Name: "bad name"}},
				}}}},
			},
			want: "audit field name",
		},
		{
			name: "duplicate audit field",
			spec: Spec{
				SchemaVersion: SchemaVersion,
				Connector:     Connector{FQN: "github://acme/connector"},
				Tools: []Tool{{Name: "google", Operations: []Operation{{
					Name:  "list",
					Audit: []AuditField{{Name: "request_id"}, {Name: "request_id"}},
				}}}},
			},
			want: "duplicate audit field",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.spec)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseRejectsUnsafeNames(t *testing.T) {
	_, err := Parse("bad.json", []byte(`{
	  "schema_version": "aileron.connector.v1",
	  "connector": {"fqn": "github://acme/connector"},
	  "tools": [{"name": "google", "operations": [{"name": "send\"mail"}]}]
	}`))
	if err == nil {
		t.Fatal("expected unsafe operation name error")
	}
	if !strings.Contains(err.Error(), "unsupported characters") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseRejectsMalformedJSON(t *testing.T) {
	_, err := Parse("bad.json", []byte(`{`))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse connector spec") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadInstalledReadsSpecsFromStore(t *testing.T) {
	root := t.TempDir()
	writeSpec(t, root, "bbb", "github://acme/b", "b")
	writeSpec(t, root, "aaa", "github://acme/a", "a")

	specs, err := LoadInstalled(root)
	if err != nil {
		t.Fatalf("LoadInstalled: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs len = %d, want 2", len(specs))
	}
	if specs[0].Connector.FQN != "github://acme/a" || specs[0].SourceHash != "sha256:aaa" {
		t.Fatalf("first spec = %+v", specs[0])
	}
}

func TestLoadInstalledSkipsEntriesWithoutSpecs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "connectors", "sha256", "abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "connectors", "sha256", "not-a-dir"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := LoadInstalled(root)
	if err != nil {
		t.Fatalf("LoadInstalled: %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("specs = %#v, want empty", specs)
	}
}

func TestLoadInstalledReturnsInvalidSpecError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "connectors", "sha256", "abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadInstalled(root)
	if err == nil {
		t.Fatal("expected spec parse error")
	}
	if !strings.Contains(err.Error(), "parse connector spec") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadInstalledMissingRootIsEmpty(t *testing.T) {
	specs, err := LoadInstalled(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("LoadInstalled: %v", err)
	}
	if specs != nil {
		t.Fatalf("specs = %#v, want nil", specs)
	}
}

func writeSpec(t *testing.T, root, hash, fqn, tool string) {
	t.Helper()
	dir := filepath.Join(root, "connectors", "sha256", hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{
	  "schema_version": "aileron.connector.v1",
	  "connector": {"fqn": "` + fqn + `"},
	  "tools": [{"name": "` + tool + `", "operations": [{"name": "list"}]}]
	}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
