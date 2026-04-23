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

Packages under `core/store/postgres/` are tested against a real PostgreSQL instance using
[testcontainers-go](https://golang.testcontainers.org/). Each test spins up a throwaway
Postgres container, applies schema migrations, and tears down automatically.

**Requirements:** Docker must be running.

These tests are skipped with `-short`:

```sh
go test -short ./...   # skips database tests
go test ./store/postgres/ -v  # runs database tests (requires Docker)
```

The `core/store/pgtest` package provides the test helper:

```go
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
