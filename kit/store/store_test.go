package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func testURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return url
}

func TestOpenKeepsSettingsFromTheURL(t *testing.T) {
	db, err := Open("postgres://127.0.0.1:1/kaya?pool_max_conns=3&connect_timeout=2&application_name=probe")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := db.pool.Config()
	if cfg.MaxConns != 3 || cfg.ConnConfig.ConnectTimeout != 2*time.Second ||
		cfg.ConnConfig.RuntimeParams["application_name"] != "probe" {
		t.Fatalf("URL settings were overridden: max=%d timeout=%s app=%q",
			cfg.MaxConns, cfg.ConnConfig.ConnectTimeout, cfg.ConnConfig.RuntimeParams["application_name"])
	}

	defaults, err := Open("postgres://127.0.0.1:1/kaya")
	if err != nil {
		t.Fatal(err)
	}
	defer defaults.Close()
	cfg = defaults.pool.Config()
	if cfg.MaxConns != 10 || cfg.ConnConfig.ConnectTimeout != 5*time.Second ||
		cfg.ConnConfig.RuntimeParams["application_name"] != "kaya" ||
		cfg.ConnConfig.RuntimeParams["search_path"] != "" {
		t.Fatalf("defaults not applied: max=%d timeout=%s", cfg.MaxConns, cfg.ConnConfig.ConnectTimeout)
	}
}

func TestOpenAppliesOptionsUnlessTheURLSetsThem(t *testing.T) {
	db, err := Open("postgres://127.0.0.1:1/kaya", WithAppName("kaya-core"), WithSearchPath("whatsmeow,public"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	params := db.pool.Config().ConnConfig.RuntimeParams
	if params["application_name"] != "kaya-core" || params["search_path"] != "whatsmeow,public" {
		t.Fatalf("options not applied: %v", params)
	}

	urlWins, err := Open("postgres://127.0.0.1:1/kaya?application_name=probe", WithAppName("kaya-core"))
	if err != nil {
		t.Fatal(err)
	}
	defer urlWins.Close()
	if got := urlWins.pool.Config().ConnConfig.RuntimeParams["application_name"]; got != "probe" {
		t.Fatalf("the URL should win, got %q", got)
	}
}

func TestOpenRejectsBadURL(t *testing.T) {
	if _, err := Open("postgres://%zz"); err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
}

func TestCheckAgainstRealPostgres(t *testing.T) {
	db, err := Open(testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}
}

func TestCheckFailsWhenUnreachable(t *testing.T) {
	db, err := Open("postgres://127.0.0.1:1/kaya")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.Check(ctx); err == nil {
		t.Fatal("expected an error for an unreachable db")
	}
}
