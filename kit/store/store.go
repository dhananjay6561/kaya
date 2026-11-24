// Package store owns a Kaya service's connection to Postgres.
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

// Option changes how Open sets up the pool.
type Option func(*settings)

type settings struct {
	appName    string
	searchPath string
}

// WithAppName names the connections in pg_stat_activity, unless the URL already names them.
func WithAppName(name string) Option { return func(s *settings) { s.appName = name } }

// WithSearchPath sets the schemas unqualified names resolve in, unless the URL already sets it.
func WithSearchPath(path string) Option { return func(s *settings) { s.searchPath = path } }

// Open builds a connection pool for url. It does not connect until the first query.
func Open(dsn string, options ...Option) (*DB, error) {
	chosen := settings{appName: "kaya"}
	for _, option := range options {
		option(&chosen)
	}
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
	if !set["search_path"] && chosen.searchPath != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = chosen.searchPath
	}
	if !set["application_name"] {
		cfg.ConnConfig.RuntimeParams["application_name"] = chosen.appName
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

// QueryRow runs a query that returns at most one row; Scan reports pgx.ErrNoRows when there is none.
func (db *DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return db.pool.QueryRow(ctx, sql, args...)
}

// Exec runs a statement and reports how many rows it touched.
func (db *DB) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := db.pool.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// InTx runs fn in a transaction, committing when it returns nil and rolling back otherwise.
func (db *DB) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, db.pool, fn)
}

// Listen calls fn each time a notification arrives on channel, until ctx is done. It takes one
// connection out of the pool for this, names it "<app>-listen" in pg_stat_activity, and closes it
// when it stops or fails, so a subscribed connection never goes back into the pool. A lost
// connection is replaced after a second. fn is a wake-up, not a count: it also runs once each time
// listening (re)starts, in case something arrived while nobody was listening.
func (db *DB) Listen(ctx context.Context, channel string, fn func()) {
	for ctx.Err() == nil {
		_ = db.listenOnce(ctx, channel, fn)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// Hijack takes a connection out of the pool for good, for state that must live and die with one
// connection, such as a session lock or a LISTEN. The caller closes it.
func (db *DB) Hijack(ctx context.Context) (*pgx.Conn, error) {
	pooled, err := db.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return pooled.Hijack(), nil
}

func (db *DB) listenOnce(ctx context.Context, channel string, fn func()) error {
	conn, err := db.Hijack(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	name := conn.Config().RuntimeParams["application_name"] + "-listen"
	if _, err := conn.Exec(ctx, "SELECT set_config('application_name', $1, false)", name); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return err
	}
	fn()
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		fn()
	}
}

// SQL returns a database/sql handle that borrows connections from the same pool, for libraries that need one.
func (db *DB) SQL() *sql.DB {
	return stdlib.OpenDBFromPool(db.pool)
}

// Close waits for queries in flight and closes every connection.
func (db *DB) Close() {
	db.pool.Close()
}
