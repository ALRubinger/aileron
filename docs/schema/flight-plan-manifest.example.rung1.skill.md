---
name: nightly-log-rollup
description: Read a log window from a logging API and roll it up into a daily summary artifact.
license: Apache-2.0
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:logs.query_window
        trustContract:
          credential:
            kind: aws-sigv4
            placement: signing
            identityLabel: log-reader@example.com
          hosts:
            - logs.example.com
          paths:
            - /v1/logs/query
          effect: read
          idempotency:
            safeToRetry: true
            idempotencyKey: false
          verification:
            method: GET
            path: /v1/health
          audit:
            fields:
              - connector-hash
              - action-manifest-version
              - credential-binding
              - identity-label
              - approved-input
              - approval-decision
              - network-target
              - operation-effect
              - request-summary
              - response-summary
              - result
            sink: audit/log-reads
    executionEnvironment:
      rung1Image:
        ref: registry.example.com/log-rollup-runner:2.3
  inputs:
    - name: window_hours
      type: number
      description: How many hours back the log window covers. A literal so one image serves operators with different window sizes.
      resolution:
        rule: literal
        default: 24
    - name: as_of
      type: timestamp
      description: The launch timestamp the window is anchored to. Resolved once at launch so every step sees one clock value.
      resolution:
        rule: dynamic
        value: now
  outputs:
    - name: rollup.csv
      mimeType: text/csv
      encoding: utf-8
      publish:
        target: file
        path: rollup.csv
  steps:
    - id: query_logs
      kind: action-call
      actionRef: aileron:logs.query_window
      args:
        window_hours: inputs.window_hours
        as_of: inputs.as_of
      outputs:
        - entries
    - id: render_rollup
      kind: transform
      bindings:
        entries: steps.query_logs.entries
      outputs:
        - csv
      materializesOutput: rollup.csv
---

# Nightly Log Rollup

This skill reads a recent log window and rolls it up into a compact daily CSV summary.

## What it does

The `aileron.steps` block wires this work as a deterministic step graph.

1. `query_logs` (action-call) reads the log window from the logging API.
2. `render_rollup` (transform) renders a compact CSV rollup of the entries and materializes it into the `rollup.csv` output.

Each step binds its inputs by name to a resolved input (`inputs.<name>`) or a prior step output (`steps.<stepId>.<output>`). A binding is a reference, never a value.

## Execution environment (rung 1)

This skill declares a rung-1 execution environment: it names a whole prebuilt image (`registry.example.com/log-rollup-runner:2.3`) that the operator owns. Freeze resolves that named image to an `image@sha256:` digest pin and records it in the `lock` section. There are no capability units to compose, so the resolved capability set is empty. The contrast with the rung-2 worked example is the boundary this rung-1 example exists to demonstrate: rung-1 pins a named image; rung-2 composes capability-unit Features onto the Aileron base and pins the built result.

## Inputs

- `window_hours`: how far back to look. Defaults to the last 24 hours.
- `as_of`: the moment the window is anchored to. Defaults to launch time.

## Outputs

- `rollup.csv`: the rendered rollup, one row per bucket.

## Notes

This skill is not yet a Flight Plan. It carries no `lock` section because it has not been frozen. Freeze resolves the rung-1 `rung1Image.ref` to an image digest, attaches the per-action trust contract above, and signs the result. After freeze the `aileron.lock` section is present and immutable for that version.
