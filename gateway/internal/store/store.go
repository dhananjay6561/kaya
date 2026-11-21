// Package store owns the gateway's connection to Postgres.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is a pool of Postgres connections.
type DB struct {
	pool *pgxpool.Pool
}

// Open builds a connection pool for url. It does not connect until the first query.
func Open(url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	cfg.ConnConfig.RuntimeParams["application_name"] = "kaya-gateway"

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("create db pool: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Check runs a trivial query to confirm the database answers.
func (db *DB) Check(ctx context.Context) error {
	var one int
	if err := db.pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("query db: %w", err)
	}
	return nil
}

// Close waits for queries in flight and closes every connection.
func (db *DB) Close() {
	db.pool.Close()
}
