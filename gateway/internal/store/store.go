// Package store owns the gateway's connection to Postgres.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// DB is a pool of Postgres connections.
type DB struct {
	pool *pgxpool.Pool
	dsn  string
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
	if !set["search_path"] {
		cfg.ConnConfig.RuntimeParams["search_path"] = "whatsmeow,public"
	}
	if !set["application_name"] {
		cfg.ConnConfig.RuntimeParams["application_name"] = "kaya-gateway"
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("create db pool: %w", err)
	}
	return &DB{pool: pool, dsn: dsn}, nil
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

// URL returns the connection string the pool was opened with.
func (db *DB) URL() string { return db.dsn }

// Query runs a query that returns one row and scans it into dest.
func (db *DB) Query(ctx context.Context, sql string, args []any, dest ...any) error {
	return db.pool.QueryRow(ctx, sql, args...).Scan(dest...)
}

// InTx runs fn in a transaction, committing when it returns nil and rolling back otherwise.
func (db *DB) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, db.pool, fn)
}

// SQL returns a database/sql handle that borrows connections from the same pool, for libraries that need one.
func (db *DB) SQL() *sql.DB {
	return stdlib.OpenDBFromPool(db.pool)
}

// Close waits for queries in flight and closes every connection.
func (db *DB) Close() {
	db.pool.Close()
}
