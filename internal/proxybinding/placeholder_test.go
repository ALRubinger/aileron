package proxybinding

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
)

// TestLoad_RejectsPlaceholderAccessKeyID is the regression for feedback #1872:
// a user descriptor that leaves the sigv4-resign access_key_id as the
// copy-paste placeholder "<AccessKeyId>" must fail Load at daemon boot with a
// field-named error, instead of loading and failing at launch with an opaque
// UnrecognizedClientException. The error must name the file (Load wraps with
// the user path), the entry (Parse wraps with the binding index+host), and the
// field.
func TestLoad_RejectsPlaceholderAccessKeyID(t *testing.T) {
	dir := t.TempDir()
	body := "version: v1\n" +
		"bindings:\n" +
		"  - host: s3.amazonaws.com\n" +
		"    credential_ref: user/aws\n" +
		"    scheme: sigv4-resign\n" +
		"    access_key_id: <AccessKeyId>\n" +
		"    region: us-east-1\n" +
		"    service: s3\n"
	path := writeDescriptor(t, dir, "user.yaml", body)

	_, err := Load(LoadOptions{UserPath: path})
	if err == nil {
		t.Fatal("Load = nil error, want a load-time rejection of the placeholder access_key_id")
	}
	msg := err.Error()
	for _, want := range []string{"access_key_id", "<AccessKeyId>", "s3.amazonaws.com", path} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q (want file+entry+field naming)", msg, want)
		}
	}
}

