package manifest

import _ "embed"

// embeddedSchema is a byte-identical copy of the normative Flight Plan
// manifest schema at docs/schema/flight-plan-manifest.schema.json.
//
// The schema is the source of truth, but it lives outside this Go module
// (the internal module cannot go:embed across the module boundary into
// docs/). So the parser embeds a copy and a drift-guard test
// (schema_drift_test.go) asserts byte-equality with the doc copy. This
// mirrors the drift-guard discipline already used by
// internal/flightplan/spec.
//
//go:embed flight-plan-manifest.schema.json
var embeddedSchema []byte

// EmbeddedSchema returns the embedded Flight Plan manifest schema bytes.
// Exposed so the drift-guard test can compare against the doc copy.
func EmbeddedSchema() []byte { return embeddedSchema }
