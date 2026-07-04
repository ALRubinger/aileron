---
name: composed-tools-boot
description: Minimal tools-declaring plan for the composed-image boot integration test. It declares one curated tool so freeze composes exactly one image, then renders a static HTML report through the deterministic html-render transform.
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
              - operation-effect
              - result
            sink: audit/metrics-reads
  environment:
    tools:
      - aws-cli@2.x
  inputs:
    - name: report_template
      type: string
      description: The HTML template the html-render transform renders. A literal so the plan runs offline in CI with no live source, action call, or credential.
      resolution:
        rule: literal
        default: "<!doctype html><html><body><h1>composed-tools boot ok</h1></body></html>"
  outputs:
    - name: report.html
      mimeType: text/html
      encoding: utf-8
      publish:
        target: file
        path: report.html
  steps:
    - id: render_report
      kind: transform
      transform: html-render
      bindings:
        template: inputs.report_template
      outputs:
        - report
      materializesOutput: report.html
---

# Composed Tools Boot (E2E fixture)

Minimal tools-declaring Flight Plan used by the composed-image boot integration test (`TestFlightPlanComposedToolsBootGuard`).

It declares one curated tool (`aws-cli@2.x`) so freeze composes exactly one image and pins its bootable `LocalTag` plus attested `Digest`, then renders a static HTML report through the deterministic `html-render` transform.

It carries no `source` input, no `action-call` step, and no `llm-seam`, so the whole plan executes offline inside the booted container with no connector, credential, or network dependency. The single declared action satisfies the manifest's `requires.actions` minimum and is never invoked. The test's assertion is the boot itself: the composed image boots and the boot-time Id-vs-Digest guard resolves the just-built local tag to the attested digest.
