// Package spec holds the drift-guard test for the Flight Plan manifest
// specification artifacts that live under docs/.
//
// The normative artifacts are the JSON Schema at
// docs/schema/flight-plan-manifest.schema.json and the worked example at
// docs/schema/flight-plan-manifest.example.skill.md, both described by the
// prose specification at
// docs/src/content/docs/development/flight-plan-manifest-spec.md.
//
// This package ships no runtime parser or validator. The Flight Plan parser,
// validator, freeze, and runtime enforcement are out of scope here and tracked
// in #1508, #1509, and #1511. The only Go in this package is a test that keeps
// the schema and the worked example from drifting apart.
package spec
