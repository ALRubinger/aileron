package nodedist

import (
	"errors"
	"testing"
)

func TestError_MessageAndUnwrap(t *testing.T) {
	cause := errors.New("root cause")
	e := &Error{Kind: ErrFetchFailed, Message: "fetch boom", Cause: cause}
	if got := e.Error(); got != "fetch_failed: fetch boom: root cause" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(e, cause) {
		t.Fatalf("Unwrap did not expose the cause")
	}

	noCause := &Error{Kind: ErrConfig, Message: "bad config"}
	if got := noCause.Error(); got != "config_error: bad config" {
		t.Fatalf("Error() without cause = %q", got)
	}
	if noCause.Unwrap() != nil {
		t.Fatalf("Unwrap of a causeless error was non-nil")
	}
}
