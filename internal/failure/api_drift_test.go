package failure_test

import (
	"sort"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/failure"
)

// The closed taxonomy is enforced in two places: this package's
// FailureClass / Boundary consts, and the generated OpenAPI enums.
// They must agree — drift between them is the source of bugs the
// type system can't catch (a server emitting a class no client
// schema knows about, or vice versa).
//
// These tests fail loudly if anyone amends one set without the other.

func TestClassEnumMatchesAPI(t *testing.T) {
	wantSlice := failure.AllClasses()
	want := make([]string, 0, len(wantSlice))
	for _, c := range wantSlice {
		want = append(want, string(c))
	}
	got := []string{
		string(api.CapabilityDenied),
		string(api.BindingRequired),
		string(api.BindingFailed),
		string(api.ResourceLimitExceeded),
		string(api.ConnectorRuntimeError),
		string(api.NetworkError),
		string(api.ExternalApiError),
		string(api.HashMismatch),
		string(api.SignatureFailure),
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(want) != len(got) {
		t.Fatalf("class count drift: pkg=%d api=%d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("class drift: pkg=%q api=%q", want[i], got[i])
		}
	}
}

func TestBoundaryEnumMatchesAPI(t *testing.T) {
	wantSlice := failure.AllBoundaries()
	want := make([]string, 0, len(wantSlice))
	for _, b := range wantSlice {
		want = append(want, string(b))
	}
	got := []string{
		string(api.FailureBoundarySandbox),
		string(api.FailureBoundaryConnectorManifest),
		string(api.FailureBoundaryAction),
		string(api.FailureBoundaryRuntime),
		string(api.FailureBoundaryExternal),
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(want) != len(got) {
		t.Fatalf("boundary count drift: pkg=%d api=%d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("boundary drift: pkg=%q api=%q", want[i], got[i])
		}
	}
}
