package app

import (
	"context"
	"errors"
	"testing"

	"github.com/ALRubinger/aileron/internal/auth/capture"
	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/proxybinding"
)

// swapDaemonUnitLayers substitutes the image-derived unit-layer seam for the
// duration of a test, restoring the production resolver afterward.
func swapDaemonUnitLayers(t *testing.T, fn func(context.Context) ([]capture.CaptureDescriptor, []proxybinding.Entry, error)) {
	t.Helper()
	orig := daemonUnitLayers
	daemonUnitLayers = fn
	t.Cleanup(func() { daemonUnitLayers = orig })
}

// TestAssembleHostBindings_NoUnitLayerEqualsDefaults proves the daemon binding
// table is unchanged from the embedded-defaults-only table when the image read
// contributes nothing (the clean no-op the production path takes when the
// image is absent locally or carries no label).
func TestAssembleHostBindings_NoUnitLayerEqualsDefaults(t *testing.T) {
	swapDaemonUnitLayers(t, func(context.Context) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
		return nil, nil, nil
	})

	table, err := assembleHostBindings(context.Background())
	if err != nil {
		t.Fatalf("assembleHostBindings: %v", err)
	}
	// The built-in Linear default must still be present.
	if _, ok := table.Match("api.linear.app"); !ok {
		t.Error("defaults-only table missing built-in api.linear.app")
	}
}

// TestAssembleHostBindings_UnitLayerIsAdditive proves a unit-derived sealing
// entry is additively merged into the binding table on top of the embedded
// defaults. The built-in Linear binding survives and the unit-derived host is
// resolvable.
func TestAssembleHostBindings_UnitLayerIsAdditive(t *testing.T) {
	swapDaemonUnitLayers(t, func(context.Context) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
		sealing := []proxybinding.Entry{{
			Host:          "github.com",
			CredentialRef: "user/github",
			Scheme:        binding.SchemeBasic,
			Username:      "x-access-token",
		}}
		return nil, sealing, nil
	})

	table, err := assembleHostBindings(context.Background())
	if err != nil {
		t.Fatalf("assembleHostBindings: %v", err)
	}
	if _, ok := table.Match("api.linear.app"); !ok {
		t.Error("built-in api.linear.app must remain after the additive unit layer")
	}
	hb, ok := table.Match("github.com")
	if !ok {
		t.Fatal("unit-derived github.com must be resolvable in the merged table")
	}
	if hb.CredentialRef != "user/github" {
		t.Errorf("github.com credential_ref = %q, want user/github", hb.CredentialRef)
	}
}

// TestAssembleHostBindings_MalformedUnitFailsLoudly proves a present-but-broken
// unit (a seam error) fails table construction loudly rather than degrading to
// a defaults-only or empty table.
func TestAssembleHostBindings_MalformedUnitFailsLoudly(t *testing.T) {
	wantErr := errors.New("unitloader: parse cli unit at element 0: boom")
	swapDaemonUnitLayers(t, func(context.Context) ([]capture.CaptureDescriptor, []proxybinding.Entry, error) {
		return nil, nil, wantErr
	})

	_, err := assembleHostBindings(context.Background())
	if err == nil {
		t.Fatal("assembleHostBindings = nil error, want the unit-layer error surfaced")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want it to wrap %v", err, wantErr)
	}
}
