package action

import "testing"

// TestResolveFieldPath_PlainObjectWalk verifies the dotted-path
// resolver walks nested objects per ADR-0016's contract: a path of
// "a.b.c" against `{"a": {"b": {"c": "x"}}}` yields "x". Without this,
// every preview render path would fail and the approval prompt would
// always show "n/a".
func TestResolveFieldPath_PlainObjectWalk(t *testing.T) {
	resp := map[string]any{
		"a": map[string]any{
			"b": map[string]any{
				"c": "x",
			},
		},
	}
	got, ok := ResolveFieldPath(resp, "a.b.c")
	if !ok {
		t.Fatalf("ResolveFieldPath ok = false, want true")
	}
	if got != "x" {
		t.Errorf("ResolveFieldPath = %q, want %q", got, "x")
	}
}

// TestResolveFieldPath_HeaderArrayShorthand covers the Gmail-style
// shorthand from ADR-0016: a path segment landing on an array of
// {name, value} entries is treated as a header lookup. This is the
// motivating shape for the ADR; the path `message.payload.headers.Subject`
// must resolve to the value of the entry whose name equals "Subject".
func TestResolveFieldPath_HeaderArrayShorthand(t *testing.T) {
	resp := map[string]any{
		"message": map[string]any{
			"payload": map[string]any{
				"headers": []any{
					map[string]any{"name": "From", "value": "alice@example.com"},
					map[string]any{"name": "Subject", "value": "Weekly recap"},
					map[string]any{"name": "To", "value": "bob@example.com"},
				},
			},
			"snippet": "Here's the recap from this week's standup",
		},
	}
	cases := map[string]string{
		"message.payload.headers.Subject": "Weekly recap",
		"message.payload.headers.To":      "bob@example.com",
		"message.payload.headers.From":    "alice@example.com",
		"message.snippet":                 "Here's the recap from this week's standup",
	}
	for path, want := range cases {
		got, ok := ResolveFieldPath(resp, path)
		if !ok {
			t.Errorf("ResolveFieldPath(%q) ok = false, want true", path)
			continue
		}
		if got != want {
			t.Errorf("ResolveFieldPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestResolveFieldPath_MissingPathReturnsNotFound asserts the
// per-field "n/a" surface in ADR-0016. A path whose segment does not
// exist (or whose header lookup misses) returns ok=false so the
// caller can render "n/a" rather than silently omitting the field.
func TestResolveFieldPath_MissingPathReturnsNotFound(t *testing.T) {
	resp := map[string]any{
		"message": map[string]any{
			"payload": map[string]any{
				"headers": []any{
					map[string]any{"name": "From", "value": "alice@example.com"},
				},
			},
		},
	}
	cases := []string{
		"message.payload.headers.Subject", // header miss
		"message.body",                    // object miss
		"message.payload.headers.From.x",  // walk past leaf
		"",                                // empty path
	}
	for _, path := range cases {
		_, ok := ResolveFieldPath(resp, path)
		if ok {
			t.Errorf("ResolveFieldPath(%q) ok = true, want false (path should miss)", path)
		}
	}
}

// TestResolveFieldPath_NumericIndexFallback covers arrays of plain
// (non-header) values. ADR-0016's shorthand is for {name, value}
// arrays specifically; other arrays accept a numeric index segment so
// authors can target the first attachment, third recipient, etc.
func TestResolveFieldPath_NumericIndexFallback(t *testing.T) {
	resp := map[string]any{
		"recipients": []any{"alice", "bob", "carol"},
	}
	got, ok := ResolveFieldPath(resp, "recipients.1")
	if !ok {
		t.Fatalf("ResolveFieldPath ok = false, want true")
	}
	if got != "bob" {
		t.Errorf("ResolveFieldPath = %q, want %q", got, "bob")
	}
	// Out-of-bounds index is a miss, not a panic.
	if _, ok := ResolveFieldPath(resp, "recipients.99"); ok {
		t.Errorf("ResolveFieldPath(out-of-bounds index) ok = true, want false")
	}
}

// TestResolveFieldPath_RendersNonStringLeaves verifies the resolver
// formats numbers, booleans, and other non-string leaves via %v so an
// author who targets a JSON number gets a sensible string in the
// rendered prompt rather than an empty value.
func TestResolveFieldPath_RendersNonStringLeaves(t *testing.T) {
	resp := map[string]any{
		"count":   float64(42),
		"enabled": true,
		"label":   "x",
	}
	cases := map[string]string{
		"count":   "42",
		"enabled": "true",
		"label":   "x",
	}
	for path, want := range cases {
		got, ok := ResolveFieldPath(resp, path)
		if !ok {
			t.Errorf("ResolveFieldPath(%q) ok = false, want true", path)
			continue
		}
		if got != want {
			t.Errorf("ResolveFieldPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestInterpolatePreviewArgs_SubstitutesArgsRefs verifies the preview
// directive's args template (with `${args.X}` interpolation) shares
// the same substitution semantics as [[execute]] step inputs. Without
// this, a preview's args would forward the literal `${args.draft_id}`
// to the connector and the upstream API would return an error.
func TestInterpolatePreviewArgs_SubstitutesArgsRefs(t *testing.T) {
	template := map[string]any{
		"id":    "${args.draft_id}",
		"trace": "draft-${args.draft_id}-preview",
	}
	args := map[string]any{"draft_id": "r-12345"}
	got := InterpolatePreviewArgs(template, args)
	if got["id"] != "r-12345" {
		t.Errorf("got[id] = %v, want %q", got["id"], "r-12345")
	}
	if got["trace"] != "draft-r-12345-preview" {
		t.Errorf("got[trace] = %v, want %q", got["trace"], "draft-r-12345-preview")
	}
}
