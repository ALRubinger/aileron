// Package postgres implements store interfaces backed by PostgreSQL.
//
// All stores share a *pgxpool.Pool connection pool, passed via NewDB.
// Queries use straightforward SQL — no ORM — to keep the data layer
// transparent and easy to reason about.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps a pgx connection pool and provides store constructors.
type DB struct {
	Pool *pgxpool.Pool
}

// NewDB creates a connection pool from the given DSN and returns a DB.
// The caller is responsible for calling Close when done.
func NewDB(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// Close shuts down the connection pool.
func (db *DB) Close() {
	db.Pool.Close()
}
