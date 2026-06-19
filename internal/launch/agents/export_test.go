package agents

import (
	"context"
	"time"
)

// This file exports unexported Codex device-auth internals to the
// external agents_test package. It is a `_test.go` file, so the shims
// never ship in the production binary — they exist only to let the
// black-box tests drive the poll loop deterministically (no real sleeps)
// and unit-test the id_token claim parser without weakening the
// production API surface.

// SetCodexDeviceSleepForTest swaps the poll loop's sleep seam and returns
// a restore func. Tests pass a fn that returns immediately (and records
// the requested duration) so the device-auth poll loop runs without
// sleeping real seconds.
func SetCodexDeviceSleepForTest(fn func(context.Context, time.Duration) error) (restore func()) {
	prev := codexDeviceSleep
	codexDeviceSleep = fn
	return func() { codexDeviceSleep = prev }
}

// CodexAccountIDFromIDTokenForTest exposes codexAccountIDFromIDToken for
// black-box unit tests of the JWT claim extraction.
func CodexAccountIDFromIDTokenForTest(idToken string) (string, error) {
	return codexAccountIDFromIDToken(idToken)
}

// CodexDeviceSleepForTest invokes the PRODUCTION sleep seam (not a test
// override) so the real timer/context-cancellation behavior is exercised
// directly. Tests that drive the poll loop swap the seam out via
// SetCodexDeviceSleepForTest; this shim lets a separate test cover the
// default implementation itself.
func CodexDeviceSleepForTest(ctx context.Context, d time.Duration) error {
	return codexDeviceSleep(ctx, d)
}
