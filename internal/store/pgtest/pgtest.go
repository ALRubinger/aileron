//go:build integration

// Package pgtest provides a throwaway PostgreSQL container for integration tests.
// It uses testcontainers-go to spin up a real Postgres instance, apply schema
// migrations, and return a connected *postgres.DB handle.
//
// Tests using this helper require Docker and should use the
// //go:build integration build tag.
package pgtest

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/store/postgres"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// New starts a throwaway PostgreSQL container, applies the given SQL migrations,
// and returns a connected *postgres.DB. The container is torn down automatically
// via t.Cleanup.
//
//	db := pgtest.New(t, pgtest.FullSchema)
//	store := postgres.NewEnterpriseStore(db)
func New(t *testing.T, migrations ...string) *postgres.DB {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
		tcpostgres.BasicWaitStrategies(),
		tcpostgres.WithSQLDriver("pgx"),
	)
	if err != nil {
		t.Fatalf("pgtest: start container: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("pgtest: terminate container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("pgtest: connection string: %v", err)
	}

	db, err := postgres.NewDB(ctx, dsn)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, sql := range migrations {
		if _, err := db.Pool.Exec(ctx, sql); err != nil {
			t.Fatalf("pgtest: apply migration: %v", err)
		}
	}

	return db
}
