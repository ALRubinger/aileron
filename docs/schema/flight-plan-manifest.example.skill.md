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
    executionEnvironment:
      rung2CapabilityUnits:
        features:
          - ghcr.io/example/aileron-feature-metrics-cli:1
          - ghcr.io/example/aileron-feature-tracker-cli:1
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
---

# Weekly Metrics Digest

This skill reads a recent metrics window, writes a short digest, and files a tracking issue that links to it.

## What it does

1. Query the metrics API for the active metric set over the configured window.
2. Render a compact CSV digest of the series.
3. File a tracking issue whose body summarizes the digest.

## Inputs

- `window_days`: how far back to look. Defaults to the last seven days.
- `as_of`: the moment the window is anchored to. Defaults to launch time.
- `active_metric_set`: which series to include. Read live at launch.

## Outputs

- `digest.csv`: the rendered digest, one row per series.
- `filed_issue.json`: the created tracking issue, including its URL.

## Notes

Binary outputs (for example a rendered chart image) are a deferred follow-up. v1 materializes text artifacts only. When binary outputs land, they will declare `encoding: base64` and ride the mount / run-and-collect boundary.

This skill is not yet a Flight Plan. It carries no `lock` section because it has not been frozen. Freeze (tracked in [#1509](https://github.com/ALRubinger/aileron/issues/1509)) resolves the rung-2 capability-unit `features` to image digests, pins the resolved capability set, attaches the per-action trust contract above, and signs the result. After freeze the `aileron.lock` section is present and immutable for that version.
