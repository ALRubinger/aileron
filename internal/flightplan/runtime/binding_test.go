package runtime

import "testing"

func TestParseBinding_Input(t *testing.T) {
	b, err := ParseBinding("inputs.window_days")
	if err != nil {
		t.Fatalf("ParseBinding: %v", err)
	}
	if b.Kind != BindInput || b.Name != "window_days" {
		t.Errorf("got %+v", b)
	}
}

func TestParseBinding_Step(t *testing.T) {
	b, err := ParseBinding("steps.query_metrics.series")
	if err != nil {
		t.Fatalf("ParseBinding: %v", err)
	}
	if b.Kind != BindStep || b.StepID != "query_metrics" || b.Output != "series" {
		t.Errorf("got %+v", b)
	}
}

func TestParseBinding_Rejects(t *testing.T) {
	cases := []string{
		"",
		"window_days",     // no namespace
		"inputs.",         // empty name
		"inputs.a.b",      // input has no sub-output
		"steps.only",      // step without output
		"steps.a.b.c",     // too many segments
		"literal-value",   // a literal masquerading as a binding
		"inputs.bad name", // space outside the char class
		"outputs.x",       // not a binding namespace
		"steps..x",        // empty step id
		"${secret}",       // an interpolation attempt
	}
	for _, c := range cases {
		if _, err := ParseBinding(c); err == nil {
			t.Errorf("ParseBinding(%q) accepted an invalid binding", c)
		}
	}
}
