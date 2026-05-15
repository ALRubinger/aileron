package binding_test

import (
	"reflect"
	"testing"

	"github.com/ALRubinger/aileron/internal/binding"
)

// TestMissingScopes_NothingMissingReturnsNil: when every required
// scope is in the granted set, the function returns nil — the
// contract callers test with `len(out) > 0` to decide drift.
func TestMissingScopes_NothingMissingReturnsNil(t *testing.T) {
	got := binding.MissingScopes(
		[]string{"a", "b"},
		[]string{"a", "b", "c"},
	)
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// TestMissingScopes_ReturnsRequiredNotGranted: the function reports
// exactly the elements of `required` not present in `granted`, in
// the order they appear in `required`.
func TestMissingScopes_ReturnsRequiredNotGranted(t *testing.T) {
	got := binding.MissingScopes(
		[]string{"a", "b", "c", "d"},
		[]string{"a", "c"},
	)
	want := []string{"b", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestMissingScopes_ExtraGrantedIsNotDrift: the diff is strict-superset
// on `required`. A binding holding scopes the manifest no longer
// demands is not stale — that direction is benign.
func TestMissingScopes_ExtraGrantedIsNotDrift(t *testing.T) {
	got := binding.MissingScopes(
		[]string{"a"},
		[]string{"a", "b", "c"},
	)
	if got != nil {
		t.Errorf("dropped-scope direction reported drift: %v", got)
	}
}

// TestMissingScopes_EmptyRequiredReturnsNil: a manifest with no OAuth
// scopes never triggers drift, regardless of the binding's grant.
func TestMissingScopes_EmptyRequiredReturnsNil(t *testing.T) {
	got := binding.MissingScopes(nil, []string{"a"})
	if got != nil {
		t.Errorf("empty required reported drift: %v", got)
	}
	got = binding.MissingScopes([]string{}, []string{"a"})
	if got != nil {
		t.Errorf("empty required slice reported drift: %v", got)
	}
}

// TestMissingScopes_EmptyGrantedReturnsAllRequired: when the binding
// has no recorded grant (migration case), every required scope is
// reported as missing. The drift evaluator separately translates this
// into stale_reason=no_grant_record rather than scope_drift.
func TestMissingScopes_EmptyGrantedReturnsAllRequired(t *testing.T) {
	got := binding.MissingScopes(
		[]string{"a", "b"},
		nil,
	)
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
