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

## Database tests (testcontainers)

Packages under `internal/store/postgres/` are tested against a real PostgreSQL instance using
[testcontainers-go](https://golang.testcontainers.org/). Each test spins up a throwaway
Postgres container, applies schema migrations, and tears down automatically.

**Requirements:** Docker must be running.

These tests use the `//go:build integration` build tag and run in the integration test phase:

```sh
go test ./store/postgres/ -v                        # skips database tests (no build tag)
go test -tags=integration ./store/postgres/ -v       # runs database tests (requires Docker)
task test:integration                                # runs all integration tests including database
```

The `internal/store/pgtest` package provides the test helper:

```go
//go:build integration

func TestMyStore(t *testing.T) {
    db := pgtest.New(t, pgtest.EscrowIndexSchema) // starts container, migrates, returns *postgres.DB
    store := postgres.NewEscrowIndexStore(db)
    // ...
}
```

Add new schema constants to `pgtest` as more stores gain integration tests.

## Running locally with Docker Compose

For integration tests, start the full stack first:

```sh
task up
```

Then run the integration tests against the running server.
