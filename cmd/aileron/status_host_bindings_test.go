package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeUserDescriptor writes a host-binding user-layer descriptor at the
// default path under the test HOME (`~/.aileron/binding-descriptors.yaml`),
// so showStatusHostBindings — which reads proxybinding.DefaultLoadOptions() —
// picks it up as the user override layer.
func writeUserDescriptor(t *testing.T, home, body string) {
	t.Helper()
	path := filepath.Join(home, ".aileron", "binding-descriptors.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// showStatusHostBindings renders the "Host bindings" section of
// `aileron status`. Its contract is to reuse the proxybinding loader's
// existing validation output: on a clean load it lists each merged entry's
// host and scheme; on a placeholder-rejected load it prints the loader's
// file/entry/field-named error as the section state.

// TestShowStatusHostBindings_ListsCleanEntries proves a well-formed user
// descriptor loads and its host+scheme surface in the status view.
func TestShowStatusHostBindings_ListsCleanEntries(t *testing.T) {
	home := setTestHome(t)
	writeUserDescriptor(t, home, "version: v1\n"+
		"bindings:\n"+
		"  - host: s3.amazonaws.com\n"+
		"    credential_ref: user/aws\n"+
		"    scheme: sigv4-resign\n"+
		"    access_key_id: AKIAIOSFODNN7EXAMPLE\n"+
		"    region: us-east-1\n"+
		"    service: s3\n")

	var out bytes.Buffer
	showStatusHostBindings(&out)
	got := out.String()

	if !strings.Contains(got, "s3.amazonaws.com") {
		t.Errorf("status view = %q; want the configured host listed", got)
	}
	if !strings.Contains(got, "sigv4-resign") {
		t.Errorf("status view = %q; want the entry scheme listed", got)
	}
	if strings.Contains(got, "error:") {
		t.Errorf("status view = %q; a well-formed descriptor must not report an error", got)
	}
	if strings.Contains(got, "warning:") {
		t.Errorf("status view = %q; a well-shaped key must not warn", got)
	}
}

// TestShowStatusHostBindings_PlaceholderReportsError is the regression for
// feedback #1874 surface 1: a copy-paste placeholder in a descriptor field
// hard-fails the loader, and the section prints the file/entry/field-named
// error as its state rather than silently listing a broken binding.
func TestShowStatusHostBindings_PlaceholderReportsError(t *testing.T) {
	home := setTestHome(t)
	writeUserDescriptor(t, home, "version: v1\n"+
		"bindings:\n"+
		"  - host: s3.amazonaws.com\n"+
		"    credential_ref: user/aws\n"+
		"    scheme: sigv4-resign\n"+
		"    access_key_id: <AccessKeyId>\n"+
		"    region: us-east-1\n"+
		"    service: s3\n")

	var out bytes.Buffer
	showStatusHostBindings(&out)
	got := out.String()

	if !strings.Contains(got, "error:") {
		t.Errorf("status view = %q; want a placeholder-rejection error state", got)
	}
	for _, want := range []string{"access_key_id", "<AccessKeyId>"} {
		if !strings.Contains(got, want) {
			t.Errorf("status view = %q; want field-named error containing %q", got, want)
		}
	}
}

// TestShowStatusHostBindings_SuspectShapeWarns proves a well-formed but
// wrong-shaped sigv4-resign access_key_id loads (no error) yet surfaces the
// loader's non-fatal warning under its entry.
func TestShowStatusHostBindings_SuspectShapeWarns(t *testing.T) {
	home := setTestHome(t)
	writeUserDescriptor(t, home, "version: v1\n"+
		"bindings:\n"+
		"  - host: s3.amazonaws.com\n"+
		"    credential_ref: user/aws\n"+
		"    scheme: sigv4-resign\n"+
		"    access_key_id: totally-wrong-shape\n"+
		"    region: us-east-1\n"+
		"    service: s3\n")

	var out bytes.Buffer
	showStatusHostBindings(&out)
	got := out.String()

	if strings.Contains(got, "error:") {
		t.Errorf("status view = %q; a suspect-shape key must not error", got)
	}
	if !strings.Contains(got, "warning:") {
		t.Errorf("status view = %q; want a suspect-shape warning", got)
	}
	if !strings.Contains(got, "totally-wrong-shape") {
		t.Errorf("status view = %q; warning must name the offending value", got)
	}
}
