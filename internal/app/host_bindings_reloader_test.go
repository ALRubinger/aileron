package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/auth/capture"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// primeHostBindingsReloader builds a reloader over a specific user-descriptor
// path and cached unit layer, performing the initial load exactly as the
// production constructor does (minus the image resolution the tests control
// separately). It is the seam that lets a test drive the user-file edit path
// deterministically without a real home directory.
func primeHostBindingsReloader(t *testing.T, userPath string, extra []proxybinding.Entry) *hostBindingsReloader {
	t.Helper()
	r := &hostBindingsReloader{
		userPath:     userPath,
		extraEntries: extra,
		log:          discardLogger(),
	}
	sig := r.statUserFile()
	table, err := r.loadTable()
	if err != nil {
		t.Fatalf("initial loadTable: %v", err)
	}
	r.table = table
	r.sig = sig
	return r
}

// writeUserDescriptor writes a single bearer host binding into a fresh
// descriptor at path via the same Upsert the CLI uses, so the file is a
// realistic, load-valid document rather than hand-rolled YAML.
func writeUserDescriptor(t *testing.T, path, host, ref string) {
	t.Helper()
	if err := proxybinding.Upsert(path, proxybinding.Entry{
		Host:          host,
		CredentialRef: ref,
		Scheme:        binding.SchemeBearer,
	}); err != nil {
		t.Fatalf("Upsert descriptor: %v", err)
	}
}

// TestHostBindingsReloader_PicksUpUserFileEdit is the core #1887 regression: a
// binding written to the descriptor file AFTER the reloader was constructed is
// matched on the next access, with no reconstruction ("restart"). Before the
// reload holder existed, the table was assembled once and this edit was invisible
// until a daemon restart.
func TestHostBindingsReloader_PicksUpUserFileEdit(t *testing.T) {
	descPath := filepath.Join(t.TempDir(), "binding-descriptors.yaml")

	// Constructed against an absent user file: built-in defaults only.
	r := primeHostBindingsReloader(t, descPath, nil)
	if _, ok := r.current().Match("api.linear.app"); !ok {
		t.Fatal("defaults-only table must still carry the built-in api.linear.app")
	}
	if _, ok := r.current().Match("edited.example.test"); ok {
		t.Fatal("host must be unbound before the descriptor is written")
	}

	// Operator (or `skill bind`) writes a new binding out of band.
	writeUserDescriptor(t, descPath, "edited.example.test", "user/edited")

	hb, ok := r.current().Match("edited.example.test")
	if !ok {
		t.Fatal("edited binding must be live on the next access without a restart")
	}
	if hb.CredentialRef != "user/edited" {
		t.Errorf("credential_ref = %q, want user/edited", hb.CredentialRef)
	}
	// The additive edit must not drop the built-in defaults.
	if _, ok := r.current().Match("api.linear.app"); !ok {
		t.Error("built-in api.linear.app must survive the reload")
	}
}

// TestHostBindingsReloader_MalformedReloadKeepsPreviousTable proves a broken edit
// (the same load-time validation as #1873) does NOT half-apply or degrade to an
// empty passthrough table: the previously-loaded bindings keep serving, and once
// the file is corrected the reload succeeds on a later access.
func TestHostBindingsReloader_MalformedReloadKeepsPreviousTable(t *testing.T) {
	descPath := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
	writeUserDescriptor(t, descPath, "kept.example.test", "user/kept")

	r := primeHostBindingsReloader(t, descPath, nil)
	if _, ok := r.current().Match("kept.example.test"); !ok {
		t.Fatal("initial valid binding must be present")
	}

	// Corrupt the file with content of a clearly different length so the size
	// component of the stat signature changes even on coarse-mtime filesystems.
	if err := os.WriteFile(descPath, []byte("::: this is not valid descriptor yaml :::\n"), 0o600); err != nil {
		t.Fatalf("write malformed descriptor: %v", err)
	}

	// The malformed reload is rejected; the previous table keeps serving.
	if _, ok := r.current().Match("kept.example.test"); !ok {
		t.Fatal("previous binding must survive a malformed reload (no half-apply, no empty table)")
	}
	if _, ok := r.current().Match("api.linear.app"); !ok {
		t.Error("built-in defaults must survive a malformed reload")
	}

	// Correcting the file lets a later access reload cleanly (the failed reload
	// did not advance the stat signature, so the retry is not blocked). Replace
	// the malformed file outright rather than Upsert-merging onto it.
	if err := os.Remove(descPath); err != nil {
		t.Fatalf("remove malformed descriptor: %v", err)
	}
	writeUserDescriptor(t, descPath, "fixed.example.test", "user/fixed")
	if _, ok := r.current().Match("fixed.example.test"); !ok {
		t.Fatal("a corrected descriptor must reload on the next access")
	}
}

// TestHostBindingsReloader_ReloadReusesCachedUnitLayer proves a reload re-reads
// only the user file and never re-resolves the image unit layer: daemonUnitLayers
// is called exactly once (at construction), and the cached sealing entry remains
// resolvable across reloads driven by user-file edits.
func TestHostBindingsReloader_ReloadReusesCachedUnitLayer(t *testing.T) {
	calls := 0
	sealing := []proxybinding.Entry{{
		Host:          "sealed.example.test",
		CredentialRef: "user/sealed",
		Scheme:        binding.SchemeBearer,
	}}
	swapDaemonUnitLayers(t, func(context.Context) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
		calls++
		return nil, sealing, nil
	})

	r, err := newHostBindingsReloader(context.Background(), discardLogger())
	if err != nil {
		t.Fatalf("newHostBindingsReloader: %v", err)
	}
	if calls != 1 {
		t.Fatalf("daemonUnitLayers called %d times at construction, want 1", calls)
	}

	// Repoint at a controllable user file and reset the stat signature so the
	// next access observes the new file and reloads.
	descPath := filepath.Join(t.TempDir(), "binding-descriptors.yaml")
	r.userPath = descPath
	r.sig = hostBindingsStatSig{}

	writeUserDescriptor(t, descPath, "userland.example.test", "user/userland")
	if _, ok := r.current().Match("userland.example.test"); !ok {
		t.Fatal("user-file binding must reload")
	}
	// A second edit forces another reload.
	writeUserDescriptor(t, descPath, "userland2.example.test", "user/userland2")
	if _, ok := r.current().Match("userland2.example.test"); !ok {
		t.Fatal("second user-file edit must reload")
	}

	if calls != 1 {
		t.Errorf("daemonUnitLayers called %d times total, want 1 (reload must not re-resolve the image)", calls)
	}
	// The cached image unit layer is still applied after the user-file reloads.
	if _, ok := r.current().Match("sealed.example.test"); !ok {
		t.Error("cached unit-layer binding must remain resolvable across reloads")
	}
}
