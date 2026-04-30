package sandbox

import (
	"testing"
	"time"
)

func TestLimits_Resolve_DefaultsFromADR(t *testing.T) {
	// ADR-0005 §"Resource limits": memory 64 MiB default, wall time 30 s
	// default. The zero-value Limits resolves to those defaults.
	got := Limits{}.Resolve()
	if got.MemoryBytes != 64*1024*1024 {
		t.Errorf("default memory = %d bytes, want 64 MiB", got.MemoryBytes)
	}
	if got.WallTime != 30*time.Second {
		t.Errorf("default wall time = %v, want 30s", got.WallTime)
	}
}

func TestLimits_Resolve_HonorsCallerNonZero(t *testing.T) {
	// Per-call overrides take precedence over defaults. The action
	// manifest is the v1 source of overrides (deferred field; the
	// runtime accepts overrides through the Call struct today).
	got := Limits{MemoryBytes: 128 * 1024 * 1024, WallTime: 90 * time.Second}.Resolve()
	if got.MemoryBytes != 128*1024*1024 {
		t.Errorf("memory = %d bytes, want 128 MiB", got.MemoryBytes)
	}
	if got.WallTime != 90*time.Second {
		t.Errorf("wall time = %v, want 90s", got.WallTime)
	}
}

func TestLimits_Resolve_ClampsAboveCeilings(t *testing.T) {
	// ADR-0005 hard ceilings: 1 GiB memory, 5 minutes wall time.
	// Requests above the ceiling are clamped, not rejected — the
	// runtime is forgiving on overrides but unforgiving on the cap.
	got := Limits{
		MemoryBytes: 4 * 1024 * 1024 * 1024, // 4 GiB
		WallTime:    24 * time.Hour,
	}.Resolve()
	if got.MemoryBytes != 1024*1024*1024 {
		t.Errorf("memory clamp = %d bytes, want 1 GiB", got.MemoryBytes)
	}
	if got.WallTime != 5*time.Minute {
		t.Errorf("wall time clamp = %v, want 5m", got.WallTime)
	}
}

func TestLimits_MemoryPages_RoundsUp(t *testing.T) {
	// WASM pages are 64 KiB. 64 MiB / 64 KiB = 1024 pages exactly;
	// anything not a page multiple should round up so the limit is
	// not below what the caller asked for.
	cases := []struct {
		bytes uint64
		pages uint32
	}{
		{0, 1024},                  // zero falls back to default → 1024 pages
		{64 * 1024, 1},             // exactly one page
		{64*1024 + 1, 2},           // round up
		{64 * 1024 * 1024, 1024},   // 64 MiB
	}
	for _, tc := range cases {
		got := Limits{MemoryBytes: tc.bytes}.MemoryPages()
		if got != tc.pages {
			t.Errorf("MemoryPages(%d bytes) = %d, want %d", tc.bytes, got, tc.pages)
		}
	}
}

func TestLimits_String_IncludesPagesAndWall(t *testing.T) {
	got := Limits{MemoryBytes: 64 * 1024 * 1024, WallTime: 30 * time.Second}.String()
	want := "memory=67108864 bytes (1024 pages), wall=30s"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
