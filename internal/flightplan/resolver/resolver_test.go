package resolver

import (
	"context"
	"errors"
	"testing"
)

func action(name string, connectorFQNs ...string) Action {
	a := Action{Name: name}
	for _, fqn := range connectorFQNs {
		a.Requires.Connectors = append(a.Requires.Connectors, Connector{Name: fqn})
	}
	return a
}

func TestParseRef(t *testing.T) {
	tests := []struct {
		raw               string
		wantConn, wantAct string
		wantErr           bool
	}{
		{"aileron:metrics.query_series", "metrics", "query_series", false},
		{"aileron:slack.send.message", "slack", "send.message", false},
		{"metrics.query_series", "", "", true}, // missing prefix
		{"aileron:metrics", "", "", true},      // missing dot
		{"aileron:.query", "", "", true},       // empty connector
		{"aileron:metrics.", "", "", true},     // empty action
	}
	for _, tt := range tests {
		got, err := ParseRef(tt.raw)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseRef(%q) expected error, got %+v", tt.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRef(%q): %v", tt.raw, err)
			continue
		}
		if got.Connector != tt.wantConn || got.Action != tt.wantAct {
			t.Errorf("ParseRef(%q) = {%q, %q}, want {%q, %q}", tt.raw, got.Connector, got.Action, tt.wantConn, tt.wantAct)
		}
	}
}

func TestSatisfies(t *testing.T) {
	ref, err := ParseRef("aileron:metrics.query_series")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		a    Action
		want bool
	}{
		// Satisfiable: action name matches, connector FQN tail matches.
		{"exact", action("query_series", "github://aileron/metrics"), true},
		// Kebab/underscore normalization on the action handle.
		{"kebab action handle", action("query-series", "github://aileron/metrics"), true},
		// FQN-tail matching: bare connector segment matches the tail.
		{"bare connector fqn", action("query_series", "metrics"), true},
		// Wrong connector tail.
		{"wrong connector", action("query_series", "github://aileron/tracker"), false},
		// Right connector, wrong action.
		{"wrong action", action("create_issue", "github://aileron/metrics"), false},
		// No connectors declared.
		{"no connectors", action("query_series"), false},
		// Multiple connectors, one matches.
		{"one of many", action("query_series", "github://aileron/x", "github://aileron/metrics"), true},
	}
	for _, tt := range tests {
		if got := Satisfies(ref, tt.a); got != tt.want {
			t.Errorf("%s: Satisfies = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestNormalizationBoundary(t *testing.T) {
	// query_series in the ref must match query-series on the action and
	// vice versa.
	refUnderscore, _ := ParseRef("aileron:metrics.query_series")
	refKebab, _ := ParseRef("aileron:metrics.query-series")
	a := action("query-series", "github://aileron/metrics")
	if !Satisfies(refUnderscore, a) {
		t.Error("underscore ref should match kebab action")
	}
	if !Satisfies(refKebab, action("query_series", "github://aileron/metrics")) {
		t.Error("kebab ref should match underscore action")
	}
}

func TestResolve(t *testing.T) {
	actions := []Action{
		action("query_series", "github://aileron/metrics"),
		action("create_issue", "github://aileron/tracker"),
	}
	res := Resolve([]string{
		"aileron:metrics.query_series",  // satisfied
		"aileron:tracker.create_issue",  // satisfied
		"aileron:metrics.delete_series", // unsatisfied (no such action)
		"not-a-ref",                     // unsatisfied (malformed)
	}, actions)

	if len(res.Satisfied) != 2 {
		t.Errorf("Satisfied = %v, want 2", res.Satisfied)
	}
	if len(res.Unsatisfied) != 2 {
		t.Errorf("Unsatisfied = %v, want 2", res.Unsatisfied)
	}
	if res.AllSatisfied() {
		t.Error("AllSatisfied should be false with unsatisfied refs")
	}
}

func TestResolveEmptyActionList(t *testing.T) {
	res := Resolve([]string{"aileron:metrics.query_series"}, nil)
	if len(res.Satisfied) != 0 || len(res.Unsatisfied) != 1 {
		t.Errorf("empty action list: %+v, want all unsatisfied", res)
	}
}

func TestResolveAllSatisfied(t *testing.T) {
	res := Resolve(nil, nil)
	if !res.AllSatisfied() {
		t.Error("no refs should be vacuously all-satisfied")
	}
}

func TestResolveWithFetcher(t *testing.T) {
	fake := FetcherFunc(func(ctx context.Context) ([]Action, error) {
		return []Action{action("query_series", "github://aileron/metrics")}, nil
	})
	res, err := ResolveWith(context.Background(), fake, []string{"aileron:metrics.query_series"})
	if err != nil {
		t.Fatalf("ResolveWith: %v", err)
	}
	if !res.AllSatisfied() {
		t.Errorf("expected satisfied, got %+v", res)
	}
}

func TestResolveWithFetcherError(t *testing.T) {
	wantErr := errors.New("daemon down")
	fake := FetcherFunc(func(ctx context.Context) ([]Action, error) {
		return nil, wantErr
	})
	_, err := ResolveWith(context.Background(), fake, []string{"aileron:metrics.query_series"})
	if !errors.Is(err, wantErr) {
		t.Errorf("expected fetch error to propagate, got %v", err)
	}
}
