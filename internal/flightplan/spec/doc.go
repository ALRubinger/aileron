// Package spec holds the drift-guard test for the Flight Plan manifest
// specification artifacts that live under docs/.
//
// The normative artifacts are the JSON Schema at
// docs/schema/flight-plan-manifest.schema.json and the worked example at
// docs/schema/flight-plan-manifest.example.skill.md, both described by the
// prose specification at
// docs/src/content/docs/development/flight-plan-manifest-spec.md.
//
// The schema covers the requires block, the per-action trust contract, inputs,
// outputs, and the deterministic step graph (the no-LLM composition format).
// The step graph wires declared actions and transforms into a directed acyclic
// graph with a single marked llm-seam.
//
// This package ships no runtime parser or validator. The Flight Plan parser,
// validator, freeze, and runtime enforcement are out of scope here and tracked
// in #1508, #1509, and #1511. The only Go in this package is a test that keeps
// the schema and the worked example from drifting apart. Graph invariants a
// JSON Schema cannot express (action-ref membership, binding resolution,
// acyclicity) are asserted by the test as contract checks, not implemented as a
// runtime validator.
package spec
