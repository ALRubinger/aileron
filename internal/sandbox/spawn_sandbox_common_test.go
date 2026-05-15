package sandbox

import (
	"errors"
	"testing"
)

func TestApplyUnsupportedSandbox_NoOpWhenNoParamsDeclared(t *testing.T) {
	// Contract (ADR-0014 legacy compat): zero-value SpawnLimits
	// gets the no-op return so pre-sandbox callers and tests run
	// unchanged on platforms without a real sandbox.
	hooks, err := applyUnsupportedSandbox(SpawnLimits{})
	if err != nil {
		t.Errorf("expected nil error with empty limits, got %v", err)
	}
	if hooks.PostStart != nil || hooks.Cleanup != nil {
		t.Errorf("expected zero-value hooks, got %+v", hooks)
	}
}

func TestApplyUnsupportedSandbox_RefusesWhenSandboxRequested(t *testing.T) {
	// Contract (ADR-0014 graceful unavailability): when the
	// manifest declares any sandbox parameter on an unsupported
	// platform, return the wrapped ErrSpawnUnavailable so the
	// host function emits `spawn_sandbox_unavailable`.
	cases := []SpawnLimits{
		{FSRead: []string{"~/code/"}},
		{FSWrite: []string{"~/.cache/aileron/"}},
		{ProxyAddr: "127.0.0.1:54321"},
	}
	for _, lim := range cases {
		hooks, err := applyUnsupportedSandbox(lim)
		if !errors.Is(err, ErrSpawnUnavailable) {
			t.Errorf("limits=%+v: expected wrapped ErrSpawnUnavailable, got %v", lim, err)
		}
		if hooks.PostStart != nil || hooks.Cleanup != nil {
			t.Errorf("limits=%+v: unavailable path should not return hooks, got %+v", lim, hooks)
		}
	}
}
