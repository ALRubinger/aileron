---
name: weekly-metrics-digest
description: Read a metrics window from a metrics API, summarize it, and file a tracking issue with the summary.
license: Apache-2.0
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.query_series
        trustContract:
          credential:
            kind: aws-sigv4
            placement: signing
            identityLabel: metrics-reader@example.com
          hosts:
            - api.example.com
          paths:
            - /v2/metrics/query
          effect: read
          idempotency:
            safeToRetry: true
            idempotencyKey: false
          redaction:
            - field: series[].labels.account_id
              rule: hash
          verification:
            method: GET
            path: /v2/health
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
            sink: audit/metrics-reads
      - ref: aileron:tracker.create_issue
        trustContract:
          credential:
            kind: oauth2
            placement: header
            identityLabel: digest-bot@example.com
          oauth:
            scopes:
              - issues:write
            endpoints:
              authorization: https://auth.example.com/oauth/authorize
              token: https://auth.example.com/oauth/token
            refresh: refresh-token
          hosts:
            - tracker.example.com
          paths:
            - /api/v1/issues
          effect: write
          idempotency:
            safeToRetry: false
            idempotencyKey: true
          redaction:
            - field: assignee.email
              rule: mask
          verification:
            method: GET
            path: /api/v1/me
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
            sink: audit/tracker-writes
  environment:
    tools:
      - aws-cli@2.x
  inputs:
    - name: window_days
      type: number
      description: How many days back the metrics window covers. A literal so one composition serves operators with different window sizes.
      resolution:
        rule: literal
        default: 7
    - name: as_of
      type: timestamp
      description: The launch timestamp the window is anchored to. Resolved once at launch so every step sees one clock value.
      resolution:
        rule: dynamic
        value: now
    - name: active_metric_set
      type: array
      description: The metric set to summarize, read live from the metrics API at launch.
      resolution:
        rule: source
        source:
          actionRef: aileron:metrics.query_series
          select: series[].name
  outputs:
    - name: digest.csv
      mimeType: text/csv
      encoding: utf-8
      publish:
        target: file
        path: digest.csv
    - name: filed_issue.json
      mimeType: application/json
      encoding: utf-8
      publish:
        target: file
        path: filed_issue.json
  steps:
    - id: query_metrics
      kind: action-call
      actionRef: aileron:metrics.query_series
      args:
        window_days: inputs.window_days
        as_of: inputs.as_of
        metric_set: inputs.active_metric_set
      outputs:
        - series
    - id: render_csv
      kind: transform
      bindings:
        series: steps.query_metrics.series
      outputs:
        - csv
      materializesOutput: digest.csv
    - id: summarize
      kind: llm-seam
      bindings:
        series: steps.query_metrics.series
        csv: steps.render_csv.csv
      outputs:
        - issue_body
    - id: file_issue
      kind: action-call
      actionRef: aileron:tracker.create_issue
      args:
        body: steps.summarize.issue_body
      outputs:
        - issue
      materializesOutput: filed_issue.json
---

# Weekly Metrics Digest

This skill reads a recent metrics window, writes a short digest, and files a tracking issue that links to it.

## What it does

The `aileron.steps` block wires this work as a deterministic step graph.

1. `query_metrics` (action-call) reads the active metric set over the configured window from the metrics API.
2. `render_csv` (transform) renders a compact CSV digest of the series and materializes it into the `digest.csv` output.
3. `summarize` (llm-seam) drafts the issue body from the series and the CSV. This is the single marked non-deterministic seam.
4. `file_issue` (action-call) files a tracking issue whose body is the summary and materializes the result into the `filed_issue.json` output.

Each step binds its inputs by name to a resolved input (`inputs.<name>`) or a prior step output (`steps.<stepId>.<output>`). A binding is a reference, never a value. The references form a directed acyclic graph the runtime executes in topological order.

## Execution environment

The `aileron.environment` block declares the container the plan runs in. Every run gets exactly one container. This skill declares one curated tool, `aws-cli@2.x`, which fits its SigV4-signed metrics read. Freeze composes the declared tools onto the Aileron runner base and pins the built image to a single digest recorded in the `lock` section. A skill that needs tooling outside the curated catalog can name a custom base with `environment.image` instead.

## Inputs

- `window_days`: how far back to look. Defaults to the last seven days.
- `as_of`: the moment the window is anchored to. Defaults to launch time.
- `active_metric_set`: which series to include. Read live at launch.

## Outputs

- `digest.csv`: the rendered digest, one row per series.
- `filed_issue.json`: the created tracking issue, including its URL.

## Notes

Binary outputs (for example a rendered chart image) are a deferred follow-up. v1 materializes text artifacts only. When binary outputs land, they will declare `encoding: base64` and ride the mount / run-and-collect boundary.

This skill is not yet a Flight Plan. It carries no `lock` section because it has not been frozen. Freeze (tracked in [#1509](https://github.com/ALRubinger/aileron/issues/1509)) resolves the declared `environment.tools` to one composed image digest, pins the resolved capability set, attaches the per-action trust contract above, and signs the result. After freeze the `aileron.lock` section is present and immutable for that version.
