package sandbox

import (
	"fmt"
	"time"
)

// ADR-0005 §"Resource limits are enforced and bounded" defaults and
// hard ceilings. The connector manifest may request a higher cap, up to
// the ceiling; the action manifest may override per-call wall time, up
// to the ceiling. Field-level overrides on the action manifest are not
// part of the v1 schema and are deferred (see plan).
const (
	// DefaultMemoryBytes is the default per-instance memory cap.
	DefaultMemoryBytes uint64 = 64 * 1024 * 1024

	// MaxMemoryBytes is the hard ceiling — manifest requests above this
	// are clamped.
	MaxMemoryBytes uint64 = 1024 * 1024 * 1024

	// DefaultWallTime is the default per-call wall time.
	DefaultWallTime = 30 * time.Second

	// MaxWallTime is the hard ceiling for per-call overrides.
	MaxWallTime = 5 * time.Minute

	// wasmPageBytes is the WebAssembly page size — the unit Wazero uses
	// for memory limits (per the WASM spec).
	wasmPageBytes = 64 * 1024
)

// Resolve takes the caller-supplied limits, the connector manifest's
// declared overrides (if any), and produces the effective per-call
// limits. Zero values fall back to ADR defaults; values above the hard
// ceilings are clamped.
//
// Precedence (highest wins):
//  1. Caller's explicit non-zero limit (e.g. action manifest override).
//  2. Connector manifest's declared cap.
//  3. ADR-0005 default.
func (l Limits) Resolve() Limits {
	out := Limits{
		MemoryBytes: DefaultMemoryBytes,
		WallTime:    DefaultWallTime,
	}
	if l.MemoryBytes != 0 {
		out.MemoryBytes = l.MemoryBytes
	}
	if l.WallTime != 0 {
		out.WallTime = l.WallTime
	}
	if out.MemoryBytes > MaxMemoryBytes {
		out.MemoryBytes = MaxMemoryBytes
	}
	if out.WallTime > MaxWallTime {
		out.WallTime = MaxWallTime
	}
	return out
}

// MemoryPages converts MemoryBytes to WebAssembly memory pages (64 KiB
// each), rounding *up* so the cap is never less than what the caller
// requested. Wazero's memory-limit API works in pages, not bytes.
func (l Limits) MemoryPages() uint32 {
	bytes := l.MemoryBytes
	if bytes == 0 {
		bytes = DefaultMemoryBytes
	}
	pages := (bytes + wasmPageBytes - 1) / wasmPageBytes
	return uint32(pages)
}

// String returns a one-line human-readable summary, used in error
// messages and structured logging.
func (l Limits) String() string {
	return fmt.Sprintf("memory=%d bytes (%d pages), wall=%s",
		l.MemoryBytes, l.MemoryPages(), l.WallTime)
}
