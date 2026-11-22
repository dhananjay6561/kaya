// Package store owns the gateway's connection to Postgres.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// DB is a pool of Postgres connections.
type DB struct {
	pool *pgxpool.Pool
}

// Open builds a connection pool for url. It does not connect until the first query.
func Open(dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	set := settingsIn(dsn)
	if !set["pool_max_conns"] {
		cfg.MaxConns = 10
	}
	if !set["pool_max_conn_idle_time"] {
		cfg.MaxConnIdleTime = 5 * time.Minute
	}
	if !set["pool_health_check_period"] {
		cfg.HealthCheckPeriod = 30 * time.Second
	}
	if !set["connect_timeout"] {
		cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	}
	if !set["application_name"] {
		cfg.ConnConfig.RuntimeParams["application_name"] = "kaya-gateway"
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("create db pool: %w", err)
	}
	return &DB{pool: pool}, nil
}

func settingsIn(raw string) map[string]bool {
	set := map[string]bool{}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		for key := range u.Query() {
			set[key] = true
		}
		return set
	}
	for _, field := range strings.Fields(raw) {
		if key, _, ok := strings.Cut(field, "="); ok {
			set[key] = true
		}
	}
	return set
}

// Check runs a trivial query to confirm the database answers.
func (db *DB) Check(ctx context.Context) error {
	var one int
	if err := db.pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("query db: %w", err)
	}
	return nil
}

// SQL returns a database/sql handle that borrows connections from the same pool, for libraries that need one.
func (db *DB) SQL() *sql.DB {
	return stdlib.OpenDBFromPool(db.pool)
}

// Close waits for queries in flight and closes every connection.
func (db *DB) Close() {
	db.pool.Close()
}
