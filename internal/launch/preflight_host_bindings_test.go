package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setPreflightHome redirects the process home directory to a fresh temp dir
// and writes a host-binding user-layer descriptor at the default path
// (`~/.aileron/binding-descriptors.yaml`), so preflightHostBindings — which
// reads proxybinding.DefaultLoadOptions() — picks it up. Setting HOME plus the
// Windows equivalents keeps os.UserHomeDir() resolving under the temp dir on
// every platform.
func setPreflightHome(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	if runtime.GOOS == "windows" {
		vol := filepath.VolumeName(dir)
		t.Setenv("HOMEDRIVE", vol)
		t.Setenv("HOMEPATH", dir[len(vol):])
	}
	path := filepath.Join(dir, ".aileron", "binding-descriptors.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// preflightHostBindings validates the merged host-binding descriptor table
// before the agent environment boots. Its contract is to reuse the
// proxybinding loader's existing validation output: a placeholder field
// hard-fails the launch, a suspect-shape key warns without blocking, and a
// clean descriptor passes silently.

// TestPreflightHostBindings_CleanDescriptorPasses proves a well-formed user
// descriptor preflights with no error and no warning noise.
func TestPreflightHostBindings_CleanDescriptorPasses(t *testing.T) {
	setPreflightHome(t, "version: v1\n"+
		"bindings:\n"+
		"  - host: s3.amazonaws.com\n"+
		"    credential_ref: user/aws\n"+
		"    scheme: sigv4-resign\n"+
		"    access_key_id: AKIAIOSFODNN7EXAMPLE\n"+
		"    region: us-east-1\n"+
		"    service: s3\n")

	var out bytes.Buffer
	if err := preflightHostBindings(&out); err != nil {
		t.Fatalf("preflightHostBindings = %v, want nil for a clean descriptor", err)
	}
	if strings.Contains(out.String(), "warning") {
		t.Errorf("preflight output = %q; a well-shaped key must not warn", out.String())
	}
}

// TestPreflightHostBindings_PlaceholderFailsLaunch is the regression for
// feedback #1874 surface 2: a copy-paste placeholder in a descriptor field
// fails the preflight with the loader's field-named error, instead of the
// launch proceeding and surfacing an opaque upstream auth failure later.
func TestPreflightHostBindings_PlaceholderFailsLaunch(t *testing.T) {
	setPreflightHome(t, "version: v1\n"+
		"bindings:\n"+
		"  - host: s3.amazonaws.com\n"+
		"    credential_ref: user/aws\n"+
		"    scheme: sigv4-resign\n"+
		"    access_key_id: <AccessKeyId>\n"+
		"    region: us-east-1\n"+
		"    service: s3\n")

	var out bytes.Buffer
	err := preflightHostBindings(&out)
	if err == nil {
		t.Fatal("preflightHostBindings = nil, want a placeholder rejection")
	}
	for _, want := range []string{"access_key_id", "<AccessKeyId>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q (want field-named rejection)", err.Error(), want)
		}
	}
}

// TestPreflightHostBindings_SuspectShapeWarnsNotFatal proves a wrong-shaped
// sigv4-resign access_key_id preflights successfully (launch proceeds) but
// prints the loader's non-fatal warning naming the offending field and value.
func TestPreflightHostBindings_SuspectShapeWarnsNotFatal(t *testing.T) {
	setPreflightHome(t, "version: v1\n"+
		"bindings:\n"+
		"  - host: s3.amazonaws.com\n"+
		"    credential_ref: user/aws\n"+
		"    scheme: sigv4-resign\n"+
		"    access_key_id: totally-wrong-shape\n"+
		"    region: us-east-1\n"+
		"    service: s3\n")

	var out bytes.Buffer
	if err := preflightHostBindings(&out); err != nil {
		t.Fatalf("preflightHostBindings = %v, want nil (suspect shape is warn-only)", err)
	}
	got := out.String()
	for _, want := range []string{"warning", "access_key_id", "totally-wrong-shape"} {
		if !strings.Contains(got, want) {
			t.Errorf("preflight output = %q; missing %q", got, want)
		}
	}
}
