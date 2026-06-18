// Package ci holds CI-configuration regression guards. It ships no
// production code; it exists so the workflow contract (every CI job
// declares a job-level timeout-minutes) is asserted as a normal Go
// unit test that auto-flows into the existing test shards.
package ci
