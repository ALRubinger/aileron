package manifest

import (
	"strings"
	"testing"
)

// invalidCases enumerates the contract's stated failure modes for a
// present aileron block. Each document has the block present (so the parser
// must validate it) but violates exactly one required-field rule.
func TestParseInvalidBlocks(t *testing.T) {
	cases := map[string]string{
		"missing requires": `---
name: s
description: d
aileron:
  inputs: []
  outputs: []
---
body
`,
		"missing inputs": `---
name: s
description: d
aileron:
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract: {}
  outputs: []
---
body
`,
		"missing outputs": `---
name: s
description: d
aileron:
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract: {}
  inputs: []
---
body
`,
		"empty actions array": `---
name: s
description: d
aileron:
  requires:
    actions: []
  inputs: []
  outputs: []
---
body
`,
		"action missing ref": `---
name: s
description: d
aileron:
  requires:
    actions:
      - trustContract: {}
  inputs: []
  outputs: []
---
body
`,
		"action missing trustContract": `---
name: s
description: d
aileron:
  requires:
    actions:
      - ref: aileron:metrics.query_series
  inputs: []
  outputs: []
---
body
`,
		"ref malformed (no connector.action)": `---
name: s
description: d
aileron:
  requires:
    actions:
      - ref: not-a-valid-ref
        trustContract: {}
  inputs: []
  outputs: []
---
body
`,
	}

	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatalf("expected validation error for %q, got nil", name)
			}
			if !strings.Contains(err.Error(), "schema validation") {
				t.Errorf("error should cite schema validation, got: %v", err)
			}
		})
	}
}

// TestStrippedTopLevelStillValid confirms the schema does not reject a
// document with the aileron block absent. This is the lossless-if-stripped
// guarantee at the parser boundary: an instruction-only skill is valid.
func TestStrippedTopLevelStillValid(t *testing.T) {
	if err := validateFrontmatter([]byte("name: s\ndescription: d\n")); err != nil {
		t.Fatalf("a stripped manifest must validate, got: %v", err)
	}
}