// TestValidate_RejectsPlaceholderInEveryStringField proves the placeholder
// guard covers every scanned string field, not just access_key_id. Each case
// injects a "<...>" span into one field of an otherwise-valid entry and
// expects a field-named error.
func TestValidate_RejectsPlaceholderInEveryStringField(t *testing.T) {
	cases := []struct {
		field string
		mut   func(e *Entry)
	}{
		{"host", func(e *Entry) { e.Host = "<host>" }},
		{"credential_ref", func(e *Entry) { e.CredentialRef = "user/<svc>" }},
		{"scheme", func(e *Entry) { e.Scheme = "<scheme>" }},
		{"emit_mechanism", func(e *Entry) { e.EmitMechanism = "<mech>" }},
		{"username", func(e *Entry) { e.Scheme = binding.SchemeBasic; e.Username = "<user>" }},
		{"header", func(e *Entry) { e.Scheme = binding.SchemeHeaderTemplate; e.Header = "<hdr>"; e.Template = "{token}" }},
		{"template", func(e *Entry) {
			e.Scheme = binding.SchemeHeaderTemplate
			e.Header = "Authorization"
			e.Template = "<tpl>"
		}},
		{"query_param", func(e *Entry) { e.Scheme = binding.SchemeQueryParam; e.QueryParam = "<qp>" }},
		{"access_key_id", func(e *Entry) {
			e.Scheme = binding.SchemeSigV4Resign
			e.AccessKeyID = "<AccessKeyId>"
			e.Region = "us-east-1"
			e.Service = "s3"
		}},
		{"region", func(e *Entry) {
			e.Scheme = binding.SchemeSigV4Resign
			e.AccessKeyID = "AKIAIOSFODNN7EXAMPLE"
			e.Region = "<region>"
			e.Service = "s3"
		}},
		{"service", func(e *Entry) {
			e.Scheme = binding.SchemeSigV4Resign
			e.AccessKeyID = "AKIAIOSFODNN7EXAMPLE"
			e.Region = "us-east-1"
			e.Service = "<service>"
		}},
		{"effect", func(e *Entry) { e.Effect = "<effect>" }},
		{"allowed_hosts[0]", func(e *Entry) { e.AllowedHosts = []string{"<host>"} }},
		{"sentinel.value", func(e *Entry) {
			e.EmitMechanism = string(binding.EmitMechanismSentinelSwap)
			e.Sentinel = &Sentinel{Value: "<value>", Env: "GH_TOKEN"}
		}},
		{"sentinel.env", func(e *Entry) {
			e.EmitMechanism = string(binding.EmitMechanismSentinelSwap)
			e.Sentinel = &Sentinel{Value: "ghp_placeholder", Env: "<ENV>"}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			e := &Entry{
				Host:          "api.example.com",
				CredentialRef: "user/example",
				Scheme:        binding.SchemeBearer,
			}
			tc.mut(e)
			err := e.Validate()
			if err == nil {
				t.Fatalf("Validate = nil, want placeholder rejection for %s", tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name field %q", err.Error(), tc.field)
			}
		})
	}
}

// TestLoad_AKIDEXAMPLEAndBuiltinsLoadClean proves the placeholder guard and the
// warn-only sigv4 shape check produce no false positives: the repository's own
// documented SigV4 test vector "AKIDEXAMPLE" loads and produces no warning that
// would block boot, and the embedded built-in defaults (Linear) load clean.
func TestLoad_AKIDEXAMPLEAndBuiltinsLoadClean(t *testing.T) {
	// Built-in defaults must load without error.
	if _, err := Load(LoadOptions{}); err != nil {
		t.Fatalf("built-in defaults must load clean: %v", err)
	}

	dir := t.TempDir()
	body := "version: v1\n" +
		"bindings:\n" +
		"  - host: s3.amazonaws.com\n" +
		"    credential_ref: user/aws\n" +
		"    scheme: sigv4-resign\n" +
		"    access_key_id: AKIDEXAMPLE\n" +
		"    region: us-east-1\n" +
		"    service: s3\n"
	path := writeDescriptor(t, dir, filepath.Base("aws-user.yaml"), body)

	table, warnings, err := LoadHostBindingsWithWarnings(LoadOptions{UserPath: path})
	if err != nil {
		t.Fatalf("AKIDEXAMPLE vector must load clean, not error: %v", err)
	}
	if _, ok := table.Match("s3.amazonaws.com"); !ok {
		t.Error("AKIDEXAMPLE binding must be present in the table")
	}
	// AKIDEXAMPLE does not match the AKIA/ASIA shape, so warn-only surfaces a
	// warning; the load must still succeed (it did above). The contract is:
	// warn, do not block. We only assert it loaded; a warning here is allowed.
	_ = warnings
}

// TestLoadWithWarnings_SuspectSigV4KeyWarns proves a well-formed sigv4-resign
// entry with a wrong-shaped access_key_id loads successfully but is surfaced as
// a non-fatal warning naming the field and value.
func TestLoadWithWarnings_SuspectSigV4KeyWarns(t *testing.T) {
	dir := t.TempDir()
	body := "version: v1\n" +
		"bindings:\n" +
		"  - host: s3.amazonaws.com\n" +
		"    credential_ref: user/aws\n" +
		"    scheme: sigv4-resign\n" +
		"    access_key_id: totally-wrong-shape\n" +
		"    region: us-east-1\n" +
		"    service: s3\n"
	path := writeDescriptor(t, dir, "user.yaml", body)

	table, warnings, err := LoadHostBindingsWithWarnings(LoadOptions{UserPath: path})
	if err != nil {
		t.Fatalf("suspect-shape key must not block load: %v", err)
	}
	if _, ok := table.Match("s3.amazonaws.com"); !ok {
		t.Error("suspect-key binding must still be present in the table")
	}
	if len(warnings) == 0 {
		t.Fatal("expected a warning for the wrong-shaped access_key_id, got none")
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"access_key_id", "totally-wrong-shape"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warning %q missing %q", joined, want)
		}
	}
}

// TestWarnings_WellShapedSigV4KeyIsSilent proves a canonical AKIA/ASIA-prefixed
// access_key_id produces no warning, so the warn path does not cry wolf on
// legitimate keys.
func TestWarnings_WellShapedSigV4KeyIsSilent(t *testing.T) {
	for _, key := range []string{"AKIAIOSFODNN7EXAMPLE", "ASIAY34FZKBOKMUTVV7A"} {
		e := &Entry{
			Host:          "s3.amazonaws.com",
			CredentialRef: "user/aws",
			Scheme:        binding.SchemeSigV4Resign,
			AccessKeyID:   key,
			Region:        "us-east-1",
			Service:       "s3",
		}
		if err := e.Validate(); err != nil {
			t.Fatalf("well-shaped key %q must validate: %v", key, err)
		}
		if w := e.Warnings(); len(w) != 0 {
			t.Errorf("well-shaped key %q produced warnings: %v", key, w)
		}
	}
}
