// Package postgres is an optional pgx database/sql connection helper for Trest jobs.
// It does not apply migrations or start workers.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Open creates and checks a bounded connection pool. Connection strings are never
// returned in error messages. Close the returned DB after stopping all workers.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("postgres: invalid connection configuration")
	}
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, errors.New("postgres: database connection check failed")
	}
	return db, nil
}
