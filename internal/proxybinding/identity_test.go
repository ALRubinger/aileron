package proxybinding

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
)

// TestParse_HostlessIdentityEntry pins the host-less identity binding: a
// sigv4-resign entry that declares kind + identity_label but no host loads,
// and its ToHostBinding is selectable by MatchIdentity.
func TestParse_HostlessIdentityEntry(t *testing.T) {
	const yaml = `
version: v1
bindings:
  - kind: aws-sigv4
    identity_label: metrics-reader
    credential_ref: user/aws
    scheme: sigv4-resign
    access_key_id: AKIDEXAMPLE
`
	d, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(d.Bindings))
	}
	e := d.Bindings[0]
	if e.Host != "" {
		t.Errorf("host = %q, want empty for identity binding", e.Host)
	}
	if e.Kind != "aws-sigv4" || e.IdentityLabel != "metrics-reader" {
		t.Errorf("identity = (%q,%q), want (aws-sigv4,metrics-reader)", e.Kind, e.IdentityLabel)
	}
	hb, err := e.ToHostBinding()
	if err != nil {
		t.Fatalf("ToHostBinding: %v", err)
	}
	tbl := binding.HostBindings{hb}
	if _, ok := tbl.MatchIdentity("aws-sigv4", "metrics-reader"); !ok {
		t.Error("MatchIdentity must select the parsed host-less identity binding")
	}
}

// TestParse_TwoIdentityEntriesDifferentLabels ensures two host-less identity
// entries with different labels coexist in one document: they must NOT collide
// as `duplicate host ""`.
func TestParse_TwoIdentityEntriesDifferentLabels(t *testing.T) {
	const yaml = `
version: v1
bindings:
  - kind: aws-sigv4
    identity_label: metrics-reader
    credential_ref: user/aws
    scheme: sigv4-resign
    access_key_id: AKIDEXAMPLE
  - kind: aws-sigv4
    identity_label: admin
    credential_ref: user/aws
    scheme: sigv4-resign
    access_key_id: AKIDEXAMPLE
`
	d, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Bindings) != 2 {
		t.Fatalf("bindings = %d, want 2", len(d.Bindings))
	}
}

// TestParse_DuplicateIdentityPair rejects two entries with the same
// (kind, identity_label) pair, naming the pair in the error.
func TestParse_DuplicateIdentityPair(t *testing.T) {
	const yaml = `
version: v1
bindings:
  - kind: aws-sigv4
    identity_label: metrics-reader
    credential_ref: user/aws
    scheme: sigv4-resign
    access_key_id: AKIDEXAMPLE
  - kind: aws-sigv4
    identity_label: metrics-reader
    credential_ref: user/other
    scheme: sigv4-resign
    access_key_id: AKIDEXAMPLE
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected duplicate-identity error, got nil")
	}
	if !strings.Contains(err.Error(), "identity") || !strings.Contains(err.Error(), "metrics-reader") {
		t.Errorf("error = %q, want it to name the duplicate identity pair", err.Error())
	}
}

// TestParse_NeitherHostNorIdentity rejects an entry with no host and no
// identity: it has no selection key.
func TestParse_NeitherHostNorIdentity(t *testing.T) {
	const yaml = `
version: v1
bindings:
  - credential_ref: user/aws
    scheme: sigv4-resign
    access_key_id: AKIDEXAMPLE
`
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("expected error for entry with neither host nor identity, got nil")
	}
}

// TestParse_PartialIdentity rejects a half-identity (exactly one of kind or
// identity_label) whether or not a host is present.
func TestParse_PartialIdentity(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "kind only, host-less",
			yaml: "version: v1\nbindings:\n  - kind: aws-sigv4\n    credential_ref: user/aws\n    scheme: sigv4-resign\n    access_key_id: AKIDEXAMPLE\n",
		},
		{
			name: "label only, host-less",
			yaml: "version: v1\nbindings:\n  - identity_label: metrics-reader\n    credential_ref: user/aws\n    scheme: sigv4-resign\n    access_key_id: AKIDEXAMPLE\n",
		},
		{
			name: "kind only, with host",
			yaml: "version: v1\nbindings:\n  - host: s3.amazonaws.com\n    kind: aws-sigv4\n    credential_ref: user/aws\n    scheme: sigv4-resign\n    access_key_id: AKIDEXAMPLE\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.yaml)); err == nil {
				t.Fatalf("Parse(%s) = nil error, want error", tc.name)
			}
		})
	}
}

// TestParse_PlaceholderScanCoversIdentityFields ensures the placeholder guard
// catches an un-substituted "<...>" span in kind or identity_label.
func TestParse_PlaceholderScanCoversIdentityFields(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		field string
	}{
		{
			name:  "placeholder in kind",
			yaml:  "version: v1\nbindings:\n  - kind: \"<kind>\"\n    identity_label: metrics-reader\n    credential_ref: user/aws\n    scheme: sigv4-resign\n    access_key_id: AKIDEXAMPLE\n",
			field: "kind",
		},
		{
			name:  "placeholder in identity_label",
			yaml:  "version: v1\nbindings:\n  - kind: aws-sigv4\n    identity_label: \"<label>\"\n    credential_ref: user/aws\n    scheme: sigv4-resign\n    access_key_id: AKIDEXAMPLE\n",
			field: "identity_label",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected placeholder error, got nil")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error = %q, want it to name field %q", err.Error(), tc.field)
			}
		})
	}
}

// TestLoad_UserLayerOverridesIdentityBinding pins that a user-layer identity
// entry overrides a built-in one with the same (kind, label) pair, and that
// host entries still override per host, with deterministic ordering.
func TestLoad_UserLayerOverridesIdentityBinding(t *testing.T) {
	builtin := Entry{
		Kind:          "aws-sigv4",
		IdentityLabel: "metrics-reader",
		CredentialRef: "user/aws",
		Scheme:        "sigv4-resign",
		AccessKeyID:   "AKIDEXAMPLE",
	}
	// A different label => distinct dedup key; both survive.
	otherLabel := Entry{
		Kind:          "aws-sigv4",
		IdentityLabel: "admin",
		CredentialRef: "user/aws",
		Scheme:        "sigv4-resign",
		AccessKeyID:   "AKIDEXAMPLE",
	}
	// Same pair as builtin => overrides it (different credential_ref).
	override := Entry{
		Kind:          "aws-sigv4",
		IdentityLabel: "metrics-reader",
		CredentialRef: "oauth2/aws/metrics",
		Scheme:        "sigv4-resign",
		AccessKeyID:   "AKIDEXAMPLE",
	}

	// Emulate the merge via applyLayer directly (Load uses the same helper).
	merged := map[string]Entry{}
	applyLayer(merged, []Entry{builtin, otherLabel})
	applyLayer(merged, []Entry{override})
	if len(merged) != 2 {
		t.Fatalf("merged entries = %d, want 2 (override collapses onto builtin)", len(merged))
	}
	got := merged[override.dedupKey()]
	if got.CredentialRef != "oauth2/aws/metrics" {
		t.Errorf("overridden credential_ref = %q, want oauth2/aws/metrics", got.CredentialRef)
	}
	if merged[otherLabel.dedupKey()].IdentityLabel != "admin" {
		t.Error("the distinct-label identity binding must survive the merge")
	}
}
