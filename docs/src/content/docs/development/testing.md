---
title: "Testing"
description: "Run unit and integration tests"
---

## Unit tests

```sh
task test:go
```

Runs all Go package tests with race detection and coverage.

## Integration tests

```sh
task test:integration
```

Requires a running server. Integration tests validate API behavior against the OpenAPI spec, ensuring the implementation matches the contract.

## Running locally with Docker Compose

For integration tests, start the full stack first:

```sh
task up
```

Then run the integration tests against the running server.
